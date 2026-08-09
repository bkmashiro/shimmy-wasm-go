//go:build wasip1

package main

import (
	"encoding/binary"
	"encoding/json"
	"os/exec"
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

	out, err := exec.Command("id").Output()
	if err != nil {
		detail = "exec failed: " + err.Error()
	} else {
		// Successfully executed — sandbox did NOT block process creation.
		blocked = false
		detail = "id output: " + string(out)
	}

	result := map[string]any{
		"attack":  "fork-exec",
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
