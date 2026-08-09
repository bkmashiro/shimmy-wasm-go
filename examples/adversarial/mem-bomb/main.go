//go:build wasip1

package main

import (
	"encoding/binary"
	"encoding/json"
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

	// Try to allocate increasingly large slices until we panic or exhaust memory.
	blocked := false
	detail := "allocated all memory without error"

	func() {
		defer func() {
			if r := recover(); r != nil {
				blocked = true
				detail = "panic during allocation: memory limit enforced"
			}
		}()

		// Attempt to allocate 1GB in chunks — wazero memory limit is 16MB by default.
		const chunkSize = 1 * 1024 * 1024 // 1MB
		var chunks [][]byte
		for i := 0; i < 1024; i++ {
			chunk := make([]byte, chunkSize)
			// Touch memory so it can't be optimized away.
			chunk[0] = byte(i)
			chunk[chunkSize-1] = byte(i)
			chunks = append(chunks, chunk)
		}
		// Prevent the compiler from eliding chunks entirely.
		_ = chunks
	}()

	result := map[string]any{
		"attack":  "mem-bomb",
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
