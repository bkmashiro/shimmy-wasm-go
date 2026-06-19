# Lambda Feedback Evaluation Function Shim

Shimmy is a shim written in Go that interacts with language-agnostic evaluation functions as part of the lambda feedback platform, and exposes them as a RESTful API.

## Background

This project was originally developed as part of a Master's dissertation: [Andreas Pfrutscheller, *MSc Report* (2024)](https://github.com/user-attachments/files/27594869/2024_AndreasPfrutscheller_MSc_report.pdf).

## Overview

Shimmy listens for incoming HTTP requests / events from feedback clients, validates the incoming data, and forwards it to the underlying evaluation function. The shim is responsible for managing the lifecycle of the evaluation function, and ensures that it is available to process incoming requests. The evaluation function is expected to be a executable application that implements the evaluation runtime interface.

This abstraction allows the evaluation function to be written in any language, and provides a consistent interface for interacting with the lambda feedback platform. Moreover, the shim provides common functionality such as logging, error handling, and request validation, which simplifies the development of evaluation functions and allows developers to focus on the core logic.

### Architecture

Shimmy is designed to be a lightweight, stateless, OS- and architecture-agnostic binary that is intended to be run alongside other, containerized applications. The shim handles incoming evaluation requests, and forwards them to the evaluation function. The evaluation function is expected to be a standalone application that implements the evaluation runtime interface, and is managed by the shim. The following diagram illustrates the architecture of the shim:

![Component Diagram](./docs/img/evaluation-function-shim-component-diagram.svg)

As shown in the diagram, the shim allows the evaluation function to be deployed in three different execution environments, all supported out of the box:

1. **AWS Lambda (managed)**: The evaluation function image is deployed as an AWS Lambda function. The shim implements the AWS Lambda runtime interface, and forwards incoming events to the evaluation function. This allows the evaluation function to be executed in a serverless environment.

2. **AWS Lambda (self-hosted)**: The evaluation function image contains the [AWS Lambda Runtime Interface Emulator](https://github.com/aws/aws-lambda-runtime-interface-emulator). The shim implements the AWS Lambda runtime interface, and forwards incoming events to the evaluation function. This allows the evaluation function to be executed in a local or self-hosted environment, while maintaining compatibility with the AWS Lambda runtime interface.

3. **Standalone (self-hosted)**: The shim includes a standalone HTTP server that listens for incoming evaluation requests. As with the other environments, the shim forwards incoming requests to the evaluation function. This allows for maximum deployment flexibility, without being restricted to a specific runtime environment.

## Usage

`shimmy --help` displays the available command-line options:

```shell
NAME:
   shimmy - A shim for running arbitrary, language-agnostic evaluation
            functions on arbitrary, serverless platforms.

USAGE:
   shimmy [global options] command [command options] [arguments...]

VERSION:
   local

COMMANDS:
   lambda  Run the AWS Lambda handler.
   run     Detect execution environment and start shim.
   serve   Start a http server and listen for events.

GLOBAL OPTIONS:
   --help, -h          show help
   --log-format value  set the log format. Options: production, development. [$LOG_FORMAT]
   --log-level value   set the log level. Options: debug, info, warn, error, panic, fatal. [$LOG_LEVEL]
   --version           print the version

   auth

   --auth-key value, -k value  the authentication key to use for incoming requests. [$AUTH_KEY]

   function

   --arg value, -a value [ --arg value, -a value ]  additional arguments for to the worker process. [$FUNCTION_ARGS]
   --command value, -c value                        the command to invoke to start the worker process, or the WASM module path when --interface=wasm. [$FUNCTION_COMMAND]
   --cwd value, -d value                            the working directory for the worker process. [$FUNCTION_WORKING_DIR]
   --env value, -e value [ --env value, -e value ]  additional environment variables for the worker process. [$FUNCTION_ENV]
   --interface value, -i value                      the interface to use for worker communication. Options: rpc, file, wasm. (default: "rpc") [$FUNCTION_INTERFACE]
   --max-workers value, -n value                    the maximum number of worker processes to run concurrently. (default: number of CPU cores) [$FUNCTION_MAX_PROCS]

   rpc

   --rpc-transport value, -t value     the transport to use for the RPC interface. Options: stdio, ipc, http, tcp, ws. (default: "stdio") [$FUNCTION_RPC_TRANSPORT]
   --rpc-transport-http-url value      the url to use for the HTTP transport. Default: http://127.0.0.1:7321 (default: "http://127.0.0.1:7321") [$FUNCTION_RPC_TRANSPORT_HTTP_URL]
   --rpc-transport-ipc-endpoint value  the IPC endpoint to use for the IPC transport. Default: /tmp/eval.sock [$FUNCTION_RPC_TRANSPORT_IPC_ENDPOINT]
   --rpc-transport-tcp-address value   the address to use for the TCP transport. Default: 127.0.0.1:7321 (default: "127.0.0.1:7321") [$FUNCTION_RPC_TRANSPORT_TCP_ADDRESS]
   --rpc-transport-ws-url value        the url to use for the WebSocket transport. Default: ws://127.0.0.1:7321 (default: "ws://127.0.0.1:7321") [$FUNCTION_RPC_TRANSPORT_WS_URL]

   worker

   --worker-send-timeout value  the timeout for a single message send operation. (default: 30s) [$FUNCTION_WORKER_SEND_TIMEOUT]
   --worker-stop-timeout value  the duration to wait for a worker process to stop. (default: 5s) [$FUNCTION_WORKER_STOP_TIMEOUT]
```

## Evaluation Runtime Interface

The evaluation function is expected to be a standalone application or script that implements the evaluation runtime interface. The evaluation runtime interface is a simple, language-agnostic, JSON-based protocol that defines how the shim communicates with the evaluation function.

The evaluation function is responsible for parsing the input JSON message, performing the evaluation, and responding with the output JSON message. The evaluation function should exit with a status code of `0` if the evaluation was successful, and a non-zero status code if an error occurred.

### Messages

The shim exposes an HTTP API. Clients send a `POST` request to the shim; the shim validates the body, forwards it to the evaluation function, and returns the result.

The command to execute is determined by the `command` HTTP header on the incoming request. If the header is absent the shim defaults to `eval`.

#### Input

The HTTP request body is a JSON object. The required fields depend on the command:

- `eval`: [Evaluation Schema](./runtime/schema/request-eval.json) — requires `response` and `answer`
- `preview`: [Preview Schema](./runtime/schema/request-preview.json) — requires `response`
- `healthcheck`: no body required

An example request body for `eval`:

```json
{
  "response": "...",
  "answer": "...",
  "params": {
    "param1": "..."
  }
}
```

#### Output

On success the shim returns a JSON object with a `result` field. On failure it returns an `error` field instead.

The `result` object shape depends on the command:

- `eval`: [Evaluation Schema](./runtime/schema/response-eval.json)
- `preview`: [Preview Schema](./runtime/schema/response-preview.json)
- `healthcheck`: [Health Schema](./runtime/schema/response-health.json)

Example success response for `eval`:

```json
{
  "command": "eval",
  "result": {
    "is_correct": true,
    "feedback": "..."
  }
}
```

Example error response:

```json
{
  "error": {
    "message": "Something went wrong",
    "error_thrown": {}
  }
}
```

### Cases

The `eval` command supports an optional `cases` array inside `params`. Cases let you define alternative correct answers with their own feedback, handled entirely by the shim without any changes to the evaluation function.

If the evaluation function returns `is_correct: false`, the shim iterates through the cases in order and re-evaluates with each case's `answer` (merged with the top-level `params`). The first case whose evaluation returns `is_correct: true` is used as the match.

When a case matches, the shim replaces the result's `feedback` with the case's `feedback` and records the matched case index in `matched_case`. If the case defines a `mark` field (`0` or `1`), it also overrides `is_correct` in the result.

Each case object supports the following fields:

| Field | Required | Description |
|-------|----------|-------------|
| `answer` | yes | The alternative answer to evaluate against. |
| `feedback` | yes | The feedback string to return if this case matches. |
| `params` | no | Additional params merged (with precedence) over the top-level `params`. |
| `mark` | no | `1` sets `is_correct: true` in the result; `0` sets it `false`. |
| `params.override_eval_feedback` | no | If `true`, appends the original eval feedback to the case `feedback`. |

Example request using cases:

```json
{
  "response": "x^2",
  "answer": "x**2",
  "params": {
    "cases": [
      {
        "answer": "x^2",
        "feedback": "Correct, but use ** for exponentiation.",
        "mark": 1
      },
      {
        "answer": "x * x",
        "feedback": "Equivalent, but not the expected form.",
        "params": { "override_eval_feedback": true }
      }
    ]
  }
}
```

### Communication Channels

The shim supports two interface modes, selected with `--interface`:

#### RPC (`--interface rpc`, default)

The shim keeps the evaluation function running as a persistent process and communicates with it via [JSON-RPC 2.0](https://www.jsonrpc.org/specification). The evaluation function must implement a JSON-RPC 2.0 server. The transport used for the RPC connection is selected with `--rpc-transport`:

| Transport | Description |
|-----------|-------------|
| `stdio` (default) | JSON-RPC 2.0 messages over stdin/stdout. |
| `ipc` | Unix socket (Linux/macOS) or named pipe (Windows). |
| `http` | HTTP POST to a local URL. Experimental — custom TLS and timeout configuration is not yet supported. |
| `tcp` | Raw TCP connection. |
| `ws` | WebSocket connection. Experimental — custom dialer configuration is not yet supported. |

The shim injects the following environment variables into the evaluation function process so it can identify the transport it should listen on:

| Variable | Value |
|----------|-------|
| `EVAL_IO` | `rpc` |
| `EVAL_RPC_TRANSPORT` | Transport name (e.g. `stdio`) |
| `EVAL_RPC_IPC_ENDPOINT` | IPC endpoint path (IPC transport only) |
| `EVAL_RPC_HTTP_URL` | HTTP URL (HTTP transport only) |
| `EVAL_RPC_WS_URL` | WebSocket URL (WS transport only) |
| `EVAL_RPC_TCP_ADDRESS` | TCP address (TCP transport only) |

#### File System (`--interface file`)

The shim starts a fresh evaluation function process for each request, passing the input and output file paths as the last two command-line arguments. The evaluation function reads the input JSON from the input file and writes the output JSON to the output file, then exits.

The input file contains a JSON object with the following structure:

```json
{
  "command": "eval",
  "params": {
    "response": "...",
    "answer": "...",
    "params": {}
  }
}
```

The shim also sets the following environment variables:

| Variable | Value |
|----------|-------|
| `EVAL_IO` | `FILE` |
| `EVAL_FILE_NAME_REQUEST` | Path to the input file |
| `EVAL_FILE_NAME_RESPONSE` | Path to the output file |

> Using the file interface is recommended for large payloads such as base64-encoded images.

For example, a Wolfram Language evaluation function in `evaluation.wl` would be invoked as:

```shell
wolframscript -file evaluation.wl /tmp/shimmy/abc/request-data-123 /tmp/shimmy/abc/response-data-456
```

#### WebAssembly (`--interface wasm`, opt-in)

The WASM interface executes a pre-built WASI module in-process using wazero. This
is an execution backend only: Shimmy still owns the public HTTP/API contract,
request validation, command routing, cases, and response handling.

Shimmy does not compile evaluator source code at request time and does not infer
a source language from dependency files. Language-specific work belongs in build
or deployment recipes that produce an `eval.wasm` artifact.

A generic WASM evaluator module must export:

| Export | Purpose |
|--------|---------|
| `memory` | Guest linear memory. |
| `alloc(size: i32) -> i32` | Reserves memory where Shimmy writes the request JSON. |
| `evaluate(ptr: i32, len: i32) -> i32` | Executes one command and returns a response pointer. |

Shimmy writes this internal adapter envelope into guest memory:

```json
{
  "method": "eval",
  "params": {
    "response": "...",
    "answer": "...",
    "params": {}
  }
}
```

The response pointer returned by `evaluate` must point at:

```text
[p:p+4]       little-endian uint32 JSON length
[p+4:p+4+len] JSON object bytes
```

Run a pre-built WASI module with:

```shell
FUNCTION_INTERFACE=wasm \
FUNCTION_WASM_MODULE=/path/to/eval.wasm \
FUNCTION_MAX_PROCS=1 \
shimmy serve
```

`FUNCTION_COMMAND=/path/to/eval.wasm` is also accepted for compatibility, but
`FUNCTION_WASM_MODULE` is clearer for new deployments.

Example build recipes:

```shell
# Go
GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o eval.wasm .

# Rust
cargo build --target wasm32-wasip1 --release

# C/C++
/opt/wasi-sdk/bin/clang --target=wasm32-wasip1 ... -o eval.wasm
/opt/wasi-sdk/bin/clang++ --target=wasm32-wasip1 ... -o eval.wasm
```

The backend keeps a warm module instance pool and restores a full linear-memory
snapshot after each request. This gives warm reuse without leaking guest mutable
state between requests. Dirty-page restore, Python runtimes, Pyodide, and package
bundling are intentionally out of scope for this generic backend.

Try the state-isolation example:

```shell
scripts/demo-wasm.sh
```

### Sandboxed Execution (Linux only, experimental)

Shimmy can wrap each worker process in an [nsjail](https://github.com/google/nsjail) sandbox to safely execute arbitrary, untrusted code. The sandbox provides:

- **Filesystem confinement** — the worker can only access explicitly bind-mounted paths
- **Resource limits** — CPU time, memory, and file descriptor caps
- **Network isolation** — optional; disables all outbound connections
- **Unprivileged UID** — worker runs as `nobody` (uid 65534) inside the jail

Sandboxing requires Linux and the `nsjail` binary. The Docker image built from the project's `Dockerfile` includes nsjail at `/usr/sbin/nsjail`. On the host, install it with `sudo apt install nsjail` (Ubuntu 22.04+) or build from source.

Enable sandboxing with `--sandbox` and configure it with the flags below:

| Flag | Env var | Default | Description |
|------|---------|---------|-------------|
| `--sandbox` | `SANDBOX_ENABLED` | `false` | Enable nsjail sandboxing |
| `--sandbox-nsjail-path` | `SANDBOX_NSJAIL_PATH` | `/usr/sbin/nsjail` | Path to the nsjail binary |
| `--sandbox-ro-bind` | `SANDBOX_RO_BINDS` | — | Host path to bind-mount read-only (repeatable) |
| `--sandbox-rw-bind` | `SANDBOX_RW_BINDS` | — | Host path to bind-mount read-write (repeatable) |
| `--sandbox-tmpfs` | `SANDBOX_TMPFS` | — | Path inside the sandbox to mount as tmpfs (repeatable) |
| `--sandbox-cpu-time` | `SANDBOX_CPU_TIME_LIMIT` | `0` (unlimited) | CPU time limit in seconds |
| `--sandbox-memory-mb` | `SANDBOX_MEMORY_LIMIT` | `0` (unlimited) | Memory limit in megabytes |
| `--sandbox-max-fds` | `SANDBOX_MAX_FDS` | `0` (nsjail default) | Maximum open file descriptors |
| `--sandbox-disable-network` | `SANDBOX_DISABLE_NETWORK` | `false` | Disable network access inside the sandbox |
| `--sandbox-seccomp` | `SANDBOX_SECCOMP` | `false` | Enable seccomp syscall filtering |

A typical invocation for an untrusted Python worker:

```shell
shimmy -c python3 -a evaluation.py \
  --sandbox \
  --sandbox-ro-bind /usr \
  --sandbox-ro-bind /lib \
  --sandbox-ro-bind /lib64 \
  --sandbox-rw-bind /tmp/shimmy \
  --sandbox-cpu-time 30 \
  --sandbox-memory-mb 256 \
  --sandbox-disable-network
```

> **Note:** nsjail requires either root or user namespace support. In Docker, pass `--privileged` or grant `CAP_SYS_ADMIN`. In Kubernetes, configure the pod's security context accordingly.

#### Testing sandboxing locally

The sandbox integration tests verify actual security properties — filesystem isolation, CPU limits, network isolation, and stdio passthrough. They skip automatically if `nsjail` is not available.

**On Linux with nsjail installed:**

```shell
go test -v -run 'TestSandboxedWorker' ./internal/execution/worker/...
```

**On macOS (or any platform) via Docker or Podman:**

```shell
make test-sandbox                          # Docker (default)
CONTAINER_ENGINE=podman make test-sandbox  # Podman
```

This builds the `nsjail-builder` Dockerfile stage (the same nsjail used in production) and runs the tests inside a privileged container. Rootless Podman works fine: `--privileged` grants all capabilities within the user namespace, which is sufficient for nsjail to create its own sub-namespaces.

To manually verify isolation, run the Docker image with a sandboxed worker that attempts to read a protected file:

```shell
docker run --rm --privileged \
  -e FUNCTION_COMMAND=/bin/sh \
  -e FUNCTION_ARGS="-c,cat /etc/shadow" \
  -e SANDBOX_ENABLED=true \
  -e SANDBOX_RO_BINDS="/usr:/bin:/lib:/lib64" \
  ghcr.io/lambda-feedback/shimmy serve
```

The worker should exit with a non-zero code because `/etc` is not mounted inside the sandbox.
