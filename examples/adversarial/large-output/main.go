//go:build wasip1

package main

import (
	"encoding/binary"
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

	// Fill respBuf with a large JSON-like payload.
	// respBuf is 256KB; we'll write a payload that fills as much as possible.
	// Format: 4-byte LE length + JSON body
	// Body: {"attack":"large-output","blocked":false,"data":"AAAA..."}
	// We'll hand-craft the buffer to maximize size.

	prefix := []byte(`{"attack":"large-output","blocked":false,"detail":"`)
	suffix := []byte(`"}`)

	// Available space for data content (minus 4-byte header, prefix, suffix).
	available := len(respBuf) - 4 - len(prefix) - len(suffix)
	if available < 0 {
		available = 0
	}

	bodyLen := len(prefix) + available + len(suffix)
	binary.LittleEndian.PutUint32(respBuf[:4], uint32(bodyLen))

	offset := 4
	copy(respBuf[offset:], prefix)
	offset += len(prefix)

	// Fill with 'A' characters.
	for i := 0; i < available; i++ {
		respBuf[offset+i] = 'A'
	}
	offset += available

	copy(respBuf[offset:], suffix)

	return int32(uintptr(unsafe.Pointer(&respBuf[0])))
}

func main() {}
