package execution

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"go.uber.org/zap"

	"github.com/lambda-feedback/shimmy/internal/execution/dispatcher"
	"github.com/lambda-feedback/shimmy/internal/execution/supervisor"
	"github.com/lambda-feedback/shimmy/internal/execution/wasm"
)

type Dispatcher dispatcher.Dispatcher

type Config struct {
	// MaxWorkers is the maximum number of concurrent workers
	// when employing a pooled dispatcher.
	MaxWorkers int `conf:"max_workers"`

	// SupervisorConfig is the configuration to use for the supervisor
	Supervisor supervisor.Config `conf:",squash"`
}

type Params struct {
	// Context is the context to use for the dispatcher
	Context context.Context

	// Config is the config for the dispatcher and the underlying supervisors
	Config Config

	// Log is the logger to use for the dispatcher
	Log *zap.Logger
}

func NewDispatcher(params Params) (dispatcher.Dispatcher, error) {
	switch params.Config.Supervisor.IO.Interface {
	case supervisor.RpcIO:
		return dispatcher.NewDedicatedDispatcher(
			dispatcher.DedicatedDispatcherParams{
				Config: dispatcher.DedicatedDispatcherConfig{
					Supervisor: params.Config.Supervisor,
				},
				Context: params.Context,
				Log:     params.Log,
			},
		)

	case supervisor.WasmIO:
		wasmProfile := strings.ToLower(strings.TrimSpace(os.Getenv("FUNCTION_WASM_PROFILE")))
		if wasmProfile == "" {
			wasmProfile = "generic"
		}
		if wasmProfile != "generic" {
			validProfiles := []string{"generic"}
			sort.Strings(validProfiles)
			return nil, fmt.Errorf("unsupported FUNCTION_WASM_PROFILE %q; supported values: %s", wasmProfile, strings.Join(validProfiles, ", "))
		}

		cfg := wasm.Config{
			ModulePath:   params.Config.Supervisor.StartParams.Cmd,
			MaxInstances: params.Config.MaxWorkers,
			Timeout:      params.Config.Supervisor.SendParams.Timeout,
		}
		d := wasm.NewDispatcher(cfg, params.Log)
		if err := d.Start(params.Context); err != nil {
			return nil, err
		}
		return d, nil

	default:
		return dispatcher.NewPooledDispatcher(
			dispatcher.PooledDispatcherParams{
				Config: dispatcher.PooledDispatcherConfig{
					Supervisor: params.Config.Supervisor,
					MaxWorkers: params.Config.MaxWorkers,
				},
				Context: params.Context,
				Log:     params.Log,
			},
		)
	}
}
