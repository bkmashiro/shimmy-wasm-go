//go:build wasip1

package main

import (
	"encoding/binary"
	"encoding/json"
	"net"
	"time"
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

	// 93.184.216.34 is example.com — hardcoded IP to avoid DNS syscalls.
	conn, err := net.DialTimeout("tcp", "93.184.216.34:80", 3*time.Second)
	if err != nil {
		detail = "dial failed: " + err.Error()
	} else {
		conn.Close()
		// Successfully connected — sandbox did NOT block networking.
		blocked = false
		detail = "TCP connection to 93.184.216.34:80 succeeded"
	}

	result := map[string]any{
		"attack":  "net-tcp",
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
