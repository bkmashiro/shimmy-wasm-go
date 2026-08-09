// Smoke tests for WASM eval function examples.
// Requires pre-built eval.wasm artifacts in each example directory.
// Run after CI build jobs have placed the artifacts:
//
//	go test ./examples/... -v

package examples_test

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

type evalRequest struct {
	Method string         `json:"method"`
	Params map[string]any `json:"params"`
}

type evalResult struct {
	Command string         `json:"command"`
	Result  map[string]any `json:"result"`
	Error   map[string]any `json:"error"`
}

func runWasmExample(t *testing.T, wasmPath string) {
	t.Helper()
	wasmBytes, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Skipf("wasm artifact not found (%s), skipping: %v", wasmPath, err)
	}

	ctx := context.Background()
	rt := wazero.NewRuntime(ctx)
	defer rt.Close(ctx)

	wasi_snapshot_preview1.MustInstantiate(ctx, rt)

	compiled, err := rt.CompileModule(ctx, wasmBytes)
	if err != nil {
		t.Fatalf("CompileModule: %v", err)
	}

	cfg := wazero.NewModuleConfig().WithName("eval").WithStartFunctions("_initialize")
	mod, err := rt.InstantiateModule(ctx, compiled, cfg)
	if err != nil {
		t.Fatalf("InstantiateModule: %v", err)
	}
	defer mod.Close(ctx)

	mem := mod.Memory()
	allocFn := mod.ExportedFunction("alloc")
	dispatchFn := mod.ExportedFunction("dispatch")

	call := func(t *testing.T, method string, params map[string]any) evalResult {
		t.Helper()
		req := evalRequest{Method: method, Params: params}
		reqBytes, _ := json.Marshal(req)

		// alloc
		res, err := allocFn.Call(ctx, uint64(len(reqBytes)))
		if err != nil {
			t.Fatalf("alloc: %v", err)
		}
		ptr := uint32(res[0])

		// write request into linear memory
		if !mem.Write(ptr, reqBytes) {
			t.Fatal("mem.Write failed")
		}

		// dispatch
		res, err = dispatchFn.Call(ctx, uint64(ptr), uint64(len(reqBytes)))
		if err != nil {
			t.Fatalf("dispatch: %v", err)
		}
		respPtr := uint32(res[0])

		// read 4-byte LE length prefix
		lenBytes, ok := mem.Read(respPtr, 4)
		if !ok {
			t.Fatal("mem.Read length failed")
		}
		respLen := binary.LittleEndian.Uint32(lenBytes)

		// read JSON body
		body, ok := mem.Read(respPtr+4, respLen)
		if !ok {
			t.Fatal("mem.Read body failed")
		}

		var result evalResult
		if err := json.Unmarshal(body, &result); err != nil {
			t.Fatalf("unmarshal response: %v\nbody: %s", err, body)
		}
		return result
	}

	t.Run("healthcheck", func(t *testing.T) {
		r := call(t, "healthcheck", nil)
		if r.Error != nil {
			t.Fatalf("unexpected error: %v", r.Error)
		}
		status, _ := r.Result["status"].(string)
		if status != "ok" {
			t.Fatalf("expected status=ok, got %q", status)
		}
	})

	t.Run("eval_correct", func(t *testing.T) {
		r := call(t, "eval", map[string]any{"response": "42", "answer": "42"})
		if r.Error != nil {
			t.Fatalf("unexpected error: %v", r.Error)
		}
		isCorrect, _ := r.Result["is_correct"].(bool)
		if !isCorrect {
			t.Fatalf("expected is_correct=true")
		}
	})

	t.Run("eval_incorrect", func(t *testing.T) {
		r := call(t, "eval", map[string]any{"response": "41", "answer": "42"})
		if r.Error != nil {
			t.Fatalf("unexpected error: %v", r.Error)
		}
		isCorrect, _ := r.Result["is_correct"].(bool)
		if isCorrect {
			t.Fatalf("expected is_correct=false")
		}
	})

	t.Run("preview", func(t *testing.T) {
		r := call(t, "preview", map[string]any{"response": "hello"})
		if r.Error != nil {
			t.Fatalf("unexpected error: %v", r.Error)
		}
	})

	t.Run("unknown_method", func(t *testing.T) {
		r := call(t, "bogus", nil)
		if r.Error == nil {
			t.Fatal("expected error for unknown method")
		}
	})
}

func TestEvalRust(t *testing.T) {
	runWasmExample(t, filepath.Join("eval-rust", "eval.wasm"))
}

func TestEvalC(t *testing.T) {
	runWasmExample(t, filepath.Join("eval-c", "eval.wasm"))
}

func TestEvalCpp(t *testing.T) {
	runWasmExample(t, filepath.Join("eval-cpp", "eval.wasm"))
}
