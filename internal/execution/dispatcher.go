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
	supervisorCfg, err := applyExecutionWrappers(params.Config.Supervisor)
	if err != nil {
		return nil, err
	}

	// wasmBaseConfig builds the Config fields shared by all WASM-backed
	// dispatchers (Wasm, PythonWasm, ReactorPython).
	wasmBaseConfig := func() wasm.Config {
		return wasm.Config{
			ModulePath:   supervisorCfg.StartParams.Cmd,
			MaxInstances: params.Config.MaxWorkers,
			Timeout:      supervisorCfg.SendParams.Timeout,
		}
	}

	newReactorPythonDispatcher := func() (dispatcher.Dispatcher, error) {
		cfg := wasmBaseConfig()
		cfg.PythonScriptPath = os.Getenv("FUNCTION_WASM_PYTHON_SCRIPT")
		d := wasm.NewAgentPythonDispatcher(cfg, params.Log)
		if err := d.Start(params.Context); err != nil {
			return nil, err
		}
		return d, nil
	}

	newGenericWasmDispatcher := func() (dispatcher.Dispatcher, error) {
		cfg := wasmBaseConfig()
		d := wasm.NewDispatcher(cfg, params.Log)
		if err := d.Start(params.Context); err != nil {
			return nil, err
		}
		return d, nil
	}

	validWasmProfiles := []string{"generic", "python-reactor"}
	wasmProfile := strings.ToLower(strings.TrimSpace(os.Getenv("FUNCTION_WASM_PROFILE")))

	switch supervisorCfg.IO.Interface {
	case supervisor.WasmIO:
		if wasmProfile == "" {
			wasmProfile = "generic"
		}

		switch wasmProfile {
		case "generic":
			return newGenericWasmDispatcher()
		case "python-reactor":
			return newReactorPythonDispatcher()
		default:
			sort.Strings(validWasmProfiles)
			return nil, fmt.Errorf("unsupported FUNCTION_WASM_PROFILE %q; supported values: %s", wasmProfile, strings.Join(validWasmProfiles, ", "))
		}

	case supervisor.PyodideIO:
		// Pyodide uses the rpc dispatcher with stdio transport.
		// Build a supervisor config that runs:
		//   node <runner_path> <script_path>
		// in legacy mode, or node <runner_path> in package mode.
		// Legacy mode still uses FUNCTION_PYODIDE_SCRIPT or argv[2]-style runners.
		// Package mode is selected when FUNCTION_PYODIDE_ROOT and
		// FUNCTION_PYODIDE_EVAL_ENTRYPOINT are both set.
		runnerPath := os.Getenv("FUNCTION_PYODIDE_RUNNER")
		if runnerPath == "" {
			runnerPath = "runner.js" // assume cwd contains runner.js
		}

		pyodideScriptPath := os.Getenv("FUNCTION_PYODIDE_SCRIPT")
		pyodideRoot := os.Getenv("FUNCTION_PYODIDE_ROOT")
		pyodideEvalEntrypoint := os.Getenv("FUNCTION_PYODIDE_EVAL_ENTRYPOINT")
		pyodidePackageMode := pyodideRoot != "" && pyodideEvalEntrypoint != ""

		pyodideSupervisorCfg := supervisorCfg
		pyodideSupervisorCfg.IO.Interface = supervisor.RpcIO
		pyodideSupervisorCfg.IO.Rpc.Transport = supervisor.StdioTransport
		pyodideSupervisorCfg.StartParams.Cmd = "node"
		if pyodidePackageMode {
			pyodideSupervisorCfg.StartParams.Args = []string{runnerPath}
		} else {
			if pyodideScriptPath == "" {
				return nil, fmt.Errorf("pyodide: FUNCTION_PYODIDE_SCRIPT must be set (or provide FUNCTION_PYODIDE_ROOT + FUNCTION_PYODIDE_EVAL_ENTRYPOINT)")
			}
			pyodideSupervisorCfg.StartParams.Args = []string{runnerPath, pyodideScriptPath}
		}

		return dispatcher.NewDedicatedDispatcher(
			dispatcher.DedicatedDispatcherParams{
				Config: dispatcher.DedicatedDispatcherConfig{
					Supervisor: pyodideSupervisorCfg,
				},
				Context: params.Context,
				Log:     params.Log,
			},
		)

	case supervisor.RpcIO:
		return dispatcher.NewDedicatedDispatcher(
			dispatcher.DedicatedDispatcherParams{
				Config: dispatcher.DedicatedDispatcherConfig{
					Supervisor: supervisorCfg,
				},
				Context: params.Context,
				Log:     params.Log,
			},
		)

	case supervisor.FileIO:
		return dispatcher.NewPooledDispatcher(
			dispatcher.PooledDispatcherParams{
				Config: dispatcher.PooledDispatcherConfig{
					Supervisor: supervisorCfg,
					MaxWorkers: params.Config.MaxWorkers,
				},
				Context: params.Context,
				Log:     params.Log,
			},
		)

	default:
		return nil, fmt.Errorf("unsupported execution interface %q", supervisorCfg.IO.Interface)
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
