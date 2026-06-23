//go:build wasip1

package main

import (
	"encoding/binary"
	"encoding/json"
	"unsafe"

	"demo-go-package/internal/compare"
)

var reqBuf [256 * 1024]byte
var respBuf [256 * 1024]byte
var invocationCount uint32

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
	if req.Method != "eval" {
		writeResp(map[string]any{"error": map[string]any{"message": "unsupported method"}})
		return int32(uintptr(unsafe.Pointer(&respBuf[0])))
	}

	invocationCount++
	isCorrect := compare.IsCorrect(req.Params.Response, req.Params.Answer)
	correctFeedback, _ := req.Params.Params["correct_response_feedback"].(string)
	incorrectFeedback, _ := req.Params.Params["incorrect_response_feedback"].(string)
	feedback := compare.Feedback(isCorrect, correctFeedback, incorrectFeedback)
	writeResp(map[string]any{
		"command": "eval",
		"result": map[string]any{
			"is_correct":             isCorrect,
			"feedback":               feedback,
			"guest_invocation_count": invocationCount,
			"snapshot_isolation_ok":  invocationCount == 1,
		},
	})
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
