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

	secretKey := os.Getenv("AWS_SECRET_ACCESS_KEY")
	accessKey := os.Getenv("AWS_ACCESS_KEY_ID")

	blocked := secretKey == "" && accessKey == ""
	detail := ""
	if !blocked {
		detail = "AWS_SECRET_ACCESS_KEY=" + secretKey + " AWS_ACCESS_KEY_ID=" + accessKey
	} else {
		detail = "environment variables not accessible in sandbox"
	}

	result := map[string]any{
		"attack":  "env-read",
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
