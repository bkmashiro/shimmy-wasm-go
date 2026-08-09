package execution

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lambda-feedback/shimmy/internal/execution/supervisor"
)

func setValidQEMUEnvironment(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	for env, name := range map[string]string{
		"FUNCTION_QEMU_BINARY":         "qemu-system-x86_64",
		"FUNCTION_QEMU_ROOTFS":         "evaluator.qcow2",
		"FUNCTION_QEMU_IMAGE_MANIFEST": "manifest.json",
	} {
		path := filepath.Join(root, name)
		require.NoError(t, os.WriteFile(path, []byte("fixture"), 0o600))
		t.Setenv(env, path)
	}
	t.Setenv("FUNCTION_QEMU_ENABLED", "true")
	t.Setenv("FUNCTION_QEMU_ACCELERATOR", "tcg")
}

func TestApplyQEMUFallbackConfigLeavesDisabledWorkerExactlyUnchanged(t *testing.T) {
	original := representativeRPCConfig(supervisor.HttpTransport)
	got, err := applyQEMUFallbackConfig(original)
	require.NoError(t, err)
	assert.Equal(t, original, got)
}

func TestApplyQEMUFallbackConfigWrapsFileWorkerTransparently(t *testing.T) {
	setValidQEMUEnvironment(t)
	t.Setenv("FUNCTION_QEMU_RUNNER", "/opt/shimmy qemu/runner")
	original := supervisor.Config{
		IO: supervisor.IOConfig{Interface: supervisor.FileIO},
		StartParams: supervisor.StartConfig{
			Cmd:  "/opt/evaluator/python 3",
			Args: []string{"worker.py", "arg with spaces", "--literal=--"},
			Cwd:  "/opt/evaluator dir",
			Env:  []string{"TOKEN=not-logged", "DUP=last"},
		},
		StopParams: supervisor.StopConfig{Timeout: 2 * time.Second},
		SendParams: supervisor.SendConfig{Timeout: 3 * time.Second},
	}

	got, err := applyQEMUFallbackConfig(original)
	require.NoError(t, err)
	assert.Equal(t, original.IO, got.IO)
	assert.Equal(t, original.StartParams.Cwd, got.StartParams.Cwd)
	assert.Equal(t, original.StartParams.Env, got.StartParams.Env)
	assert.Equal(t, original.StopParams, got.StopParams)
	assert.Equal(t, original.SendParams, got.SendParams)
	assert.Equal(t, "/opt/shimmy qemu/runner", got.StartParams.Cmd)
	assert.Equal(t, []string{"--", "/opt/evaluator/python 3", "worker.py", "arg with spaces", "--literal=--"}, got.StartParams.Args)
}

func TestApplyQEMUFallbackConfigPreservesEveryRPCTransportConfig(t *testing.T) {
	setValidQEMUEnvironment(t)
	for _, transport := range []supervisor.IOTransport{
		supervisor.StdioTransport,
		supervisor.IpcTransport,
		supervisor.TcpTransport,
		supervisor.HttpTransport,
		supervisor.WsTransport,
	} {
		t.Run(string(transport), func(t *testing.T) {
			original := representativeRPCConfig(transport)
			got, err := applyQEMUFallbackConfig(original)
			require.NoError(t, err)
			assert.Equal(t, original.IO, got.IO)
			assert.Equal(t, defaultQEMURunner, got.StartParams.Cmd)
			assert.Equal(t, []string{"--", "python3", "rpc.py", "--serve"}, got.StartParams.Args)
			assert.Equal(t, original.StartParams.Cwd, got.StartParams.Cwd)
			assert.Equal(t, original.StartParams.Env, got.StartParams.Env)
		})
	}
}

func TestApplyQEMUFallbackConfigLambdaDefaultsToLazyInvocationLifecycle(t *testing.T) {
	setValidQEMUEnvironment(t)
	t.Setenv("AWS_LAMBDA_RUNTIME_API", "127.0.0.1:9001")
	t.Setenv("FUNCTION_QEMU_RESET_POLICY", "")

	got, err := applyQEMUFallbackConfig(representativeRPCConfig(supervisor.StdioTransport))
	require.NoError(t, err)
	assert.Equal(t, supervisor.WorkerLifecycleInvocation, got.WorkerLifecycle)
}

func TestApplyQEMUFallbackConfigNonLambdaKeepsPersistentRPCDefault(t *testing.T) {
	setValidQEMUEnvironment(t)
	t.Setenv("AWS_LAMBDA_RUNTIME_API", "")
	t.Setenv("FUNCTION_QEMU_RESET_POLICY", "")

	got, err := applyQEMUFallbackConfig(representativeRPCConfig(supervisor.StdioTransport))
	require.NoError(t, err)
	assert.Equal(t, supervisor.WorkerLifecycleAutomatic, got.WorkerLifecycle)
}

func TestApplyQEMUFallbackConfigExplicitLazyUsesInvocationLifecycleOutsideLambda(t *testing.T) {
	setValidQEMUEnvironment(t)
	t.Setenv("AWS_LAMBDA_RUNTIME_API", "")
	t.Setenv("FUNCTION_QEMU_RESET_POLICY", "lazy")

	got, err := applyQEMUFallbackConfig(representativeRPCConfig(supervisor.StdioTransport))
	require.NoError(t, err)
	assert.Equal(t, supervisor.WorkerLifecycleInvocation, got.WorkerLifecycle)
}

func TestApplyQEMUFallbackConfigExplicitOffPreservesPersistentLambdaRPC(t *testing.T) {
	setValidQEMUEnvironment(t)
	t.Setenv("AWS_LAMBDA_RUNTIME_API", "127.0.0.1:9001")
	t.Setenv("FUNCTION_QEMU_RESET_POLICY", "off")

	got, err := applyQEMUFallbackConfig(representativeRPCConfig(supervisor.StdioTransport))
	require.NoError(t, err)
	assert.Equal(t, supervisor.WorkerLifecycleAutomatic, got.WorkerLifecycle)
}

func TestApplyQEMUFallbackConfigUsesEagerLifecycleOutsideLambda(t *testing.T) {
	setValidQEMUEnvironment(t)
	t.Setenv("AWS_LAMBDA_RUNTIME_API", "")
	t.Setenv("FUNCTION_QEMU_RESET_POLICY", "eager")

	got, err := applyQEMUFallbackConfig(representativeRPCConfig(supervisor.StdioTransport))
	require.NoError(t, err)
	assert.Equal(t, supervisor.WorkerLifecycleEager, got.WorkerLifecycle)
}

func TestApplyQEMUFallbackConfigRejectsEagerForFileInterface(t *testing.T) {
	setValidQEMUEnvironment(t)
	t.Setenv("AWS_LAMBDA_RUNTIME_API", "")
	t.Setenv("FUNCTION_QEMU_RESET_POLICY", "eager")

	_, err := applyQEMUFallbackConfig(supervisor.Config{
		IO:          supervisor.IOConfig{Interface: supervisor.FileIO},
		StartParams: supervisor.StartConfig{Cmd: "/opt/evaluator"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "requires rpc interface")
}

func TestApplyQEMUFallbackConfigRejectsEagerInLambdaWithoutPostResponseRuntimeLoop(t *testing.T) {
	setValidQEMUEnvironment(t)
	t.Setenv("AWS_LAMBDA_RUNTIME_API", "127.0.0.1:9001")
	t.Setenv("FUNCTION_QEMU_RESET_POLICY", "eager")

	_, err := applyQEMUFallbackConfig(representativeRPCConfig(supervisor.StdioTransport))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "post-response Lambda runtime loop")
}

func TestApplyQEMUFallbackConfigRejectsUnsupportedResetPolicy(t *testing.T) {
	setValidQEMUEnvironment(t)
	t.Setenv("FUNCTION_QEMU_RESET_POLICY", "adaptive")

	_, err := applyQEMUFallbackConfig(representativeRPCConfig(supervisor.StdioTransport))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "FUNCTION_QEMU_RESET_POLICY")
}

func TestApplyExecutionWrappersRejectsQEMUAndDBITogether(t *testing.T) {
	setValidQEMUEnvironment(t)
	t.Setenv("FUNCTION_DBI_SECURITY_ENABLED", "true")
	_, err := applyExecutionWrappers(representativeRPCConfig(supervisor.StdioTransport))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mutually exclusive")
}

func TestApplyExecutionWrappersAllowsExplicitlyDisabledDBI(t *testing.T) {
	setValidQEMUEnvironment(t)
	t.Setenv("FUNCTION_DBI_SECURITY_ENABLED", "false")
	got, err := applyExecutionWrappers(representativeRPCConfig(supervisor.StdioTransport))
	require.NoError(t, err)
	assert.Equal(t, defaultQEMURunner, got.StartParams.Cmd)
}

func TestApplyQEMUFallbackConfigRejectsInvalidEnabledValue(t *testing.T) {
	t.Setenv("FUNCTION_QEMU_ENABLED", "sometimes")
	_, err := applyQEMUFallbackConfig(representativeRPCConfig(supervisor.StdioTransport))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "qemu: invalid FUNCTION_QEMU_ENABLED")
}

func TestApplyQEMUFallbackConfigRejectsMissingRequiredArtifacts(t *testing.T) {
	for _, missing := range []string{"FUNCTION_QEMU_BINARY", "FUNCTION_QEMU_ROOTFS", "FUNCTION_QEMU_IMAGE_MANIFEST"} {
		t.Run(missing, func(t *testing.T) {
			setValidQEMUEnvironment(t)
			t.Setenv(missing, "")
			_, err := applyQEMUFallbackConfig(representativeRPCConfig(supervisor.StdioTransport))
			require.Error(t, err)
			assert.Contains(t, err.Error(), "qemu:")
			assert.Contains(t, err.Error(), missing)
		})
	}
}

func TestApplyQEMUFallbackConfigRejectsUnreadableArtifact(t *testing.T) {
	setValidQEMUEnvironment(t)
	missing := filepath.Join(t.TempDir(), "missing.qcow2")
	t.Setenv("FUNCTION_QEMU_ROOTFS", missing)
	_, err := applyQEMUFallbackConfig(representativeRPCConfig(supervisor.StdioTransport))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "FUNCTION_QEMU_ROOTFS")
	assert.Contains(t, err.Error(), "not readable")
}

func TestApplyQEMUFallbackConfigRequiresExplicitAccelerator(t *testing.T) {
	for _, value := range []string{"", "auto", "magic"} {
		t.Run(value, func(t *testing.T) {
			setValidQEMUEnvironment(t)
			t.Setenv("FUNCTION_QEMU_ACCELERATOR", value)
			_, err := applyQEMUFallbackConfig(representativeRPCConfig(supervisor.StdioTransport))
			require.Error(t, err)
			assert.Contains(t, err.Error(), "FUNCTION_QEMU_ACCELERATOR")
			assert.Contains(t, err.Error(), "tcg or kvm")
		})
	}
}

func TestApplyQEMUFallbackConfigRejectsUnsupportedInterfaces(t *testing.T) {
	setValidQEMUEnvironment(t)
	for _, iface := range []supervisor.IOInterface{supervisor.WasmIO, supervisor.PythonWasmIO, supervisor.ReactorPythonIO, supervisor.PyodideIO} {
		t.Run(string(iface), func(t *testing.T) {
			_, err := applyQEMUFallbackConfig(supervisor.Config{
				IO:          supervisor.IOConfig{Interface: iface},
				StartParams: supervisor.StartConfig{Cmd: "worker"},
			})
			require.Error(t, err)
			assert.Contains(t, err.Error(), "only supported for file and rpc")
		})
	}
}

func TestApplyQEMUFallbackConfigRejectsMissingOriginalCommand(t *testing.T) {
	setValidQEMUEnvironment(t)
	_, err := applyQEMUFallbackConfig(supervisor.Config{IO: supervisor.IOConfig{Interface: supervisor.FileIO}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "FUNCTION_COMMAND")
}

func representativeRPCConfig(transport supervisor.IOTransport) supervisor.Config {
	return supervisor.Config{
		IO: supervisor.IOConfig{
			Interface: supervisor.RpcIO,
			Rpc: supervisor.RpcConfig{
				Transport: transport,
				Http:      supervisor.HttpTransportConfig{Url: "http://127.0.0.1:8088/rpc"},
				Ipc:       supervisor.IpcTransportConfig{Endpoint: "/tmp/evaluator.sock"},
				Ws:        supervisor.WsTransportConfig{Url: "ws://127.0.0.1:8089/rpc"},
				Tcp:       supervisor.TcpTransportConfig{Address: "127.0.0.1:9099"},
			},
		},
		StartParams: supervisor.StartConfig{
			Cmd:  "python3",
			Args: []string{"rpc.py", "--serve"},
			Cwd:  "/opt/evaluator",
			Env:  []string{"VISIBLE=yes"},
		},
		StopParams: supervisor.StopConfig{Timeout: time.Second},
		SendParams: supervisor.SendConfig{Timeout: 5 * time.Second},
	}
}
