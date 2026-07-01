// Package wasm implements a WebAssembly execution backend for shimmy using
// wazero. It exposes a [Dispatcher] that manages a pool of pre-compiled WASM
// module instances and dispatches evaluation requests to them.
//
// # Guest ABI
//
// WASM modules loaded by this backend must export two functions:
//
//	alloc(size i32) i32
//	    Allocate `size` bytes in guest linear memory and return a pointer to
//	    the start of the allocation. The host will write the JSON-encoded
//	    request into this region immediately after the call returns.
//
//	evaluate(req_ptr i32, req_len i32) i32
//	    Process the JSON request at [req_ptr, req_ptr+req_len). Returns a
//	    pointer P into guest memory where the response is encoded as:
//	        bytes [P, P+4)   — uint32 little-endian response length L
//	        bytes [P+4, P+4+L) — L bytes of UTF-8 JSON response
//
// The JSON request envelope has the shape:
//
//	{"method": "<method>", "params": {…}}
//
// The JSON response is a plain JSON object (map[string]any) that is returned
// verbatim to the caller.
package wasm

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"time"

	"github.com/tetratelabs/wazero/api"
	"go.uber.org/zap"
)

// requestEnvelope is the JSON structure written into guest memory for each
// evaluation call.
type requestEnvelope struct {
	Method string         `json:"method"`
	Params map[string]any `json:"params"`
}

// wasmAdapter performs a single evaluate call against a live wazero api.Module.
// It is stateless and safe to call from one goroutine at a time.
type wasmAdapter struct {
	mod     api.Module
	log     *zap.Logger
	allocFn api.Function // cached exported "alloc" function (M-4 fix)
	evalFn  api.Function // cached exported "evaluate" function (M-4 fix)
}

func newWasmAdapter(mod api.Module, log *zap.Logger) *wasmAdapter {
	return &wasmAdapter{
		mod:     mod,
		log:     log.Named("adapter_wasm"),
		allocFn: mod.ExportedFunction("alloc"),
		evalFn:  mod.ExportedFunction("evaluate"),
	}
}

// send marshals (method, data) into JSON, writes it into the guest's linear
// memory via alloc, calls evaluate, and reads back the length-prefixed
// response.
func (a *wasmAdapter) send(
	ctx context.Context,
	method string,
	data map[string]any,
	timeout time.Duration,
) (map[string]any, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	// 1. Marshal request envelope.
	envelope := requestEnvelope{Method: method, Params: data}

	reqBytes, err := json.Marshal(envelope)
	if err != nil {
		return nil, fmt.Errorf("wasm: marshal request: %w", err)
	}

	reqLen := uint64(len(reqBytes))

	// 2. Allocate guest memory for the request (cached lookup — M-4 fix).
	if a.allocFn == nil {
		return nil, fmt.Errorf("wasm: guest module does not export 'alloc'")
	}

	allocRes, err := a.allocFn.Call(ctx, reqLen)
	if err != nil {
		return nil, fmt.Errorf("wasm: alloc(%d): %w", reqLen, err)
	}
	if len(allocRes) != 1 {
		return nil, fmt.Errorf("wasm: alloc returned %d values, expected 1", len(allocRes))
	}

	reqPtr := allocRes[0]
	if reqPtr == 0 {
		return nil, fmt.Errorf("wasm: alloc returned NULL (out of memory)")
	}

	// 3. Write request bytes into guest memory.
	mem := a.mod.Memory()
	if mem == nil {
		return nil, fmt.Errorf("wasm: guest module has no linear memory")
	}

	if !mem.Write(uint32(reqPtr), reqBytes) {
		return nil, fmt.Errorf(
			"wasm: failed to write %d bytes at ptr=%d (memory size=%d)",
			len(reqBytes), reqPtr, mem.Size(),
		)
	}

	// 4. Call evaluate (cached lookup — M-4 fix).
	if a.evalFn == nil {
		return nil, fmt.Errorf("wasm: guest module does not export 'evaluate'")
	}

	a.log.Debug("calling evaluate",
		zap.String("method", method),
		zap.Uint64("req_ptr", reqPtr),
		zap.Uint64("req_len", reqLen),
	)

	evalRes, err := a.evalFn.Call(ctx, reqPtr, reqLen)
	if err != nil {
		return nil, fmt.Errorf("wasm: evaluate: %w", err)
	}
	if len(evalRes) != 1 {
		return nil, fmt.Errorf("wasm: evaluate returned %d values, expected 1", len(evalRes))
	}

	resPtr := uint32(evalRes[0])

	// 5. Read the 4-byte little-endian length prefix.
	lenBytes, ok := mem.Read(resPtr, 4)
	if !ok {
		return nil, fmt.Errorf("wasm: failed to read response length at ptr=%d", resPtr)
	}

	resLen := binary.LittleEndian.Uint32(lenBytes)

	// 6. Read the response JSON body.
	// Validate bounds before reading to catch corrupt/malicious response pointers.
	if uint64(resPtr)+4+uint64(resLen) > uint64(mem.Size()) {
		return nil, fmt.Errorf(
			"wasm: response out of bounds: resPtr=%d resLen=%d memSize=%d",
			resPtr, resLen, mem.Size(),
		)
	}
	resBytes, ok := mem.Read(resPtr+4, resLen)
	if !ok {
		return nil, fmt.Errorf(
			"wasm: failed to read %d response bytes at ptr=%d",
			resLen, resPtr+4,
		)
	}

	a.log.Debug("received response",
		zap.Uint32("res_ptr", resPtr),
		zap.Uint32("res_len", resLen),
	)

	// 7. Unmarshal response.
	var result map[string]any
	if err := json.Unmarshal(resBytes, &result); err != nil {
		return nil, fmt.Errorf("wasm: unmarshal response: %w", err)
	}

	return result, nil
}
