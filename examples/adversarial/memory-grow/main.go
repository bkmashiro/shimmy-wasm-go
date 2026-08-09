//go:build wasip1

// memory-grow adversarial module.
//
// Tests that the host's WithMemoryLimitPages cap is enforced at the WASM
// memory.grow instruction boundary. The module asks Go's runtime to grow
// linear memory in 1-page (64 KB) chunks; once the host-configured page
// limit is reached, the runtime panics and the sandbox traps.
//
// Unlike mem-bomb (which allocates all-at-once), this module grows
// incrementally and reports how many pages were successfully grown before
// the limit was hit, giving a quantitative measure of the enforcement.
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

	// Grow linear memory one page (64 KB) at a time.
	// We expect the host-configured memory limit (WithMemoryLimitPages) to
	// trap this loop before we reach 1 GB (16384 pages).
	//
	// Each iteration allocates a fresh 64 KB slice (one WASM page) and touches
	// it to prevent the optimizer from eliding the allocation.
	grewPages := 0
	blocked := false

	func() {
		defer func() {
			if r := recover(); r != nil {
				blocked = true
			}
		}()
		// Keep all chunks alive so the GC cannot reclaim them — otherwise the
		// Go runtime inside WASM recycles pages between iterations and the
		// memory.grow limit is never actually reached.
		live := make([][]byte, 0, 16384)
		for i := 0; i < 16384; i++ { // 16384 × 64KB = 1 GB
			chunk := make([]byte, 65536)
			chunk[0] = byte(i & 0xff)
			live = append(live, chunk)
			grewPages++
		}
		_ = live
	}()

	detail := "grew_pages before limit"
	if !blocked {
		detail = "WARNING: memory.grow was never blocked — limit may not be enforced"
	}

	result := map[string]any{
		"attack":     "memory-grow",
		"blocked":    blocked,
		"grew_pages": grewPages,
		"detail":     detail,
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
