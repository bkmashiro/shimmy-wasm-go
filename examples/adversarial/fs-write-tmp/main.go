//go:build wasip1

package main

import (
	"encoding/binary"
	"encoding/json"
	"os"
	"unsafe"
)

var reqBuf [256 * 1024]byte
var respBuf [256 * 1024]byte

//go:wasmexport alloc
func alloc(size int32) int32 {
	_ = size
	return int32(uintptr(unsafe.Pointer(&reqBuf[0])))
}

//go:wasmexport dispatch
func dispatch(reqPtr int32, reqLen int32) int32 {
	_ = reqPtr
	_ = reqLen

	blocked := true
	detail := ""

	err := os.WriteFile("/tmp/evil-shimmy-test", []byte("pwned"), 0644)
	if err != nil {
		detail = "write failed: " + err.Error()
	} else {
		// Successfully wrote — sandbox did NOT block this.
		blocked = false
		detail = "file written to /tmp/evil-shimmy-test"
	}

	result := map[string]any{
		"attack":  "fs-write-tmp",
		"blocked": blocked,
		"detail":  detail,
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
