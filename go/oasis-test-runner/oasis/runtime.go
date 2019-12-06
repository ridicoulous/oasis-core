package oasis

import (
	"os"
	"path/filepath"
	"strconv"

	"github.com/pkg/errors"

	"github.com/oasislabs/oasis-core/go/common/crypto/signature"
	"github.com/oasislabs/oasis-core/go/common/node"
	"github.com/oasislabs/oasis-core/go/common/sgx"
	"github.com/oasislabs/oasis-core/go/oasis-node/cmd/common"
	cmdRegRt "github.com/oasislabs/oasis-core/go/oasis-node/cmd/registry/runtime"
	"github.com/oasislabs/oasis-core/go/oasis-test-runner/env"
	registry "github.com/oasislabs/oasis-core/go/registry/api"
)

const rtDescriptorFile = "runtime_genesis.json"

// Runtime is an Oasis runtime.
type Runtime struct { // nolint: maligned
	dir *env.Dir

	id   signature.PublicKey
	kind registry.RuntimeKind

	binary      string
	teeHardware node.TEEHardware
	mrEnclave   *sgx.MrEnclave
	mrSigner    *sgx.MrSigner
}

// RuntimeCfg is the Oasis runtime provisioning configuration.
type RuntimeCfg struct { // nolint: maligned
	ID          signature.PublicKey
	Kind        registry.RuntimeKind
	Entity      *Entity
	Keymanager  *Runtime
	TEEHardware node.TEEHardware
	MrSigner    *sgx.MrSigner

	Binary       string
	GenesisState string

	Compute      registry.ComputeParameters
	Merge        registry.MergeParameters
	TxnScheduler registry.TxnSchedulerParameters
	Storage      registry.StorageParameters
}

// ID returns the runtime ID.
func (rt *Runtime) ID() signature.PublicKey {
	return rt.id
}

func (rt *Runtime) toGenesisArgs() []string {
	return []string{
		"--runtime", filepath.Join(rt.dir.String(), rtDescriptorFile),
	}
}

// NewRuntime provisions a new runtime and adds it to the network.
func (net *Network) NewRuntime(cfg *RuntimeCfg) (*Runtime, error) {
	rtDir, err := net.baseDir.NewSubDir("runtime-" + cfg.ID.String())
	if err != nil {
		net.logger.Error("failed to create runtime subdir",
			"err", err,
		)
		return nil, errors.Wrap(err, "oasis/runtime: failed to create runtime subdir")
	}

	args := []string{
		"registry", "runtime", "init_genesis",
		"--" + common.CfgDataDir, rtDir.String(),
		"--" + cmdRegRt.CfgID, cfg.ID.String(),
		"--" + cmdRegRt.CfgKind, cfg.Kind.String(),
	}
	if cfg.Kind == registry.KindCompute {
		args = append(args, []string{
			"--" + cmdRegRt.CfgComputeGroupSize, strconv.FormatUint(cfg.Compute.GroupSize, 10),
			"--" + cmdRegRt.CfgComputeGroupBackupSize, strconv.FormatUint(cfg.Compute.GroupBackupSize, 10),
			"--" + cmdRegRt.CfgComputeAllowedStragglers, strconv.FormatUint(cfg.Compute.AllowedStragglers, 10),
			"--" + cmdRegRt.CfgComputeRoundTimeout, cfg.Compute.RoundTimeout.String(),
			"--" + cmdRegRt.CfgMergeGroupSize, strconv.FormatUint(cfg.Merge.GroupSize, 10),
			"--" + cmdRegRt.CfgMergeGroupBackupSize, strconv.FormatUint(cfg.Merge.GroupBackupSize, 10),
			"--" + cmdRegRt.CfgMergeAllowedStragglers, strconv.FormatUint(cfg.Merge.AllowedStragglers, 10),
			"--" + cmdRegRt.CfgMergeRoundTimeout, cfg.Merge.RoundTimeout.String(),
			"--" + cmdRegRt.CfgTxnSchedulerGroupSize, strconv.FormatUint(cfg.TxnScheduler.GroupSize, 10),
			"--" + cmdRegRt.CfgTxnSchedulerMaxBatchSize, strconv.FormatUint(cfg.TxnScheduler.MaxBatchSize, 10),
			"--" + cmdRegRt.CfgTxnSchedulerMaxBatchSizeBytes, strconv.FormatUint(cfg.TxnScheduler.MaxBatchSizeBytes, 10),
			"--" + cmdRegRt.CfgTxnSchedulerAlgorithm, cfg.TxnScheduler.Algorithm,
			"--" + cmdRegRt.CfgTxnSchedulerBatchFlushTimeout, cfg.TxnScheduler.BatchFlushTimeout.String(),
			"--" + cmdRegRt.CfgStorageGroupSize, strconv.FormatUint(cfg.Storage.GroupSize, 10),
		}...)

		if cfg.GenesisState != "" {
			args = append(args, "--"+cmdRegRt.CfgGenesisState, cfg.GenesisState)
		}
	}
	var mrEnclave *sgx.MrEnclave
	if cfg.TEEHardware == node.TEEHardwareIntelSGX {
		if mrEnclave, err = deriveMrEnclave(cfg.Binary); err != nil {
			return nil, err
		}

		args = append(args, []string{
			"--" + cmdRegRt.CfgTEEHardware, cfg.TEEHardware.String(),
			"--" + cmdRegRt.CfgVersionEnclave, mrEnclave.String() + cfg.MrSigner.String(),
		}...)
	}
	if cfg.Keymanager != nil {
		args = append(args, []string{
			"--" + cmdRegRt.CfgKeyManager, cfg.Keymanager.id.String(),
		}...)
	}
	args = append(args, cfg.Entity.toGenesisArgs()...)

	w, err := rtDir.NewLogWriter("provision.log")
	if err != nil {
		return nil, err
	}
	defer w.Close()

	if err = net.runNodeBinary(w, args...); err != nil {
		net.logger.Error("failed to provision runtime",
			"err", err,
		)
		return nil, errors.Wrap(err, "oasis/runtime: failed to provision runtime")
	}

	rt := &Runtime{
		dir:         rtDir,
		id:          cfg.ID,
		kind:        cfg.Kind,
		binary:      cfg.Binary,
		teeHardware: cfg.TEEHardware,
		mrEnclave:   mrEnclave,
		mrSigner:    cfg.MrSigner,
	}
	net.runtimes = append(net.runtimes, rt)

	return rt, nil
}

func deriveMrEnclave(f string) (*sgx.MrEnclave, error) {
	r, err := os.Open(f)
	if err != nil {
		return nil, errors.Wrap(err, "oasis: failed to open enclave binary")
	}
	defer r.Close()

	var m sgx.MrEnclave
	if err = m.FromSgxs(r); err != nil {
		return nil, err
	}

	return &m, nil
}
