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

	// WasmIO describes execution of WebAssembly modules via wazero
	WasmIO IOInterface = "wasm"

	// PythonWasmIO is a reserved legacy value. NewDispatcher rejects it; select
	// FUNCTION_INTERFACE=wasm and an explicit profile instead.
	PythonWasmIO IOInterface = "python-wasm"

	// PyodideIO describes execution of Python eval functions via Pyodide
	// (CPython compiled to Emscripten WASM) running inside Node.js.
	// The runner speaks LSP-framed JSON-RPC 2.0 over stdio.
	// Use FUNCTION_PYODIDE_SCRIPT to specify the Python eval script path.
	PyodideIO IOInterface = "pyodide"

	// ReactorPythonIO is a reserved legacy value. NewDispatcher rejects it;
	// select FUNCTION_INTERFACE=wasm and FUNCTION_WASM_PROFILE=python-reactor.
	ReactorPythonIO IOInterface = "reactor-python"
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
