package registry

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	flag "github.com/spf13/pflag"
	"github.com/spf13/viper"

	"github.com/oasisprotocol/oasis-core/go/common"
	"github.com/oasisprotocol/oasis-core/go/common/node"
	consensus "github.com/oasisprotocol/oasis-core/go/consensus/api"
	ias "github.com/oasisprotocol/oasis-core/go/ias/api"
	cmdFlags "github.com/oasisprotocol/oasis-core/go/oasis-node/cmd/common/flags"
	"github.com/oasisprotocol/oasis-core/go/runtime/bundle"
	"github.com/oasisprotocol/oasis-core/go/runtime/history"
	runtimeHost "github.com/oasisprotocol/oasis-core/go/runtime/host"
	hostMock "github.com/oasisprotocol/oasis-core/go/runtime/host/mock"
	hostProtocol "github.com/oasisprotocol/oasis-core/go/runtime/host/protocol"
	hostSandbox "github.com/oasisprotocol/oasis-core/go/runtime/host/sandbox"
	hostSgx "github.com/oasisprotocol/oasis-core/go/runtime/host/sgx"
)

const (
	// CfgRuntimeProvisioner configures the runtime provisioner.
	//
	// The same provisioner is used for all runtimes.
	CfgRuntimeProvisioner = "runtime.provisioner"
	// CfgRuntimePaths confgures the paths for supported runtimes.
	//
	// The value should be a vector of slices to the runtime bundles.
	CfgRuntimePaths = "runtime.paths"
	// CfgSandboxBinary configures the runtime sandbox binary location.
	CfgSandboxBinary = "runtime.sandbox.binary"
	// CfgRuntimeSGXLoader configures the runtime loader binary required for SGX runtimes.
	//
	// The same loader is used for all runtimes.
	CfgRuntimeSGXLoader = "runtime.sgx.loader"

	// CfgRuntimeConfig configures node-local runtime configuration.
	CfgRuntimeConfig = "runtime.config"

	// CfgHistoryPrunerStrategy configures the history pruner strategy.
	CfgHistoryPrunerStrategy = "runtime.history.pruner.strategy"
	// CfgHistoryPrunerInterval configures the history pruner interval.
	CfgHistoryPrunerInterval = "runtime.history.pruner.interval"
	// CfgHistoryPrunerKeepLastNum configures the number of last kept
	// rounds when using the "keep last" pruner strategy.
	CfgHistoryPrunerKeepLastNum = "runtime.history.pruner.num_kept"

	// CfgRuntimeMode configures how the runtime workers should behave on this node.
	CfgRuntimeMode = "runtime.mode"
)

// Flags has the configuration flags.
var Flags = flag.NewFlagSet("", flag.ContinueOnError)

const (
	// RuntimeProvisionerMock is the name of the mock runtime provisioner.
	//
	// Use of this provisioner is only allowed if DebugDontBlameOasis flag is set.
	RuntimeProvisionerMock = "mock"
	// RuntimeProvisionerUnconfined is the name of the unconfined runtime provisioner that executes
	// runtimes as regular processes without any sandboxing.
	//
	// Use of this provisioner is only allowed if DebugDontBlameOasis flag is set.
	RuntimeProvisionerUnconfined = "unconfined"
	// RuntimeProvisionerSandboxed is the name of the sandboxed runtime provisioner that executes
	// runtimes as regular processes in a Linux namespaces/cgroups/SECCOMP sandbox.
	RuntimeProvisionerSandboxed = "sandboxed"
)

// RuntimeMode defines the behavior of runtime workers on this node.
type RuntimeMode string

const (
	// RuntimeModeNone is the runtime mode where runtime support is disabled and only consensus
	// layer services are enabled.
	RuntimeModeNone RuntimeMode = "none"
	// RuntimeModeCompute is the runtime mode where the node participates as a compute and storage
	// node for all the configured runtimes.
	RuntimeModeCompute RuntimeMode = "compute"
	// RuntimeModeKeymanager is the runtime mode where the node participates as a keymanager node.
	RuntimeModeKeymanager RuntimeMode = "keymanager"
	// RuntimeModeClient is the runtime mode where the node does not register and is only a stateful
	// client for all the configured runtimes. Stateful means that it keeps all runtime state.
	RuntimeModeClient RuntimeMode = "client"
	// RuntimeModeClientStateless is the runtime mode where the node does not register and is only a
	// stateless client for all the configured runtimes. No state is kept locally and the node must
	// connect to remote nodes to perform any runtime queries.
	RuntimeModeClientStateless RuntimeMode = "client-stateless"
)

// UnmarshalText decodes a text marshaled runtime mode.
func (m *RuntimeMode) UnmarshalText(text []byte) error {
	switch string(text) {
	case string(RuntimeModeNone):
		*m = RuntimeModeNone
	case string(RuntimeModeCompute):
		*m = RuntimeModeCompute
	case string(RuntimeModeKeymanager):
		*m = RuntimeModeKeymanager
	case string(RuntimeModeClient):
		*m = RuntimeModeClient
	case string(RuntimeModeClientStateless):
		*m = RuntimeModeClientStateless
	default:
		return fmt.Errorf("invalid mode: %s", string(text))
	}
	return nil
}

// RuntimeConfig is the node runtime configuration.
type RuntimeConfig struct {
	// Mode is the runtime mode for this node.
	Mode RuntimeMode

	// Host contains configuration for the runtime host. It may be nil if no runtimes are to be
	// hosted by the current node.
	Host *RuntimeHostConfig

	// History configures the runtime history keeper.
	History history.Config
}

// Runtimes returns a list of configured runtimes.
func (cfg *RuntimeConfig) Runtimes() (runtimes []common.Namespace) {
	if cfg.Host == nil || cfg.Mode == RuntimeModeKeymanager {
		return
	}

	for id := range cfg.Host.Runtimes {
		runtimes = append(runtimes, id)
	}
	return
}

// RuntimeHostConfig is configuration for a node that hosts runtimes.
type RuntimeHostConfig struct {
	// Provisioners contains a set of supported runtime provisioners, based on TEE hardware.
	Provisioners map[node.TEEHardware]runtimeHost.Provisioner

	// Runtimes contains per-runtime provisioning configuration. Some fields may be omitted as they
	// are provided when the runtime is provisioned.
	Runtimes map[common.Namespace]*runtimeHost.Config
}

func newConfig(dataDir string, consensus consensus.Backend, ias ias.Endpoint) (*RuntimeConfig, error) { //nolint: gocyclo
	var cfg RuntimeConfig

	// Parse configured runtime mode.
	if err := cfg.Mode.UnmarshalText([]byte(viper.GetString(CfgRuntimeMode))); err != nil {
		return nil, fmt.Errorf("failed to parse mode: %w", err)
	}

	// Validate configured runtimes based on the runtime mode.
	switch cfg.Mode {
	case RuntimeModeNone:
		// No runtimes should be configured.
		if viper.IsSet(CfgRuntimePaths) && !cmdFlags.DebugDontBlameOasis() {
			return nil, fmt.Errorf("no runtimes should be configured when not in runtime mode")
		}
	default:
		// In any other mode, at least one runtime should be configured.
		if !viper.IsSet(CfgRuntimePaths) && !cmdFlags.DebugDontBlameOasis() {
			return nil, fmt.Errorf("at least one runtime must be configured when in runtime mode")
		}
	}

	// Check if any runtimes are configured to be hosted.
	if viper.IsSet(CfgRuntimePaths) {
		var rh RuntimeHostConfig

		// Configure host environment information.
		cs, err := consensus.GetStatus(context.Background())
		if err != nil {
			return nil, fmt.Errorf("failed to get consensus layer status: %w", err)
		}
		chainCtx, err := consensus.GetChainContext(context.Background())
		if err != nil {
			return nil, fmt.Errorf("failed to get chain context: %w", err)
		}
		hostInfo := &hostProtocol.HostInfo{
			ConsensusBackend:         cs.Backend,
			ConsensusProtocolVersion: cs.Version,
			ConsensusChainContext:    chainCtx,
		}

		// Register provisioners based on the configured provisioner.
		var insecureNoSandbox bool
		sandboxBinary := viper.GetString(CfgSandboxBinary)
		rh.Provisioners = make(map[node.TEEHardware]runtimeHost.Provisioner)
		switch p := viper.GetString(CfgRuntimeProvisioner); p {
		case RuntimeProvisionerMock:
			// Mock provisioner, only supported when the runtime requires no TEE hardware.
			if !cmdFlags.DebugDontBlameOasis() {
				return nil, fmt.Errorf("mock provisioner requires use of unsafe debug flags")
			}

			rh.Provisioners[node.TEEHardwareInvalid] = hostMock.New()
		case RuntimeProvisionerUnconfined:
			// Unconfined provisioner, can be used with no TEE or with Intel SGX.
			if !cmdFlags.DebugDontBlameOasis() {
				return nil, fmt.Errorf("unconfined provisioner requires use of unsafe debug flags")
			}

			insecureNoSandbox = true

			fallthrough
		case RuntimeProvisionerSandboxed:
			if !insecureNoSandbox {
				if _, err = os.Stat(sandboxBinary); err != nil {
					return nil, fmt.Errorf("failed to stat sandbox binary: %w", err)
				}
			}

			// Sandboxed provisioner, can be used with no TEE or with Intel SGX.
			rh.Provisioners[node.TEEHardwareInvalid], err = hostSandbox.New(hostSandbox.Config{
				HostInfo:          hostInfo,
				InsecureNoSandbox: insecureNoSandbox,
				SandboxBinaryPath: sandboxBinary,
			})
			if err != nil {
				return nil, fmt.Errorf("failed to create runtime provisioner: %w", err)
			}

			switch sgxLoader := viper.GetString(CfgRuntimeSGXLoader); sgxLoader {
			case "":
				// No SGX loader is configured, remap to non-SGX.
				rh.Provisioners[node.TEEHardwareIntelSGX], err = hostSandbox.New(hostSandbox.Config{
					HostInfo:          hostInfo,
					InsecureNoSandbox: insecureNoSandbox,
					SandboxBinaryPath: sandboxBinary,
				})
				if err != nil {
					return nil, fmt.Errorf("failed to create runtime provisioner: %w", err)
				}
			default:
				// Configure the provided SGX loader.
				rh.Provisioners[node.TEEHardwareIntelSGX], err = hostSgx.New(hostSgx.Config{
					HostInfo:          hostInfo,
					LoaderPath:        sgxLoader,
					IAS:               ias,
					SandboxBinaryPath: sandboxBinary,
					InsecureNoSandbox: insecureNoSandbox,
				})
				if err != nil {
					return nil, fmt.Errorf("failed to create SGX runtime provisioner: %w", err)
				}
			}
		default:
			return nil, fmt.Errorf("unsupported runtime provisioner: %s", p)
		}

		// Configure runtimes.
		for _, path := range viper.GetStringSlice(CfgRuntimePaths) {
			// XXX: Special case the config if the mock provisioner is being
			// used since it is unlikely that we will actually have a bundle.

			// Open and explode the bundle.  This will call Validate().
			bnd, err := bundle.Open(path)
			if err != nil {
				return nil, fmt.Errorf("failed to load runtime bundle '%s': %w", path, err)
			}
			if err = bnd.WriteExploded(dataDir); err != nil {
				return nil, fmt.Errorf("failed to explode runtime bundle '%s': %w", path, err)
			}

			id := bnd.Manifest.ID
			if rh.Runtimes[id] != nil {
				// TODO: Support multiple versions of the same runtime.  The
				// old version of this used a map, but the new version uses
				// a vector so we need to de-duplicate for now.
				return nil, fmt.Errorf("runtime '%s' already configured", id)
			}

			// Unmarshal any local runtime configuration.
			var localConfig map[string]interface{}
			if sub := viper.Sub(CfgRuntimeConfig); sub != nil {
				if err := sub.UnmarshalKey(id.String(), &localConfig); err != nil {
					return nil, fmt.Errorf("bad runtime configuration: %w", err)
				}
			}

			runtimeHostCfg := &runtimeHost.Config{
				Bundle:      bnd,
				Path:        bnd.ExplodedPath(dataDir, bnd.Manifest.Executable),
				LocalConfig: localConfig,
			}
			if bnd.Manifest.SGX != nil {
				runtimeHostCfg.Path = bnd.ExplodedPath(dataDir, bnd.Manifest.SGX.Executable)
				runtimeHostCfg.Extra = &hostSgx.RuntimeExtra{
					SignaturePath: bnd.ExplodedPath(dataDir, bnd.Manifest.SGX.Signature),
				}
			} else {
				// HACK HACK HACK: Allow dummy SIGSTRUCT generation.
				runtimeHostCfg.Extra = &hostSgx.RuntimeExtra{
					UnsafeDebugGenerateSigstruct: true,
				}
			}

			rh.Runtimes[id] = runtimeHostCfg
		}
		if len(rh.Runtimes) == 0 {
			return nil, fmt.Errorf("no runtimes configured")
		}

		cfg.Host = &rh
	}

	strategy := viper.GetString(CfgHistoryPrunerStrategy)
	switch strings.ToLower(strategy) {
	case history.PrunerStrategyNone:
		cfg.History.Pruner = history.NewNonePruner()
	case history.PrunerStrategyKeepLast:
		numKept := viper.GetUint64(CfgHistoryPrunerKeepLastNum)
		cfg.History.Pruner = history.NewKeepLastPruner(numKept)
	default:
		return nil, fmt.Errorf("runtime/registry: unknown history pruner strategy: %s", strategy)
	}

	cfg.History.PruneInterval = viper.GetDuration(CfgHistoryPrunerInterval)
	const minPruneInterval = 1 * time.Second
	if cfg.History.PruneInterval < minPruneInterval {
		cfg.History.PruneInterval = minPruneInterval
	}

	return &cfg, nil
}

func init() {
	Flags.String(CfgRuntimeProvisioner, RuntimeProvisionerSandboxed, "Runtime provisioner to use")
	Flags.StringSlice(CfgRuntimePaths, nil, "Paths to runtime resources (format: <path>,<path>,...)")
	Flags.String(CfgSandboxBinary, "/usr/bin/bwrap", "Path to the sandbox binary (bubblewrap)")
	Flags.String(CfgRuntimeSGXLoader, "", "(for SGX runtimes) Path to SGXS runtime loader binary")

	Flags.String(CfgHistoryPrunerStrategy, history.PrunerStrategyNone, "History pruner strategy")
	Flags.Duration(CfgHistoryPrunerInterval, 2*time.Minute, "History pruning interval")
	Flags.Uint64(CfgHistoryPrunerKeepLastNum, 600, "Keep last history pruner: number of last rounds to keep")

	Flags.String(CfgRuntimeMode, string(RuntimeModeNone), "Runtime mode (none, compute, keymanager, client, client-stateless)")

	_ = viper.BindPFlags(Flags)
}
