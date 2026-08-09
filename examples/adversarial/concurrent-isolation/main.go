//go:build wasip1

// concurrent-isolation adversarial module.
//
// Each dispatch call writes a "session ID" (parsed from the request) into a
// module-global variable, sleeps briefly, then reads it back and reports
// whether it still matches.
//
// When two instances of this module run concurrently in separate wazero module
// instances, their global state must be fully independent. A host test that
// runs two goroutines each calling a separate module instance verifies that
// linear memory isolation is maintained under concurrent execution.
package main

import (
	"encoding/binary"
	"encoding/json"
	"unsafe"
)

var reqBuf [256 * 1024]byte
var respBuf [256 * 1024]byte

// sessionID is a module-global — each module instance has its own copy.
var sessionID uint32

//go:wasmexport alloc
func alloc(size int32) int32 {
	_ = size
	return int32(uintptr(unsafe.Pointer(&reqBuf[0])))
}

//go:wasmexport dispatch
func dispatch(reqPtr int32, reqLen int32) int32 {
	rawJSON := reqBuf[:reqLen]

	var req struct {
		SessionID uint32 `json:"session_id"`
		Iters     int    `json:"iters"`
	}
	req.Iters = 100_000
	_ = json.Unmarshal(rawJSON, &req)

	// Store session ID in module global.
	sessionID = req.SessionID

	// Busy-work loop to create a window where concurrent execution could
	// observe the other instance's session ID if isolation were broken.
	sum := uint32(0)
	for i := 0; i < req.Iters; i++ {
		sum += uint32(i)
	}

	// Read back — must still match what we wrote.
	observed := sessionID
	isolated := observed == req.SessionID

	result := map[string]any{
		"attack":             "concurrent-isolation",
		"blocked":            isolated, // "blocked" == isolation held
		"session_id_written": req.SessionID,
		"session_id_read":    observed,
		"checksum":           sum,
		"detail": func() string {
			if isolated {
				return "session ID unchanged after busy loop — linear memory isolation holds"
			}
			return "session ID was overwritten by another instance — ISOLATION FAILURE"
		}(),
	}
	writeResp(result)
	return int32(uintptr(unsafe.Pointer(&respBuf[0])))
}

func writeResp(v map[string]any) {
	data, _ := json.Marshal(v)
	binary.LittleEndian.PutUint32(respBuf[:4], uint32(len(data)))
	copy(respBuf[4:], data)
}

func main() {}
