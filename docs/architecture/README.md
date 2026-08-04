# Shimmy execution architecture: one control plane, three primary paths

> **Code scope.** This document describes the implementation on
> `feat/wasm-backend` at commit `5057978` (the branch checked out when this
> document set was authored). The later `shimmy-wasm-native-python` worktree
> contains additional Agent Python lifecycle work and is not silently projected
> back into this repository. Follow the source links below when behavior changes.

Shimmy does not dynamically benchmark several runtimes and pick the fastest one.
It makes a deployment-time routing decision before request execution and then
owns a backend-specific lifecycle. The three primary paths are:

1. **WASM / wazero**: execute a validated WebAssembly module in-process and
   recover bounded guest state, normally its linear memory;
2. **native + DBI**: preserve an exact native worker and optionally place
   DynamoRIO plus a policy client around the process;
3. **QEMU**: preserve the exact native worker but run it inside a whole guest
   managed by `shimmy-qemu-runner`.

DBI and QEMU are wrappers around the existing `file` or `rpc` worker contract.
They are not new `FUNCTION_INTERFACE` values and are mutually exclusive.

## 1. Architectural invariant

Every path must implement the same Host-facing dispatcher contract:

```go
Start(context.Context) error
Send(context.Context, string, map[string]any) (map[string]any, error)
Shutdown(context.Context) error
```

Source: [`internal/execution/dispatcher/dispatcher.go`](../../internal/execution/dispatcher/dispatcher.go#L9-L18).

Conceptually, every independent request follows:

```text
REQUEST
  |
  v
admission + deployment-selected route
  |
  v
acquire backend-owned capacity
  |
  v
execute exactly once
  |
  v
copy/obtain response under Host ownership
  |
  v
recover the backend state boundary
  |                         \
  | success                  \ timeout / trap / drift / protocol failure
  v                           v
return clean capacity       retire capacity
                              |
                              v
                         create/refill replacement
```

The critical ordering is **response ownership before state recovery**. For
in-process WASM, the Host copies response bytes out of guest linear memory before
memory reset. For process/guest paths, the existing file/RPC adapter obtains the
response before the invocation-scoped process or guest is stopped.

This is a reuse contract, not a claim that all external effects can be rolled
back. Files, sockets, remote services, credentials, clocks, entropy and other
Host/guest-external state remain governed by the selected capability profile and
worker lifecycle.

## 2. Route construction

The top-level factory is
[`execution.NewDispatcher`](../../internal/execution/dispatcher.go#L42-L180).
Construction has two ordered phases.

### 2.1 Apply native wrappers first

```text
supervisor.Config
  |
  v
applyExecutionWrappers
  |-- FUNCTION_QEMU_ENABLED=false --> applyDBISecurityConfig
  |-- FUNCTION_QEMU_ENABLED=true  --> reject DBI; applyQEMUFallbackConfig
  v
possibly rewritten StartConfig
```

Source:
[`internal/execution/dispatcher_qemu.go`](../../internal/execution/dispatcher_qemu.go#L20-L39).

Consequences:

- DBI and QEMU are selected **before** the worker starts;
- a request is never replayed on QEMU after another backend fails;
- enabling both wrappers fails before execution;
- the selected IO contract (`file` or `rpc`) is preserved;
- the wrapper changes the process command, not the application request schema.

### 2.2 Select the dispatcher by IO interface

After wrapper resolution, `NewDispatcher` selects:

- `FUNCTION_INTERFACE=wasm` + `FUNCTION_WASM_PROFILE=generic`:
  [`wasm.Dispatcher`](../../internal/execution/wasm/dispatcher.go);
- `FUNCTION_INTERFACE=wasm` + `agent-python|python-reactor|reactor-python`, or
  the legacy `FUNCTION_INTERFACE=reactor-python` alias:
  [`wasm.AgentPythonDispatcher`](../../internal/execution/wasm/agent_python.go);
- native `rpc`: a dedicated process-backed dispatcher;
- native `file`: a pooled process-backed dispatcher;
- the older `python-wasm` and Pyodide compatibility paths: separate adapters,
  not additional primary architecture directions.

Source:
[`internal/execution/dispatcher.go`](../../internal/execution/dispatcher.go#L48-L178).

## 3. The three state boundaries

| Path | Capacity unit | State retained while ready | Request completion recovery | Current claim boundary |
|---|---|---|---|---|
| WASM | one wazero module instance / prepared slot | compiled code is shared; each slot owns module state and linear memory | restore captured linear-memory bytes, or close the slot | bounded WASM state plus named Host capabilities |
| DBI | native worker process | depends on `file` versus `rpc` lifecycle | invocation-scoped process exit for `file`; no automatic reset for persistent `rpc` | selected syscall mediation and process-memory freshness only where lifecycle provides it |
| QEMU | QEMU runner plus guest | complete guest state until guest destruction | `lazy`/`eager` destroys the served guest; `off` intentionally reuses state | exact-binary whole-guest lifecycle under the configured image/profile |

The paths are not interchangeable performance rows. They trade different
compatibility and recovery boundaries.

## 4. WASM path ownership

The Generic WASM dispatcher owns:

```text
1 wazero.Runtime
1 wazero.CompiledModule
1 locked-down wazero.ModuleConfig
N wasmSupervisor slots
N api.Module instances
N linear-memory regions
N snapshot strategy instances
optional: 1 dispatcher-scoped COW canonical-image memfd
```

The runtime and compiled code are shared, but module instances are not. A
supervisor mutex serializes use of one module instance. The pool supplies
parallelism across instances.

Agent Python also compiles one runtime artifact once, then owns prepared module
slots. Its snapshot is taken only after runtime initialization, trusted evaluator
preparation and allocator headroom reservation. In this branch, each Agent Python
COW snapshot slot creates its own `cowImageCoordinator`; Generic WASM shares one
coordinator across its slots. This ownership difference is intentional to record,
even though later branches may evolve it.

Full details: [WASM, wazero and prepared-memory reset](wasm-wazero-memory.md).

## 5. DBI path ownership

DBI does not own a new dispatcher. It rewrites:

```text
<worker> <args...>
```

into:

```text
drrun [options...] [-c client.so] -- <worker> <args...>
```

The existing file/RPC adapter still owns request transport and process startup.
The DynamoRIO client owns the actual interception and allow/deny decisions.
Shimmy verifies wrapper configuration and policy-file readability, but it does
not infer policy completeness from client loading.

Full details: [Native worker with DynamoRIO](dbi-native-path.md).

## 6. QEMU path ownership

QEMU similarly rewrites the original worker command to:

```text
shimmy-qemu-runner -- <original command> <original args...>
```

The runner owns QEMU, the guest bridge, temporary control files/endpoints and the
inner evaluator command. Shimmy still speaks the existing file or RPC contract.
The reset policy chooses whether a guest is intentionally persistent (`off`),
destroyed after each invocation and created on the next borrow (`lazy`), or
replaced by an eagerly prepared clean guest (`eager`).

A live QEMU process is not by itself ready capacity. Readiness includes artifact
verification, boot identity, guest handshake and evaluator/bridge readiness.

Full details: [QEMU whole-guest path](qemu-guest-path.md).

## 7. Failure and replacement semantics

### 7.1 Generic WASM

`wasmSupervisor.Send` always attempts restore after guest execution. Restore
failure marks the slot unhealthy. The dispatcher then closes the slot and starts
a replacement asynchronously. Memory growth is fail-closed because WASM memory
cannot shrink: the grown tail is zero-filled, the slot is marked unhealthy and it
is retired.

Source:

- [`wasmSupervisor.Send`](../../internal/execution/wasm/supervisor.go#L140-L168);
- [`restoreSnapshot`](../../internal/execution/wasm/supervisor.go#L228-L257);
- [`Dispatcher.Send`](../../internal/execution/wasm/dispatcher.go#L221-L263);
- [`spawnReplacementAsync`](../../internal/execution/wasm/dispatcher.go#L301-L344).

### 7.2 Agent Python

Snapshot mode checks out a prepared slot, copies the response payload into a
Host-owned byte slice, restores memory and only then returns the slot to the
prepared pool. A call or restore error closes the slot and synchronously creates
a replacement before returning the combined diagnostic. `single-use` and `fresh`
close every served module instead of resetting it.

Source:
[`AgentPythonDispatcher.Send`](../../internal/execution/wasm/agent_python.go#L314-L475).

### 7.3 DBI and QEMU

Process-backed behavior is determined by `WorkerLifecycle`:

- automatic: RPC persistent, file invocation-scoped;
- persistent: one RPC worker;
- invocation: one worker per request;
- eager: one ready worker is consumed, then a replacement is prepared.

Source:
[`internal/execution/supervisor/config.go`](../../internal/execution/supervisor/config.go#L24-L35)
and
[`internal/execution/supervisor/supervisor.go`](../../internal/execution/supervisor/supervisor.go#L130-L211).

DBI does not alter this lifecycle. QEMU may explicitly narrow it through
`FUNCTION_QEMU_RESET_POLICY`.

## 8. Concurrency and shutdown

### 8.1 WASM

- a buffered channel bounds ready slots;
- checkout respects request context and dispatcher shutdown;
- each supervisor has a mutex, so one module is never used concurrently;
- `pending` tracks in-flight requests, asynchronous discards and replacement
  creation;
- shutdown atomically closes admission, waits for `pending`, drains ready slots,
  closes the runtime and finally closes the COW image coordinator.

The COW image fd must outlive every private mapping that may reset from it.
Closing the coordinator before the slots would make reset fail with
`ErrCowImageClosed`.

### 8.2 Process-backed paths

The supervisor serializes lifecycle state under its own mutex. Persistent RPC
owns one worker. Invocation lifecycle starts and waits for a worker per message.
Eager lifecycle keeps ready and refill state separate so the served worker is not
reused.

## 9. Configuration summary

| Concern | Primary configuration |
|---|---|
| Generic WASM | `FUNCTION_INTERFACE=wasm`, `FUNCTION_WASM_PROFILE=generic`, `FUNCTION_WASM_MODULE` |
| Agent Python | `FUNCTION_WASM_PROFILE=agent-python`, runtime manifest, evaluator script/bundle |
| WASM memory bound | `FUNCTION_WASM_MAX_MEMORY_PAGES` |
| WASM capabilities | `FUNCTION_WASM_ALLOWED_PATHS`, `FUNCTION_WASM_ALLOWED_ENV` |
| reset strategy | `FUNCTION_WASM_SNAPSHOT_MODE=memcpy|soft-dirty|mprotect|uffd|cow` |
| DBI | `FUNCTION_DBI_SECURITY_ENABLED`, `FUNCTION_DBI_DRRUN`, `FUNCTION_DBI_CLIENT`, `FUNCTION_DBI_CONFIG_PATH`, `FUNCTION_DBI_OPTIONS` |
| QEMU | `FUNCTION_QEMU_ENABLED`, binary/rootfs/manifest, runner and reset policy variables |
| process lifecycle | IO default or wrapper-selected `WorkerLifecycle` |

See [configuration and verification map](configuration-and-verification.md) for
exact defaults, guards, logs and tests.

## 10. What the architecture does not claim

The following statements are deliberately false:

- “COW clones a complete wazero module.” It remaps linear memory only.
- “DBI makes any native RPC worker fresh.” Persistent RPC remains stateful.
- “QEMU is chosen after a failed request.” It is selected before execution.
- “QEMU lifecycle evidence is KVM performance evidence.” The validated path in
  this repository is TCG unless a separate target proves KVM.
- “Reset rolls back external effects.” It does not.
- “One backend is universally fastest.” Cohorts and lifecycle semantics differ.

## 11. Document map

- [WASM, wazero and prepared-memory reset](wasm-wazero-memory.md)
- [Native worker with DynamoRIO](dbi-native-path.md)
- [QEMU whole-guest path](qemu-guest-path.md)
- [Configuration, observability and verification](configuration-and-verification.md)
- [Existing product-tier model](../wasm-backend-model.md)
- [Archived Linux COW evidence](../cow-memory-image-evidence-2026-07-23.md)
