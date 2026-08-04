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
   --command value, -c value                        the command to invoke to start the worker process. [$FUNCTION_COMMAND]
   --cwd value, -d value                            the working directory for the worker process. [$FUNCTION_WORKING_DIR]
   --env value, -e value [ --env value, -e value ]  additional environment variables for the worker process. [$FUNCTION_ENV]
   --interface value, -i value                      the execution interface. Options: rpc, file, wasm, pyodide; reactor-python and python-wasm are compatibility aliases. (default: "rpc") [$FUNCTION_INTERFACE]
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

### Runtime support tiers

Shimmy's product surface is organized by support role. This does not silently
change the historical `rpc` default; deployments select a runtime explicitly.

| Tier | Paths | Selection |
|---|---|---|
| Primary | Generic WASM; Python reactor profile | `FUNCTION_INTERFACE=wasm`, with `FUNCTION_WASM_PROFILE=generic` or `python-reactor` |
| Migration | `file`; `rpc`; Pyodide | `FUNCTION_INTERFACE=file`, `rpc`, or `pyodide` |
| Fallback | Native + DynamoRIO; full Linux via QEMU | Keep `file`/`rpc` and enable exactly one explicit wrapper |
| Optional memory strategy | COW; UFFD; soft-dirty; mprotect | Explicit WASM snapshot configuration; full-copy remains default |

The Python-reactor interface remains a primary architecture path, but its legacy
artifact pin is frozen historical evidence while a clean replacement is pending.
Do not use that pin as a source for new deployments.

Shimmy never scans evaluator imports, `requirements.txt`, or source extensions to
choose a backend. Environment variables configure an already deployed runtime;
they do not download runtime images, Node/Pyodide, DBI clients, or QEMU artifacts.
QEMU and DynamoRIO are mutually exclusive terminal wrappers, not evaluator
interfaces, and Shimmy never retries a failed native/DBI request under QEMU.

See the [source-level architecture guide](https://bkmashiro.github.io/shimmy-docs/architecture/)
for the common control plane and detailed
[WASM/wazero/memory](https://bkmashiro.github.io/shimmy-docs/architecture/wasm-wazero-memory.html),
[DBI/native](https://bkmashiro.github.io/shimmy-docs/architecture/dbi-native-path.html), and
[QEMU/guest lifecycle](https://bkmashiro.github.io/shimmy-docs/architecture/qemu-guest-path.html)
paths. See
[the backend model](docs/wasm-backend-model.md) for support classification and
[deployment recipes](docs/deployment-recipes.md) for variables and artifact
status. The [QEMU evidence JSON](docs/qemu-fallback-evidence.json) is a
version-bound **PARTIAL** qualification record, not proof for arbitrary current
HEADs. For a compact four-lane demonstration, use the
[Docker Compose stack](demo/compose/README.md).

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

The shim supports several interface modes, selected with `--interface`. The
important naming rule for the WASM work is that `--interface` names the execution
boundary, not the evaluator source language: avoid adding one mode per language
such as `rust-wasm`, `go-wasm`, or `js-wasm`. See
[`docs/wasm-backend-model.md`](docs/wasm-backend-model.md) for the runtime
interface / WASM profile / build recipe split, and
[`docs/deployment-recipes.md`](docs/deployment-recipes.md) for the recommended
`FUNCTION_*` environment combinations.

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

#### WASM (`--interface wasm`)

The generic WASM backend consumes an already-built `.wasm` module that exposes
Shimmy's `alloc` / `evaluate` ABI. Shimmy does not compile source code at request
time and should not infer a runtime from language imports or dependency files.

Language-specific work belongs in build/deployment recipes:

- Go: `GOOS=wasip1 GOARCH=wasm go build ... -o eval.wasm`
- Rust: `cargo build --target wasm32-wasip1` with a Shimmy ABI wrapper
- C/C++: WASI SDK / clang targeting `wasm32-wasip1`
- Python: CPython-WASI reactor or Pyodide compatibility lane, depending on package needs
- JavaScript: future Javy/QuickJS-to-WASI integration with a Shimmy ABI adapter

For Python fast-path deployments, prefer `FUNCTION_INTERFACE=wasm` plus
`FUNCTION_WASM_PROFILE=agent-python`, a manifest-bound Agent Python Runtime
artifact, and an evaluator script or startup bundle. `python-reactor`,
`reactor-python`, and `FUNCTION_INTERFACE=reactor-python` remain configuration
aliases, not separate legacy loaders.
