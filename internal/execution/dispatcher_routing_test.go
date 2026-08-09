package execution_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/lambda-feedback/shimmy/internal/execution"
	"github.com/lambda-feedback/shimmy/internal/execution/supervisor"
)

// writeTempScript creates a temporary Python script file with the given
// content and returns its path. The file is cleaned up after the test.
func writeTempScript(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "eval_*.py")
	require.NoError(t, err)
	_, err = f.WriteString(content)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	return f.Name()
}

func TestNewDispatcher_Wasm_GenericProfile_DefaultsToGenericAndErrorsOnMissingModulePath(t *testing.T) {
	t.Setenv("FUNCTION_WASM_PROFILE", "")

	_, err := execution.NewDispatcher(execution.Params{
		Context: context.Background(),
		Config: execution.Config{
			Supervisor: supervisor.Config{
				IO: supervisor.IOConfig{
					Interface: supervisor.WasmIO,
				},
			},
		},
		Log: zap.NewNop(),
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "wasm: ModulePath must be set")
}

func TestNewDispatcher_Wasm_PythonReactorProfile_RoutesToReactorPython(t *testing.T) {
	t.Setenv("FUNCTION_WASM_PROFILE", "python-reactor")

	_, err := execution.NewDispatcher(execution.Params{
		Context: context.Background(),
		Config: execution.Config{
			Supervisor: supervisor.Config{
				IO: supervisor.IOConfig{
					Interface: supervisor.WasmIO,
				},
			},
		},
		Log: zap.NewNop(),
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "PythonScriptPath")
}

func TestNewDispatcher_Wasm_UnknownProfileErrorsWithValidValues(t *testing.T) {
	t.Setenv("FUNCTION_WASM_PROFILE", "ultra-bad-profile")

	_, err := execution.NewDispatcher(execution.Params{
		Context: context.Background(),
		Config: execution.Config{
			Supervisor: supervisor.Config{
				IO: supervisor.IOConfig{
					Interface: supervisor.WasmIO,
				},
			},
		},
		Log: zap.NewNop(),
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), `unsupported FUNCTION_WASM_PROFILE "ultra-bad-profile"`)
	assert.Contains(t, err.Error(), "generic")
	assert.Contains(t, err.Error(), "python-reactor")
	assert.NotContains(t, err.Error(), "agent-python")
}

func TestNewDispatcher_Wasm_LegacyEvaluatorNamedProfileIsRejected(t *testing.T) {
	t.Setenv("FUNCTION_WASM_PROFILE", "agent-python")

	_, err := execution.NewDispatcher(execution.Params{
		Context: context.Background(),
		Config: execution.Config{
			Supervisor: supervisor.Config{IO: supervisor.IOConfig{Interface: supervisor.WasmIO}},
		},
		Log: zap.NewNop(),
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), `unsupported FUNCTION_WASM_PROFILE "agent-python"`)
	assert.Contains(t, err.Error(), "python-reactor")
}

func TestNewDispatcher_LegacyReactorPythonInterfaceIsRejected(t *testing.T) {
	_, err := execution.NewDispatcher(execution.Params{
		Context: context.Background(),
		Config: execution.Config{
			Supervisor: supervisor.Config{
				IO: supervisor.IOConfig{
					Interface: supervisor.IOInterface("reactor-python"),
				},
			},
		},
		Log: zap.NewNop(),
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), `unsupported execution interface "reactor-python"`)
}

func TestNewDispatcher_PyodideScriptModeRequiresScriptEnv(t *testing.T) {
	t.Setenv("FUNCTION_PYODIDE_SCRIPT", "")
	t.Setenv("FUNCTION_PYODIDE_ROOT", "")
	t.Setenv("FUNCTION_PYODIDE_EVAL_ENTRYPOINT", "")

	_, err := execution.NewDispatcher(execution.Params{
		Context: context.Background(),
		Config: execution.Config{
			Supervisor: supervisor.Config{
				IO: supervisor.IOConfig{Interface: supervisor.PyodideIO},
			},
		},
		Log: zap.NewNop(),
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "FUNCTION_PYODIDE_SCRIPT must be set")
}

func TestNewDispatcher_PyodidePackageModeAcceptsEnvironmentConfig(t *testing.T) {
	t.Setenv("FUNCTION_PYODIDE_RUNNER", filepath.Join(t.TempDir(), "runner.js"))
	t.Setenv("FUNCTION_PYODIDE_ROOT", t.TempDir())
	t.Setenv("FUNCTION_PYODIDE_EVAL_ENTRYPOINT", "evaluation_function.evaluation:evaluation_function")
	t.Setenv("FUNCTION_PYODIDE_SCRIPT", "")

	d, err := execution.NewDispatcher(execution.Params{
		Context: context.Background(),
		Config: execution.Config{
			Supervisor: supervisor.Config{
				IO: supervisor.IOConfig{Interface: supervisor.PyodideIO},
			},
		},
		Log: zap.NewNop(),
	})

	require.NoError(t, err)
	require.NotNil(t, d)
}

// TestNewDispatcher_ReactorPython_ExplicitInterface_EmptyModulePath verifies
// that an explicitly selected reactor-python interface takes the reactor path
// and fails with a ModulePath error (not a node/pyodide error).
func TestNewDispatcher_ReactorPython_ExplicitInterface_EmptyModulePath(t *testing.T) {
	t.Setenv("FUNCTION_WASM_PROFILE", "python-reactor")
	script := writeTempScript(t, "def dispatch(method, payload):\n    return {'method': method, 'payload': payload}\n")

	t.Setenv("FUNCTION_WASM_PYTHON_SCRIPT", script)
	// Ensure FUNCTION_PYODIDE_RUNNER is cleared — it won't be reached anyway.
	t.Setenv("FUNCTION_PYODIDE_RUNNER", "")

	_, err := execution.NewDispatcher(execution.Params{
		Context: context.Background(),
		Config: execution.Config{
			Supervisor: supervisor.Config{
				IO: supervisor.IOConfig{
					Interface: supervisor.WasmIO,
				},
				// Leave StartParams.Cmd empty → reactor-python runner will fail
				// with "ModulePath must be set".
			},
		},
		Log: zap.NewNop(),
	})

	require.Error(t, err, "reactor-python with empty ModulePath must fail")
	assert.Contains(t, err.Error(), "ModulePath",
		"error should be the reactor-python WASM config error, not a node/pyodide error")
}

func TestNewDispatcher_ReactorPythonIgnoresEvaluatorPackagingEnvironment(t *testing.T) {
	t.Setenv("FUNCTION_WASM_PROFILE", "python-reactor")
	script := writeTempScript(t, "def dispatch(method, payload):\n    return {'method': method, 'payload': payload}\n")
	t.Setenv("FUNCTION_WASM_PYTHON_SCRIPT", script)
	t.Setenv("FUNCTION_LF_ROOT", t.TempDir())
	t.Setenv("FUNCTION_LF_BUNDLER", filepath.Join(t.TempDir(), "must-not-run"))

	_, err := execution.NewDispatcher(execution.Params{
		Context: context.Background(),
		Config: execution.Config{
			Supervisor: supervisor.Config{
				IO: supervisor.IOConfig{Interface: supervisor.WasmIO},
			},
		},
		Log: zap.NewNop(),
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "ModulePath")
	assert.NotContains(t, err.Error(), "Lambda Feedback")
}

// TestNewDispatcher_ReactorPython_EmptyScriptPath verifies that when
// FUNCTION_WASM_PYTHON_SCRIPT is not set, the dispatcher skips the import
// scan entirely and falls through to the reactor-python path, which then
// fails because PythonScriptPath is required.
func TestNewDispatcher_ReactorPython_EmptyScriptPath(t *testing.T) {
	t.Setenv("FUNCTION_WASM_PROFILE", "python-reactor")
	t.Setenv("FUNCTION_WASM_PYTHON_SCRIPT", "")

	_, err := execution.NewDispatcher(execution.Params{
		Context: context.Background(),
		Config: execution.Config{
			Supervisor: supervisor.Config{
				IO: supervisor.IOConfig{
					Interface: supervisor.WasmIO,
				},
			},
		},
		Log: zap.NewNop(),
	})

	require.Error(t, err, "reactor-python with no script path must fail")
	assert.Contains(t, err.Error(), "PythonScriptPath",
		"empty script path should fall through to reactor-python, not Pyodide")
}
