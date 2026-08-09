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

// recurse calls itself infinitely — never returns.
func recurse(n int) int {
	return recurse(n+1) + 1
}

//go:wasmexport dispatch
func dispatch(reqPtr int32, reqLen int32) int32 {
	_ = reqPtr
	_ = reqLen

	// Infinite recursion — intentionally never returns.
	// The host test verifies that the context timeout (epoch interruption) kills this.
	_ = recurse(0)
	return int32(uintptr(unsafe.Pointer(&respBuf[0])))
}

func main() {}
