package rpc

import (
	"context"
	"fmt"
	"reflect"
	"time"

	core "github.com/libp2p/go-libp2p-core"
	"github.com/libp2p/go-libp2p-core/network"
	"github.com/libp2p/go-libp2p-core/protocol"

	"github.com/oasisprotocol/oasis-core/go/common"
	"github.com/oasisprotocol/oasis-core/go/common/cbor"
	"github.com/oasisprotocol/oasis-core/go/common/errors"
	"github.com/oasisprotocol/oasis-core/go/common/logging"
	"github.com/oasisprotocol/oasis-core/go/common/version"
	"github.com/oasisprotocol/oasis-core/go/common/workerpool"
)

const (
	RequestWriteDeadline = 5 * time.Second
)

// PeerFeedback is an interface for providing deferred peer feedback after an outcome is known.
type PeerFeedback interface {
	// RecordSuccess records a successful protocol interaction with the given peer.
	RecordSuccess()

	// RecordFailure records an unsuccessful protocol interaction with the given peer.
	RecordFailure()

	// RecordBadPeer records a malicious protocol interaction with the given peer.
	//
	// The peer will be ignored during peer selection.
	RecordBadPeer()
}

type peerFeedback struct {
	mgr     PeerManager
	peerID  core.PeerID
	latency time.Duration
}

func (pf *peerFeedback) RecordSuccess() {
	pf.mgr.RecordSuccess(pf.peerID, pf.latency)
}

func (pf *peerFeedback) RecordFailure() {
	pf.mgr.RecordFailure(pf.peerID, pf.latency)
}

func (pf *peerFeedback) RecordBadPeer() {
	pf.mgr.RecordBadPeer(pf.peerID)
}

type nopPeerFeedback struct{}

func (pf *nopPeerFeedback) RecordSuccess() {
}

func (pf *nopPeerFeedback) RecordFailure() {
}

func (pf *nopPeerFeedback) RecordBadPeer() {
}

// NewNopPeerFeedback creates a no-op peer feedback instance.
func NewNopPeerFeedback() PeerFeedback {
	return &nopPeerFeedback{}
}

// Client is an RPC client for a given protocol.
type Client interface {
	PeerManager

	// Call attempts to route the given RPC method call to one of the peers that supports the
	// protocol based on past experience with the peers.
	//
	// On success it returns a PeerFeedback instance that should be used by the caller to provide
	// deferred feedback on whether the peer is any good or not. This will help guide later choices
	// when routing calls.
	Call(
		ctx context.Context,
		method string,
		body, rsp interface{},
		maxPeerResponseTime time.Duration,
	) (PeerFeedback, error)

	// CallMulti routes the given RPC method call to multiple peers that support the protocol based
	// on past experience with the peers.
	//
	// It returns all successfully retrieved results and their corresponding PeerFeedback instances.
	CallMulti(
		ctx context.Context,
		method string,
		body, rspTyp interface{},
		maxPeerResponseTime time.Duration,
		maxParallelRequests uint,
	) ([]interface{}, []PeerFeedback, error)
}

type client struct {
	PeerManager

	host       core.Host
	protocolID protocol.ID
	runtimeID  common.Namespace

	logger *logging.Logger
}

func (c *client) Call(
	ctx context.Context,
	method string,
	body, rsp interface{},
	maxPeerResponseTime time.Duration,
) (PeerFeedback, error) {
	c.logger.Debug("call", "method", method)

	// Prepare the request.
	request := Request{
		Method: method,
		Body:   cbor.Marshal(body),
	}

	// Iterate through the prioritized list of peers and attempt to execute the request.
	for _, peer := range c.GetBestPeers() {
		c.logger.Debug("trying peer",
			"method", method,
			"peer_id", peer,
		)

		pf, err := c.call(ctx, peer, &request, rsp, maxPeerResponseTime)
		if err != nil {
			continue
		}
		return pf, nil
	}

	// No peers could be reached to service this request.
	c.logger.Debug("no peers could be reached to service request",
		"method", method,
	)

	return nil, fmt.Errorf("call failed on all peers")
}

func (c *client) CallMulti(
	ctx context.Context,
	method string,
	body, rspTyp interface{},
	maxPeerResponseTime time.Duration,
	maxParallelRequests uint,
) ([]interface{}, []PeerFeedback, error) {
	c.logger.Debug("call multiple", "method", method)

	// Prepare the request.
	request := Request{
		Method: method,
		Body:   cbor.Marshal(body),
	}

	// Create a worker pool.
	pool := workerpool.New("p2p/rpc")
	pool.Resize(maxParallelRequests)
	defer pool.Stop()

	// Requests results from peers.
	type result struct {
		rsp interface{}
		pf  PeerFeedback
		err error
	}
	var resultCh []chan *result
	for _, peer := range c.GetBestPeers() {
		ch := make(chan *result, 1)
		resultCh = append(resultCh, ch)

		pool.Submit(func() {
			rsp := reflect.New(reflect.TypeOf(rspTyp)).Interface()
			pf, err := c.call(ctx, peer, &request, rsp, maxPeerResponseTime)
			ch <- &result{rsp, pf, err}
			close(ch)
		})
	}

	// Gather results.
	var (
		rsps []interface{}
		pfs  []PeerFeedback
	)
	for _, ch := range resultCh {
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case result := <-ch:
			// Ignore failed results.
			if result.err != nil {
				continue
			}

			rsps = append(rsps, result.rsp)
			pfs = append(pfs, result.pf)
		}
	}
	return rsps, pfs, nil
}

func (c *client) call(
	ctx context.Context,
	peerID core.PeerID,
	request *Request,
	rsp interface{},
	maxPeerResponseTime time.Duration,
) (PeerFeedback, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	startTime := time.Now()

	err := c.sendRequestAndDecodeResponse(ctx, peerID, request, rsp, maxPeerResponseTime)
	if err != nil {
		c.logger.Debug("failed to call method",
			"err", err,
			"method", request.Method,
			"peer_id", peerID,
		)

		c.RecordFailure(peerID, time.Since(startTime))
		return nil, err
	}

	pf := &peerFeedback{
		mgr:     c.PeerManager,
		peerID:  peerID,
		latency: time.Since(startTime),
	}
	return pf, nil
}

func (c *client) sendRequestAndDecodeResponse(
	ctx context.Context,
	peerID core.PeerID,
	request *Request,
	rsp interface{},
	maxPeerResponseTime time.Duration,
) error {
	// Attempt to open stream to the given peer.
	stream, err := c.host.NewStream(
		network.WithNoDial(ctx, "should already have connection"),
		peerID,
		c.protocolID,
	)
	if err != nil {
		return fmt.Errorf("failed to open stream: %w", err)
	}
	defer stream.Close()

	codec := cbor.NewMessageCodec(stream, codecModuleName)

	// Send request.
	_ = stream.SetWriteDeadline(time.Now().Add(RequestWriteDeadline))
	if err = codec.Write(request); err != nil {
		c.logger.Debug("failed to send request",
			"err", err,
			"peer_id", peerID,
		)
		return fmt.Errorf("failed to send request: %w", err)
	}
	_ = stream.SetWriteDeadline(time.Time{})

	// Read response.
	// TODO: Add required minimum speed.
	var rawRsp Response
	_ = stream.SetReadDeadline(time.Now().Add(maxPeerResponseTime))
	if err = codec.Read(&rawRsp); err != nil {
		c.logger.Debug("failed to read response",
			"err", err,
			"peer_id", peerID,
		)
		return fmt.Errorf("failed to read response: %w", err)
	}
	_ = stream.SetWriteDeadline(time.Time{})

	// Decode response.
	if rawRsp.Error != nil {
		return errors.FromCode(rawRsp.Error.Module, rawRsp.Error.Code, rawRsp.Error.Message)
	}

	if rsp != nil {
		return cbor.Unmarshal(rawRsp.Ok, rsp)
	}
	return nil
}

// NewClient creates a new RPC client for the given protocol.
func NewClient(p2p P2P, runtimeID common.Namespace, protocolID string, version version.Version) Client {
	pid := NewRuntimeProtocolID(runtimeID, protocolID, version)

	return &client{
		PeerManager: NewPeerManager(p2p, pid),
		host:        p2p.GetHost(),
		protocolID:  pid,
		runtimeID:   runtimeID,
		logger: logging.GetLogger("worker/common/p2p/rpc/client").With(
			"protocol", protocolID,
			"runtime_id", runtimeID,
		),
	}
}
