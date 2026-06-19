//go:build wasip1

// demo-stateful is a tiny Shimmy-WASM evaluation function for live demos.
// It intentionally mutates module-global state on every request. The host
// should snapshot/restore WASM memory after each call, so the counter reported
// to the next request should still be 1 rather than leaking as 2, 3, ...
package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"unsafe"
)

var reqBuf [256 * 1024]byte
var respBuf [256 * 1024]byte

// This is deliberately mutable guest state. A non-isolated warm worker would
// leak it across requests; Shimmy-WASM restores the memory snapshot instead.
var invocationCount uint32
var lastResponse [64]byte

//go:wasmexport alloc
func alloc(size int32) int32 {
	_ = size
	return int32(uintptr(unsafe.Pointer(&reqBuf[0])))
}

//go:wasmexport evaluate
func evaluate(reqPtr int32, reqLen int32) int32 {
	_ = reqPtr

	var req struct {
		Method string `json:"method"`
		Params struct {
			Response string         `json:"response"`
			Answer   string         `json:"answer"`
			Params   map[string]any `json:"params"`
		} `json:"params"`
	}

	if err := json.Unmarshal(reqBuf[:reqLen], &req); err != nil {
		writeResp(map[string]any{"error": map[string]any{"message": err.Error()}})
		return int32(uintptr(unsafe.Pointer(&respBuf[0])))
	}

	invocationCount++
	copy(lastResponse[:], req.Params.Response)

	switch req.Method {
	case "eval":
		correct := req.Params.Response == req.Params.Answer
		feedback := "Correct — and the guest counter is still 1, so snapshot/restore worked."
		if !correct {
			feedback = fmt.Sprintf("Incorrect: got %q, expected %q. Guest counter is still %d.", req.Params.Response, req.Params.Answer, invocationCount)
		}
		writeResp(map[string]any{
			"command": "eval",
			"result": map[string]any{
				"is_correct":             correct,
				"feedback":               feedback,
				"guest_invocation_count": invocationCount,
				"snapshot_isolation_ok":  invocationCount == 1,
			},
		})
	case "preview":
		writeResp(map[string]any{
			"command": "preview",
			"result": map[string]any{
				"preview": map[string]any{"type": "text", "content": req.Params.Response},
			},
		})
	case "healthcheck":
		writeResp(map[string]any{
			"command": "healthcheck",
			"result":  map[string]any{"status": "ok"},
		})
	default:
		writeResp(map[string]any{"error": map[string]any{"message": "unknown method: " + req.Method}})
	}

	return int32(uintptr(unsafe.Pointer(&respBuf[0])))
}

func writeResp(v map[string]any) {
	data, err := json.Marshal(v)
	if err != nil {
		data = []byte(`{"error":{"message":"marshal failed"}}`)
	}
	binary.LittleEndian.PutUint32(respBuf[:4], uint32(len(data)))
	copy(respBuf[4:], data)
}

func main() {}
