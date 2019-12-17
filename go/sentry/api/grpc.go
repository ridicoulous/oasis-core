package api

import (
	"context"

	"google.golang.org/grpc"

	cmnGrpc "github.com/oasislabs/oasis-core/go/common/grpc"
	"github.com/oasislabs/oasis-core/go/common/node"
)

var (
	// serviceName is the gRPC service name.
	serviceName = cmnGrpc.NewServiceName("Sentry")

	// methodGetConsensusAddresses is the name of the GetConsensusAddresses method.
	methodGetConsensusAddresses = serviceName.NewMethodName("GetConsensusAddresses")

	// serviceDesc is the gRPC service descriptor.
	serviceDesc = grpc.ServiceDesc{
		ServiceName: string(serviceName),
		HandlerType: (*Backend)(nil),
		Methods: []grpc.MethodDesc{
			{
				MethodName: methodGetConsensusAddresses.Short(),
				Handler:    handlerGetConsensusAddresses,
			},
		},
		Streams: []grpc.StreamDesc{},
	}
)

func handlerGetConsensusAddresses( // nolint: golint
	srv interface{},
	ctx context.Context,
	dec func(interface{}) error,
	interceptor grpc.UnaryServerInterceptor,
) (interface{}, error) {
	if interceptor == nil {
		return srv.(Backend).GetConsensusAddresses(ctx)
	}
	info := &grpc.UnaryServerInfo{
		Server:     srv,
		FullMethod: methodGetConsensusAddresses.Full(),
	}
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return srv.(Backend).GetConsensusAddresses(ctx)
	}
	return interceptor(ctx, nil, info, handler)
}

// RegisterService registers a new sentry service with the given gRPC server.
func RegisterService(server *grpc.Server, service Backend) {
	server.RegisterService(&serviceDesc, service)
}

type sentryClient struct {
	conn *grpc.ClientConn
}

func (c *sentryClient) GetConsensusAddresses(ctx context.Context) ([]node.ConsensusAddress, error) {
	var rsp []node.ConsensusAddress
	if err := c.conn.Invoke(ctx, methodGetConsensusAddresses.Full(), nil, &rsp); err != nil {
		return nil, err
	}
	return rsp, nil
}

// NewSentryClient creates a new gRPC sentry client service.
func NewSentryClient(c *grpc.ClientConn) Backend {
	return &sentryClient{c}
}
