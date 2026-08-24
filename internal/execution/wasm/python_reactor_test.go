package wasm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"go.uber.org/zap"
)

func writePythonReactorManifestFixture(t *testing.T, customModule, customName string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	wasmPath := filepath.Join(dir, "python-reactor-runtime.wasm")
	wasmBytes := []byte("\x00asm\x01\x00\x00\x00fixture")
	require.NoError(t, os.WriteFile(wasmPath, wasmBytes, 0o644))
	digest := sha256.Sum256(wasmBytes)
	manifest := map[string]any{
		"schema_version":   2,
		"abi_version":      "v1",
		"artifact_profile": "base",
		"target":           "wasm32-wasip1",
		"artifact": map[string]any{
			"filename": filepath.Base(wasmPath),
			"size":     len(wasmBytes),
			"sha256":   hex.EncodeToString(digest[:]),
		},
		"build": map[string]any{
			"repository_commit": "a3b7c9d1e5f80123456789abcdef0123456789ab",
			"source_date_epoch": "1784781655",
			"compiler_target":   "wasm32-wasip1",
			"execution_model":   "reactor",
		},
		"wasm": map[string]any{
			"exports": []string{
				"memory", "runtime_init", "runtime_prepare", "alloc", "dealloc", "execute", "_initialize",
			},
			"imports": []map[string]string{
				{"module": customModule, "name": customName},
				{"module": "wasi_snapshot_preview1", "name": "random_get"},
			},
		},
	}
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	require.NoError(t, err)
	manifestPath := filepath.Join(dir, "manifest.json")
	require.NoError(t, os.WriteFile(manifestPath, append(encoded, '\n'), 0o644))
	return wasmPath, manifestPath
}

func TestVerifyPythonReactorArtifactRejectsLegacyAgentContract(t *testing.T) {
	wasmPath, manifestPath := writePythonReactorManifestFixture(t, "agent_runtime_v1", "host_call")

	_, err := verifyPythonReactorArtifact(wasmPath, manifestPath)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "shimmy-python-runtime/v1")
}

func writeShimmyPythonManifestFixture(t *testing.T, profile string, modules []string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	wasmPath := filepath.Join(dir, "shimmy-python-runtime-"+profile+".wasm")
	wasmBytes := []byte("\x00asm\x01\x00\x00\x00producer")
	require.NoError(t, os.WriteFile(wasmPath, wasmBytes, 0o644))
	digest := sha256.Sum256(wasmBytes)
	manifest := map[string]any{
		"schema":            "shimmy-python-runtime-artifact/v1",
		"artifact_contract": "shimmy-python-runtime/v1",
		"profile":           profile, "target": "wasm32-wasip1", "execution_model": "reactor",
		"python_modules": modules, "identity_u32": 1397772849,
		"producer": map[string]any{
			"project": "shimmy", "repository": "lambda-feedback/shimmy",
			"commit": "a3b7c9d1e5f80123456789abcdef0123456789ab", "dirty": false,
		},
		"source_date_epoch": 1784781655,
		"artifact": map[string]any{
			"name": filepath.Base(wasmPath), "size": len(wasmBytes), "sha256": hex.EncodeToString(digest[:]),
		},
		"wasm": map[string]any{
			"exports": []map[string]string{
				{"name": "memory", "kind": "memory"}, {"name": "_initialize", "kind": "function"},
				{"name": "shimmy_python_runtime_identity", "kind": "function"},
				{"name": "shimmy_python_init", "kind": "function"},
				{"name": "shimmy_python_prepare", "kind": "function"},
				{"name": "alloc", "kind": "function"}, {"name": "dealloc", "kind": "function"},
				{"name": "evaluate", "kind": "function"},
			},
			"imports": []map[string]string{{"module": "wasi_snapshot_preview1", "name": "fd_write", "kind": "function"}},
		},
	}
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	require.NoError(t, err)
	manifestPath := filepath.Join(dir, "manifest.json")
	require.NoError(t, os.WriteFile(manifestPath, append(encoded, '\n'), 0o644))
	return wasmPath, manifestPath
}

func TestVerifyPythonReactorArtifactAcceptsShimmyProducerContract(t *testing.T) {
	wasmPath, manifestPath := writeShimmyPythonManifestFixture(t, "sympy", []string{"mpmath", "sympy"})
	artifact, err := verifyPythonReactorArtifact(wasmPath, manifestPath)
	require.NoError(t, err)
	assert.Equal(t, "shimmy-python-runtime/v1", artifact.ABI)
	assert.Equal(t, []string{"mpmath", "sympy"}, artifact.PythonModules)
	assert.Equal(t, "shimmy_python_init", artifact.InitExport)
	assert.Equal(t, "shimmy_python_prepare", artifact.PrepareExport)
	assert.Equal(t, "evaluate", artifact.ExecuteExport)
}

func TestVerifyPythonReactorArtifactRejectsShimmyProducerDigestDrift(t *testing.T) {
	wasmPath, manifestPath := writeShimmyPythonManifestFixture(t, "base", nil)
	require.NoError(t, os.WriteFile(wasmPath, []byte("\x00asm\x01\x00\x00\x00produces"), 0o644))

	_, err := verifyPythonReactorArtifact(wasmPath, manifestPath)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not match manifest")
}

func TestVerifyPythonReactorArtifactRejectsFalseProfileModules(t *testing.T) {
	wasmPath, manifestPath := writeShimmyPythonManifestFixture(t, "base", []string{"sympy"})
	_, err := verifyPythonReactorArtifact(wasmPath, manifestPath)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "python_modules")
}

func validPythonReactorModuleShape() pythonReactorModuleShape {
	i32 := api.ValueTypeI32
	return pythonReactorModuleShape{
		Exports: map[string]pythonReactorFunctionSignature{
			"_initialize":                    {},
			"shimmy_python_runtime_identity": {Results: []api.ValueType{i32}},
			"shimmy_python_init":             {Results: []api.ValueType{i32}},
			"shimmy_python_prepare":          {Params: []api.ValueType{i32, i32}, Results: []api.ValueType{i32}},
			"alloc":                          {Params: []api.ValueType{i32}, Results: []api.ValueType{i32}},
			"dealloc":                        {Params: []api.ValueType{i32}},
			"evaluate":                       {Params: []api.ValueType{i32, i32}, Results: []api.ValueType{i32}},
		},
		ExportedMemories: map[string]struct{}{"memory": {}},
		Imports: map[pythonReactorImport]struct{}{
			{Module: "wasi_snapshot_preview1", Name: "fd_write"}: {},
		},
	}
}

func validPythonReactorArtifactContract() *PythonReactorArtifact {
	return &PythonReactorArtifact{
		ABI:           "shimmy-python-runtime/v1",
		InitExport:    "shimmy_python_init",
		PrepareExport: "shimmy_python_prepare",
		ExecuteExport: "evaluate",
		DeclaredExports: []string{
			"memory", "_initialize", "shimmy_python_runtime_identity", "shimmy_python_init",
			"shimmy_python_prepare", "alloc", "dealloc", "evaluate",
		},
		DeclaredImports: []pythonReactorImport{
			{Module: "wasi_snapshot_preview1", Name: "fd_write"},
		},
	}
}

func TestVerifyPythonReactorModuleShapeAcceptsShimmyProducerABI(t *testing.T) {
	i32 := api.ValueTypeI32
	shape := pythonReactorModuleShape{
		Exports: map[string]pythonReactorFunctionSignature{
			"_initialize":                    {},
			"shimmy_python_runtime_identity": {Results: []api.ValueType{i32}},
			"shimmy_python_init":             {Results: []api.ValueType{i32}},
			"shimmy_python_prepare":          {Params: []api.ValueType{i32, i32}, Results: []api.ValueType{i32}},
			"alloc":                          {Params: []api.ValueType{i32}, Results: []api.ValueType{i32}},
			"dealloc":                        {Params: []api.ValueType{i32}},
			"evaluate":                       {Params: []api.ValueType{i32, i32}, Results: []api.ValueType{i32}},
		},
		ExportedMemories: map[string]struct{}{"memory": {}},
		Imports: map[pythonReactorImport]struct{}{
			{Module: "wasi_snapshot_preview1", Name: "fd_write"}: {},
		},
	}
	artifact := &PythonReactorArtifact{
		ABI: "shimmy-python-runtime/v1", InitExport: "shimmy_python_init",
		PrepareExport: "shimmy_python_prepare", ExecuteExport: "evaluate",
		DeclaredExports: []string{"memory", "_initialize", "shimmy_python_runtime_identity", "shimmy_python_init", "shimmy_python_prepare", "alloc", "dealloc", "evaluate"},
		DeclaredImports: []pythonReactorImport{{Module: "wasi_snapshot_preview1", Name: "fd_write"}},
	}
	require.NoError(t, verifyPythonReactorModuleShape(shape, artifact))
}

func TestVerifyPythonReactorModuleShapeAcceptsExactContract(t *testing.T) {
	err := verifyPythonReactorModuleShape(validPythonReactorModuleShape(), validPythonReactorArtifactContract())
	require.NoError(t, err)
}

func TestVerifyPythonReactorModuleShapeRejectsUndeclaredActualImport(t *testing.T) {
	shape := validPythonReactorModuleShape()
	shape.Imports[pythonReactorImport{Module: "wasi_snapshot_preview1", Name: "sock_send"}] = struct{}{}

	err := verifyPythonReactorModuleShape(shape, validPythonReactorArtifactContract())

	require.Error(t, err)
	assert.Contains(t, err.Error(), `actual import "wasi_snapshot_preview1"."sock_send" is not declared by manifest`)
}

func TestVerifyPythonReactorModuleShapeRejectsWrongDispatchABISignature(t *testing.T) {
	shape := validPythonReactorModuleShape()
	shape.Exports["evaluate"] = pythonReactorFunctionSignature{
		Params:  []api.ValueType{api.ValueTypeI64},
		Results: []api.ValueType{api.ValueTypeI32},
	}

	err := verifyPythonReactorModuleShape(shape, validPythonReactorArtifactContract())

	require.Error(t, err)
	assert.Contains(t, err.Error(), `export "evaluate" has ABI`)
}

func TestShimmyProducerRequestAndResponseContract(t *testing.T) {
	request, err := buildShimmyPythonRunRequest("preview", map[string]any{"response": "x", "params": map[string]any{}})
	require.NoError(t, err)
	assert.JSONEq(t, `{"method":"preview","params":{"response":"x","params":{}}}`, string(request))

	result, err := decodeShimmyPythonResponse([]byte(`{"status":"ok","result":{"preview":{"sympy":"x"}}}`))
	require.NoError(t, err)
	assert.Equal(t, "x", result["preview"].(map[string]any)["sympy"])
}

func TestShimmyProducerResponsePreservesTypedError(t *testing.T) {
	_, err := decodeShimmyPythonResponse([]byte(`{"status":"error","error":{"type":"ImportError","message":"No module named scipy"}}`))
	var executionErr *PythonReactorExecutionError
	require.ErrorAs(t, err, &executionErr)
	assert.Equal(t, "ImportError", executionErr.ErrorType)
	assert.Equal(t, "No module named scipy", executionErr.Message)
}

func TestPythonReactorRejectsHostFilesystemPaths(t *testing.T) {
	t.Setenv("FUNCTION_WASM_ALLOWED_PATHS", "/tmp")
	dispatcher := NewPythonReactorDispatcher(Config{}, zap.NewNop())
	err := dispatcher.Start(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not expose Host filesystem paths")
}

func TestPythonReactorDispatcherRealNumPyArtifactCompatibility(t *testing.T) {
	wasmPath := os.Getenv("SHIMMY_PYTHON_RUNTIME_WASM")
	manifestPath := os.Getenv("SHIMMY_PYTHON_RUNTIME_MANIFEST")
	if wasmPath == "" || manifestPath == "" {
		t.Skip("SHIMMY_PYTHON_RUNTIME_WASM and SHIMMY_PYTHON_RUNTIME_MANIFEST are required")
	}

	scriptPath := filepath.Join(t.TempDir(), "eval.py")
	script := `
import numpy as np
_counter = 0

def preview_function(response, params):
    return {"preview": f"response={response}"}

def evaluation_function(response, answer, params):
    global _counter
    _counter += 1
    if response == "explode":
        raise ValueError("expected explosion")
    if response == "float128":
        one = np.longdouble("1")
        wide = np.longdouble("1.0000000000000000000000000000000002")
        return {
            "longdouble_itemsize": int(np.dtype(np.longdouble).itemsize),
            "longdouble_nmant": int(np.finfo(np.longdouble).nmant),
            "double_nmant": int(np.finfo(np.double).nmant),
            "preserves_extra_precision": bool(wide > one),
            "narrows_to_double_one": bool(float(wide) == 1.0),
            "epsilon_is_narrower": bool(np.finfo(np.longdouble).eps < np.finfo(np.double).eps),
            "counter": _counter,
        }
    return {"is_correct": response == answer, "counter": _counter}
`
	require.NoError(t, os.WriteFile(scriptPath, []byte(script), 0o644))

	dispatcher := NewPythonReactorDispatcher(Config{
		ModulePath:                wasmPath,
		PythonReactorManifestPath: manifestPath,
		PythonScriptPath:          scriptPath,
		PythonLifecycle:           "snapshot",
		MaxInstances:              1,
		MaxMemoryPages:            8192,
		Timeout:                   120 * time.Second,
	}, zap.NewNop())
	startContext, startCancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer startCancel()
	require.NoError(t, dispatcher.Start(startContext))
	t.Cleanup(func() {
		shutdownContext, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = dispatcher.Shutdown(shutdownContext)
	})

	health, err := dispatcher.Send(context.Background(), "healthcheck", nil)
	require.NoError(t, err)
	assert.Equal(t, "snapshot", health["result"].(map[string]any)["lifecycle"])
	assert.Equal(t, "memcpy", health["result"].(map[string]any)["snapshot_mode"])
	assert.Equal(t, "linear-memory-memcpy", health["result"].(map[string]any)["reset_mode"])

	first, err := dispatcher.Send(context.Background(), "eval", map[string]any{"response": "42", "answer": "42"})
	require.NoError(t, err)
	assert.Equal(t, true, first["result"].(map[string]any)["is_correct"])
	assert.Equal(t, float64(1), first["result"].(map[string]any)["counter"])

	second, err := dispatcher.Send(context.Background(), "eval", map[string]any{"response": "42", "answer": "42"})
	require.NoError(t, err)
	assert.Equal(t, float64(1), second["result"].(map[string]any)["counter"], "snapshot restore must not retain globals")

	preview, err := dispatcher.Send(context.Background(), "preview", map[string]any{"response": "3.14", "answer": "3.14"})
	require.NoError(t, err)
	assert.Equal(t, "response=3.14", preview["result"].(map[string]any)["preview"])

	failure, err := dispatcher.Send(context.Background(), "eval", map[string]any{"response": "explode", "answer": "x"})
	require.Nil(t, failure)
	var failureErr *PythonReactorExecutionError
	require.ErrorAs(t, err, &failureErr)
	assert.Equal(t, "ValueError", failureErr.ErrorType)
	assert.Equal(t, "expected explosion", failureErr.Message)

	binary128, err := dispatcher.Send(context.Background(), "eval", map[string]any{"response": "float128", "answer": "x"})
	require.NoError(t, err)
	value := binary128["result"].(map[string]any)
	assert.Equal(t, float64(16), value["longdouble_itemsize"])
	assert.GreaterOrEqual(t, value["longdouble_nmant"].(float64), float64(112))
	assert.Equal(t, true, value["preserves_extra_precision"])
	assert.Equal(t, true, value["narrows_to_double_one"])
	assert.Equal(t, true, value["epsilon_is_narrower"])
}

func TestAcquirePythonReactorSnapshotSlotReplenishesMissingSlot(t *testing.T) {
	prepared := make(chan *pythonReactorModuleSlot, 1)
	closed := make(chan struct{})
	want := &pythonReactorModuleSlot{snapshotSelected: "memcpy"}
	calls := 0

	got, err := acquirePythonReactorSnapshotSlot(
		context.Background(),
		prepared,
		closed,
		nil,
		func(context.Context) (*pythonReactorModuleSlot, error) {
			calls++
			return want, nil
		},
	)

	require.NoError(t, err)
	assert.Same(t, want, got)
	assert.Equal(t, 1, calls)
}

func TestAcquirePythonReactorSnapshotSlotReturnsReplenishFailure(t *testing.T) {
	prepared := make(chan *pythonReactorModuleSlot, 1)
	closed := make(chan struct{})
	wantErr := errors.New("replacement unavailable")

	_, err := acquirePythonReactorSnapshotSlot(
		context.Background(),
		prepared,
		closed,
		nil,
		func(context.Context) (*pythonReactorModuleSlot, error) {
			return nil, wantErr
		},
	)

	require.ErrorIs(t, err, wantErr)
}

func TestAcquirePythonReactorSnapshotSlotWaitsForInFlightRefill(t *testing.T) {
	prepared := make(chan *pythonReactorModuleSlot, 1)
	closed := make(chan struct{})
	want := &pythonReactorModuleSlot{snapshotSelected: "memcpy"}
	createCalls := 0
	go func() {
		time.Sleep(10 * time.Millisecond)
		prepared <- want
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got, err := acquirePythonReactorSnapshotSlot(
		ctx,
		prepared,
		closed,
		func() bool { return true },
		func(context.Context) (*pythonReactorModuleSlot, error) {
			createCalls++
			return nil, errors.New("must not construct a duplicate slot")
		},
	)

	require.NoError(t, err)
	assert.Same(t, want, got)
	assert.Zero(t, createCalls)
}

func TestRestorePythonReactorSnapshotRejectsMemoryGrowth(t *testing.T) {
	ctx := context.Background()
	rt, compiled := compileEchoModule(t, ctx, echoWasmBytes(t))
	t.Cleanup(func() { require.NoError(t, rt.Close(ctx)) })
	module, err := rt.InstantiateModule(ctx, compiled, wazero.NewModuleConfig())
	require.NoError(t, err)

	strategy := NewFullMemcpyStrategy()
	require.NoError(t, strategy.Take(module.Memory()))
	slot := &pythonReactorModuleSlot{
		module:       module,
		strategy:     strategy,
		baselineSize: module.Memory().Size(),
	}
	_, grew := module.Memory().Grow(1)
	require.True(t, grew)

	err = restorePythonReactorSnapshot(slot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "memory size drift")
}

func TestPythonReactorDispatcherProducerTimeoutReturnsBeforeSnapshotRefill(t *testing.T) {
	wasmPath := os.Getenv("SHIMMY_PYTHON_RUNTIME_WASM")
	manifestPath := os.Getenv("SHIMMY_PYTHON_RUNTIME_MANIFEST")
	evaluatorPath := os.Getenv("SAFE_EVAL_PYTHON_SCRIPT")
	if wasmPath == "" || manifestPath == "" || evaluatorPath == "" {
		t.Skip("SHIMMY_PYTHON_RUNTIME_WASM, SHIMMY_PYTHON_RUNTIME_MANIFEST, and SAFE_EVAL_PYTHON_SCRIPT are required")
	}

	dispatcher := NewPythonReactorDispatcher(Config{
		ModulePath:                wasmPath,
		PythonReactorManifestPath: manifestPath,
		PythonScriptPath:          evaluatorPath,
		PythonLifecycle:           "snapshot",
		MaxInstances:              1,
		MaxMemoryPages:            8192,
		Timeout:                   2 * time.Second,
	}, zap.NewNop())
	startContext, startCancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer startCancel()
	require.NoError(t, dispatcher.Start(startContext))
	t.Cleanup(func() {
		shutdownContext, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = dispatcher.Shutdown(shutdownContext)
	})

	started := time.Now()
	_, err := dispatcher.Send(context.Background(), "eval", map[string]any{
		"response": "while True:\n    pass", "answer": "", "params": map[string]any{"mode": "demo"},
	})
	elapsed := time.Since(started)
	t.Logf("timeout request returned in %s", elapsed)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, elapsed, 10*time.Second, "request must not wait for close or snapshot refill")

	require.Eventually(t, func() bool {
		health, healthErr := dispatcher.Send(context.Background(), "healthcheck", nil)
		if healthErr != nil {
			return false
		}
		return health["result"].(map[string]any)["prepared_ready"] == 1
	}, time.Minute, 100*time.Millisecond)

	recovered, err := dispatcher.Send(context.Background(), "eval", map[string]any{
		"response": "print(7 * 6)", "answer": "", "params": map[string]any{"mode": "demo"},
	})
	require.NoError(t, err)
	assert.Equal(t, "42\n", recovered["result"].(map[string]any)["stdout"])
}

func TestPythonReactorDispatcherSingleUsePreparedRefillsNeverServedCandidates(t *testing.T) {
	wasmPath := os.Getenv("SHIMMY_PYTHON_RUNTIME_WASM")
	manifestPath := os.Getenv("SHIMMY_PYTHON_RUNTIME_MANIFEST")
	if wasmPath == "" || manifestPath == "" {
		t.Skip("SHIMMY_PYTHON_RUNTIME_WASM and SHIMMY_PYTHON_RUNTIME_MANIFEST are required")
	}

	scriptPath := filepath.Join(t.TempDir(), "single-use.py")
	script := `
_counter = 0

def evaluation_function(response, answer, params):
    global _counter
    _counter += 1
    return {"counter": _counter, "is_correct": response == answer}
`
	require.NoError(t, os.WriteFile(scriptPath, []byte(script), 0o644))

	dispatcher := NewPythonReactorDispatcher(Config{
		ModulePath:                wasmPath,
		PythonReactorManifestPath: manifestPath,
		PythonScriptPath:          scriptPath,
		PythonLifecycle:           "single-use",
		PythonPreparedCapacity:    1,
		MaxInstances:              1,
		MaxMemoryPages:            8192,
		Timeout:                   120 * time.Second,
	}, zap.NewNop())
	startContext, startCancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer startCancel()
	require.NoError(t, dispatcher.Start(startContext))
	t.Cleanup(func() {
		shutdownContext, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = dispatcher.Shutdown(shutdownContext)
	})

	health, err := dispatcher.Send(context.Background(), "healthcheck", nil)
	require.NoError(t, err)
	assert.Equal(t, "single-use", health["result"].(map[string]any)["lifecycle"])
	assert.Equal(t, 1, health["result"].(map[string]any)["prepared_ready"])
	assert.Equal(t, "single-use-prepared", health["result"].(map[string]any)["reset_mode"])

	first, err := dispatcher.Send(context.Background(), "eval", map[string]any{"response": "42", "answer": "42"})
	require.NoError(t, err)
	assert.Equal(t, float64(1), first["result"].(map[string]any)["counter"])

	// The hit starts a slow background refill. An immediate next request must not
	// wait for it; it initializes one fresh single-use fallback synchronously.
	second, err := dispatcher.Send(context.Background(), "eval", map[string]any{"response": "42", "answer": "42"})
	require.NoError(t, err)
	assert.Equal(t, float64(1), second["result"].(map[string]any)["counter"])

	require.Eventually(t, func() bool {
		health, err = dispatcher.Send(context.Background(), "healthcheck", nil)
		if err != nil {
			return false
		}
		state := health["result"].(map[string]any)
		return state["prepared_ready"] == 1 && state["prepared_refills"] == uint64(1)
	}, 2*time.Minute, 100*time.Millisecond)

	health, err = dispatcher.Send(context.Background(), "healthcheck", nil)
	require.NoError(t, err)
	state := health["result"].(map[string]any)
	assert.Equal(t, uint64(1), state["prepared_hits"])
	assert.Equal(t, uint64(1), state["prepared_misses"])

	third, err := dispatcher.Send(context.Background(), "eval", map[string]any{"response": "42", "answer": "42"})
	require.NoError(t, err)
	assert.Equal(t, float64(1), third["result"].(map[string]any)["counter"])

	health, err = dispatcher.Send(context.Background(), "healthcheck", nil)
	require.NoError(t, err)
	assert.Equal(t, uint64(2), health["result"].(map[string]any)["prepared_hits"])
}

func TestPythonReactorDispatcherTimeoutDoesNotPoisonRuntime(t *testing.T) {
	wasmPath := os.Getenv("SHIMMY_PYTHON_RUNTIME_WASM")
	manifestPath := os.Getenv("SHIMMY_PYTHON_RUNTIME_MANIFEST")
	if wasmPath == "" || manifestPath == "" {
		t.Skip("SHIMMY_PYTHON_RUNTIME_WASM and SHIMMY_PYTHON_RUNTIME_MANIFEST are required")
	}

	scriptPath := filepath.Join(t.TempDir(), "timeout.py")
	script := `
def evaluation_function(response, answer, params):
    if response == "loop":
        while True:
            pass
    return {"is_correct": response == answer}
`
	require.NoError(t, os.WriteFile(scriptPath, []byte(script), 0o644))

	dispatcher := NewPythonReactorDispatcher(Config{
		ModulePath:                wasmPath,
		PythonReactorManifestPath: manifestPath,
		PythonScriptPath:          scriptPath,
		MaxInstances:              1,
		MaxMemoryPages:            8192,
		Timeout:                   12 * time.Second,
	}, zap.NewNop())
	startContext, startCancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer startCancel()
	require.NoError(t, dispatcher.Start(startContext))
	t.Cleanup(func() {
		shutdownContext, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = dispatcher.Shutdown(shutdownContext)
	})

	_, err := dispatcher.Send(context.Background(), "eval", map[string]any{"response": "loop", "answer": "x"})
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)

	after, err := dispatcher.Send(context.Background(), "eval", map[string]any{"response": "42", "answer": "42"})
	require.NoError(t, err)
	assert.Equal(t, true, after["result"].(map[string]any)["is_correct"])
}
