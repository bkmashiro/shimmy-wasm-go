package supervisor

import (
	"time"

	"github.com/lambda-feedback/shimmy/internal/execution/worker"
)

// StartConfig describes the configuration for starting the worker.
type StartConfig = worker.StartConfig

// StopConfig describes the configuration for stopping the worker.
type StopConfig struct {
	// Timeout is the timeout for stopping the worker.
	Timeout time.Duration
}

// SendConfig describes the configuration for sending messages to the worker.
type SendConfig struct {
	// Timeout is the timeout for sending a message to the worker.
	Timeout time.Duration
}

// IOInterface describes the interface used to communicate with the worker.
type IOConfig struct {
	// Interface describes the communication between the supervisor
	// and the worker. It can be "rpc", "file", or "wasm".
	//
	// If "rpc", the supervisor will communicate with the worker over
	// a specified transport. The worker is expected to handle incoming
	// messages from the supervisor and send responses back.
	//
	// If "file", the supervisor will communicate with the worker over
	// files. Only valid for transient workers. The name of the files
	// containing the message payload and response are passed as args
	// to the worker process.
	//
	// If "wasm", Shimmy loads a pre-built WASI module from FUNCTION_COMMAND
	// or FUNCTION_WASM_MODULE and calls its internal alloc/evaluate adapter ABI.
	//
	// Default is "rpc".
	Interface IOInterface `conf:"interface"`

	// Rpc is the configuration for the rpc interface.
	Rpc RpcConfig `conf:"rpc"`
}

type Config struct {
	// IO is the IO config to use for the worker.
	IO IOConfig `conf:"io"`

	// StartParams are the parameters to pass to the worker when
	// starting it. This can be used to pass configuration to the worker.
	StartParams StartConfig `conf:"start,squash"`

	// StopParams are the parameters to pass to the worker when
	// terminating it.
	StopParams StopConfig `conf:"stop"`

	// SendParams are the parameters to pass to the worker when
	// sending a message.
	SendParams SendConfig `conf:"send"`

	// Sandbox holds the nsjail sandbox configuration. When Sandbox.Enabled
	// is false (the default), behaviour is identical to an unsandboxed worker.
	Sandbox worker.SandboxConfig `conf:"sandbox"`
}
