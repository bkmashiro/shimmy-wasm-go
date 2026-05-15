# shimmy-wasm

Fork of [lambda-feedback/shimmy](https://github.com/lambda-feedback/shimmy) where we integrate a WASM-based execution backend (wazero) alongside the existing subprocess-based approach.

Go module: `github.com/lambda-feedback/shimmy`  
Go version: 1.24.5

---

## Project Purpose

Shimmy is the **evaluation function shim** for Lambda Feedback. It wraps arbitrary evaluation functions (written in Python, Rust, etc.) behind a standardised HTTP API. The existing implementation runs evaluation code as **OS subprocesses**. This fork adds a **WASM execution path** using wazero, enabling:

- True sandboxing (no subprocess escapes, no filesystem access, no network)
- Memory snapshot/restore for cheap warm-starts
- N:1 multiplexing — many concurrent requests share one compiled WASM module
- Single static Go binary — no external runtime dependencies (except the Pyodide/Node.js fallback lane)

---

## Architecture

```
HTTP Request
    └─ Handler (chi router, /health, /:env/eval, /:env/docs)
        └─ RuntimeHandler
            └─ Dispatcher  ──── interface, two impls:
                ├─ DedicatedDispatcher  (RPC mode: one persistent worker)
                └─ PooledDispatcher     (File mode: worker pool via puddle)
                    └─ Supervisor  ──── manages one worker lifecycle
                        └─ Adapter  ──── speaks the IO protocol
                            └─ Worker   ──── os/exec subprocess (existing)
                                            WasmWorker (NEW — wazero module instance)
```

Dependency injection uses **uber-go/fx**. Most wiring is in `internal/execution/module.go`.

---

## Key Interfaces

### `runtime.Runtime` (`runtime/runtime.go`)
Top-level interface. Three commands (`run`, `serve`, `lambda`) each wire up a Runtime.

```go
type Runtime interface {
    Handle(context.Context, EvaluationRequest) (EvaluationResponse, error)
    Start(context.Context) error
    Shutdown(context.Context) error
}
```

### `dispatcher.Dispatcher` (`internal/execution/dispatcher/dispatcher.go`)
One dispatcher per evaluation function. Chosen at startup based on `FUNCTION_INTERFACE`.

```go
type Dispatcher interface {
    Send(context.Context, string, map[string]any) (map[string]any, error)
    Start(context.Context) error
    Shutdown(context.Context) error
}
```

### `supervisor.Supervisor` (`internal/execution/supervisor/supervisor.go`)
Manages the lifecycle of a single worker (or WASM module instance).

```go
type Supervisor interface {
    Start(ctx context.Context) error
    Send(ctx context.Context, method string, data map[string]any) (*Result, error)
    Suspend(ctx context.Context) (WaitFunc, error)
    Shutdown(ctx context.Context) (WaitFunc, error)
}
```

### `supervisor.Adapter` (`internal/execution/supervisor/adapter.go`)
Protocol bridge between Supervisor and Worker. Each IO interface (rpc, file, **wasm**) gets its own Adapter.

```go
type Adapter interface {
    Start(context.Context, worker.StartConfig) error
    Stop() (ReleaseFunc, error)
    Send(context.Context, string, map[string]any, time.Duration) (map[string]any, error)
}
```

### `worker.Worker` (`internal/execution/worker/worker.go`)
Represents a running subprocess (or WASM instance). Provides pipes for stdio communication.

```go
type Worker interface {
    Start(context.Context) error
    Stop() error
    Wait(context.Context) (ExitEvent, error)
    WaitFor(context.Context, time.Duration) (ExitEvent, error)
    ReadPipe() (io.ReadCloser, error)
    WritePipe() (io.WriteCloser, error)
    DuplexPipe() (io.ReadWriteCloser, error)
}
```

---

## Existing IO Interfaces

| Interface | Mode | Description |
|-----------|------|-------------|
| `rpc` | Persistent | JSON-RPC over stdio/IPC/HTTP/WS/TCP. One long-lived worker. |
| `file` | Transient | Request written to temp file, response read from temp file. Worker pool (puddle). |

The interface is selected via `FUNCTION_INTERFACE` env var.

---

## Configuration (Environment Variables)

| Variable | Default | Description |
|----------|---------|-------------|
| `FUNCTION_COMMAND` | *(required)* | Command to execute (e.g. `python3 eval.py`) |
| `FUNCTION_INTERFACE` | `rpc` | IO interface: `rpc`, `file`, or (new) `wasm` |
| `FUNCTION_RPC_TRANSPORT` | `stdio` | For rpc mode: `stdio`, `ipc`, `http`, `ws`, `tcp` |
| `FUNCTION_MAX_PROCS` | `1` | Max workers in pool (file mode / wasm pool) |
| `FUNCTION_TIMEOUT` | — | Per-request timeout |
| `AUTH_KEY` | — | Bearer token for the HTTP API |
| `PORT` | `8080` | HTTP listen port |
| `SENTRY_DSN` | — | Sentry error reporting |

---

## Build & Run

```bash
# Build
go build ./...

# Run locally (file mode example)
FUNCTION_COMMAND="python3 /path/to/eval.py" \
FUNCTION_INTERFACE=file \
go run . run

# Run as HTTP server
go run . serve

# Run as AWS Lambda handler
go run . lambda

# Tests
go test ./...
```

---

## WASM Integration Plan

### New IO Interface: `wasm`

Add `WasmIO IOInterface = "wasm"` to `internal/execution/io/io.go` (or equivalent constants file).

### New Package: `internal/execution/wasm/`

Implement the full stack for WASM execution:

```
internal/execution/wasm/
├── dispatcher.go   — WasmDispatcher (implements dispatcher.Dispatcher)
│                     Uses a wazero CompiledModule shared across instances
├── supervisor.go   — WasmSupervisor (implements supervisor.Supervisor)
│                     Manages one wazero ModuleInstance lifecycle
├── adapter.go      — WasmAdapter (implements supervisor.Adapter)
│                     Marshals method+data → WASM exported function call
│                     Unmarshals returned bytes → map[string]any
└── worker.go       — WasmWorker (implements worker.Worker, optional)
                      Wraps wazero ModuleInstance as a Worker if needed
```

### wazero Design Points

- **Shared CompiledModule**: Compile the `.wasm` file once at startup with `wazero.NewRuntimeWithConfig`. All per-request `ModuleInstance`s are instantiated from it.
- **Memory snapshot**: After WASM module init (before first request), snapshot linear memory:
  ```go
  snap := make([]byte, mod.Memory().Size())
  mod.Memory().Read(0, snap)
  ```
  On recycle, restore with `mod.Memory().Write(0, snap)`.
- **Call convention**: Expose `evaluate(method_ptr, method_len, data_ptr, data_len) (resp_ptr, resp_len)` from the guest. Host provides WASI preview1 imports.
- **Resource limits**: Apply via wazero's `context`-based fuel/step counting, or via Go goroutine + context cancellation.

### Registration in Factory

In `internal/execution/module.go` or wherever dispatchers are constructed (likely a `switch` on `IOInterface`), add:

```go
case io.WasmIO:
    return wasm.NewDispatcher(cfg)
```

---

## Language Routing (broader Shimmy-WASM project)

The WASM backend routes evaluation functions by language:

| Language | Compilation Target | Runtime | Features |
|----------|--------------------|---------|---------|
| Rust / C / C++ / Go | `wasm32-wasip1` | wazero | Snapshot, N:1 mux |
| Python (pure / NumPy) | CPython-WASI → `.wasm` | wazero | Snapshot, N:1 mux |
| Python (scipy/pandas) | Pyodide (Emscripten) | Node.js subprocess | Full compat, no snapshot |
| JavaScript | Goja (pure Go, in-process) | in-process | No subprocess needed |

Routing is determined at function registration time based on declared language + `requirements`.

---

## Key Dependencies

| Package | Purpose |
|---------|---------|
| `github.com/tetratelabs/wazero` | WASM runtime (to be added) |
| `go.uber.org/fx` | Dependency injection |
| `github.com/alitto/pond` or `github.com/jackc/puddle` | Worker pool (file mode) |
| `github.com/go-chi/chi/v5` | HTTP router |
| `github.com/getsentry/sentry-go` | Error reporting |

---

## Key Files

| File | Description |
|------|-------------|
| `main.go` | Entry point: Sentry init + CLI |
| `cmd/root.go` | Three CLI commands: `run`, `serve`, `lambda` |
| `runtime/runtime.go` | `Runtime` interface |
| `internal/execution/dispatcher/dispatcher.go` | `Dispatcher` interface |
| `internal/execution/supervisor/supervisor.go` | `Supervisor` interface |
| `internal/execution/supervisor/adapter.go` | `Adapter` interface |
| `internal/execution/worker/worker.go` | `Worker` interface |
| `internal/execution/module.go` | fx module wiring |

---

## Relationship to Other Projects

- `/workspace/extra/projects/shimmy/` — original shimmy (read-only reference)
- `/workspace/extra/projects/shimmy-docs/` — architecture docs, paper notes, research notes
  - `docs/research/python-sandbox-architecture.md` — two-tier Python decision
  - `docs/research/what-works.md` — Lambda environment probing results
  - `docs/papers/zpoline.md` — syscall interception reference
  - `docs/papers/mares-grading-security-2021.md` — threat model reference
- `/workspace/extra/projects/interim-report/` — Typst interim report
