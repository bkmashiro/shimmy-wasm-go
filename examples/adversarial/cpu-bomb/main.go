//go:build wasip1

package main

import (
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

	// Infinite loop — intentionally never returns.
	// The host test verifies that the context timeout (epoch interruption) kills this.
	var i uint64
	for {
		i++
		_ = i
	}
}

func main() {}
