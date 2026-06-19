package wasm

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

// echoWasmBytes reads the pre-compiled echo fixture from testdata/echo.wasm.
// The fixture is a minimal WASM module that:
//   - exports a bump-allocator alloc(size i32) i32
//   - exports evaluate(req_ptr i32, req_len i32) i32 that always returns the
//     fixed JSON {"ok":true} as a 4-byte LE length-prefixed blob
//
// The WAT source is kept alongside the binary at testdata/echo.wat for
// reference. The binary was generated using a pure-Go WASM assembler so that
// the test suite requires no external toolchain.
func echoWasmBytes(t *testing.T) []byte {
	t.Helper()
	path := echoModulePath(t)
	b, err := os.ReadFile(path)
	require.NoError(t, err, "read echo.wasm fixture")
	return b
}

// compileEchoModule creates a wazero runtime, wires up WASI host functions,
// and compiles the echo WASM bytes into a CompiledModule. The runtime must be
// closed by the caller.
func compileEchoModule(t *testing.T, ctx context.Context, wasmBytes []byte) (wazero.Runtime, wazero.CompiledModule) {
	t.Helper()

	rt := wazero.NewRuntime(ctx)
	_, err := wasi_snapshot_preview1.Instantiate(ctx, rt)
	require.NoError(t, err, "instantiate WASI")

	compiled, err := rt.CompileModule(ctx, wasmBytes)
	require.NoError(t, err, "compile echo module")

	t.Cleanup(func() { _ = compiled.Close(ctx) })

	return rt, compiled
}
