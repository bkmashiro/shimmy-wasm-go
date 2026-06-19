package supervisor

import "errors"

var (
	ErrUnsupportedIOInterface = errors.New("unsupported io interface")
	ErrUnsupportedIOTransport = errors.New("unsupported io transport")
)

// IOInterface describes the interface used to communicate with the worker
type IOInterface string

const (
	// RpcIO describes communication w/ processes over rpc
	RpcIO IOInterface = "rpc"

	// FileIO describes communication w/ processes over files
	FileIO IOInterface = "file"

	// WasmIO describes in-process execution of a pre-built WASI module.
	WasmIO IOInterface = "wasm"
)

// IOTransport describes the transport mechanism used to communicate with
type IOTransport string

const (
	// IpcTransport describes communication w/ processes over IPC.
	// This can be unix sockets or windows named pipes, depending on the OS.
	IpcTransport IOTransport = "ipc"

	// Http describes communication w/ processes over http
	HttpTransport IOTransport = "http"

	// Stdio describes communication w/ processes over stdio
	StdioTransport IOTransport = "stdio"

	// Ws describes communication w/ processes over websockets
	WsTransport IOTransport = "ws"

	// Tcp describes communication w/ processes over tcp
	TcpTransport IOTransport = "tcp"
)
