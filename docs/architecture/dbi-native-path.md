# DBI path: native-process execution under DynamoRIO

> **Scope.** Current implementation on `feat/wasm-backend` at `5057978`.
> The checked-in policy client is bounded evidence code. This document
> deliberately separates wrapper mechanics, worker lifecycle, and policy
> coverage.

[Architecture index](README.md) · [WASM path](wasm-wazero-memory.md) ·
[QEMU path](qemu-guest-path.md)

## 1. Position in Shimmy

DBI is not an IO interface and not a second dispatcher. It is a command wrapper
applied before ordinary `file` or `rpc` dispatcher selection:

```text
original native worker
  command + args + cwd + env
          │
          ▼
applyExecutionWrappers
          │ QEMU disabled, DBI enabled
          ▼
applyDBISecurityConfig
          │
          ▼
drrun [options] [-c client.so] -- worker [worker args]
          │
          ▼
ordinary file or RPC supervisor
```

Sources:

- [`NewDispatcher`](../../internal/execution/dispatcher.go#L42-L46)
- [`applyExecutionWrappers`](../../internal/execution/dispatcher_qemu.go#L20-L38)
- [`applyDBISecurityConfig`](../../internal/execution/dispatcher_dbi.go#L14-L66)

QEMU and DBI are mutually exclusive. DBI accepts only `file` and `rpc`; enabling
it for WASM, Agent Python or Pyodide fails during dispatcher construction.

## 2. Configuration and argv transformation

| Variable | Current behavior |
|---|---|
| `FUNCTION_DBI_SECURITY_ENABLED` | Go boolean; empty/false disables; malformed values fail startup. |
| `FUNCTION_DBI_DRRUN` | Launcher path; defaults to `drrun`. |
| `FUNCTION_DBI_CLIENT` | Optional client shared object passed after `-c`. |
| `FUNCTION_DBI_OPTIONS` | Extra `drrun` words split with `strings.Fields`. |
| `FUNCTION_DBI_CONFIG_PATH` | Optional file checked only for readability. |

The wrapper starts from:

```text
worker worker-arg-0 worker-arg-1
```

and constructs typed Go argv:

```text
drrun <DBI option words> -c client.so -- worker worker-arg-0 worker-arg-1
```

There is no shell. However, `FUNCTION_DBI_OPTIONS` is not a shell parser either:
quotes and embedded spaces are not preserved because the code uses
`strings.Fields`.

The Go wrapper does not parse `FUNCTION_DBI_CONFIG_PATH`, pass a new environment
variable for it, or prove that the client consumes it. The checked-in
`path_policy.c` does not parse this file. A custom client must define that
contract itself.

The client and runner paths are not pre-opened or validated. A bad path fails
when the process is started.

## 3. Process and memory topology

```text
Shimmy Go process
  └─ dispatcher
      └─ WorkerSupervisor
          └─ ProcessWorker
              └─ drrun
                  ├─ DynamoRIO runtime
                  ├─ policy client callbacks
                  └─ exact native evaluator
```

`ProcessWorker` uses `exec.CommandContext`, inherits `os.Environ()`, appends the
configured environment, applies the configured cwd and creates a Unix process
group. The DBI client runs in the instrumented native process. There is no
WASM-style shared linear memory or host/guest COW image.

Sources:

- [`ProcessWorker` command creation](../../internal/execution/worker/worker.go#L438-L461)
- [Unix process-group setup](../../internal/execution/worker/worker_unix.go#L10-L27)

### 3.1 What process exit resets

For a transient `file` worker, process exit discards its private native address
space: heap, writable mappings, globals, stacks and threads. The kernel also
closes its descriptor table entries and removes the process.

This is lifecycle destruction, not byte restoration. Shimmy does not hold a
native memory snapshot and does not call `fork`, `mmap` or `madvise` to reset the
DBI worker.

External effects are not rolled back. Files, sockets, remote services, shared
mappings, child processes that escape supervision, and other system-wide state
must be controlled separately.

### 3.2 File and RPC are different freshness contracts

| Interface | Process lifetime | Request freshness |
|---|---|---|
| `file` | New `drrun` + evaluator per request | Process-private state is discarded on exit. |
| `rpc` automatic | One instrumented worker persists | Native heap/globals/threads persist. |
| `rpc` invocation/eager | Supervisor policy can replace the worker | Freshness comes from replacement, not DBI. |

Automatic lifecycle selection treats `file` as transient and RPC as persistent.
`WorkerSupervisor.Send` serializes access to an individual supervisor with
`sendLock`.

Sources:

- [lifecycle resolution](../../internal/execution/supervisor/supervisor.go#L130-L177)
- [send serialization](../../internal/execution/supervisor/supervisor.go#L180-L198)

## 4. File path: one instrumented process per request

The outer file adapter owns the request envelope and temporary files. For each
request it:

1. creates a private request directory below `/tmp/shimmy`;
2. creates request and response files;
3. serializes `{command, params}` into the request file;
4. appends both paths to the already wrapped command;
5. adds `EVAL_IO=FILE` and the two `EVAL_FILE_NAME_*` variables;
6. starts `drrun`;
7. drains stdout in a goroutine;
8. waits for complete process exit;
9. requires a zero exit status;
10. decodes the response file;
11. removes the temporary directory and files.

The effective argv is:

```text
drrun [options] [-c client.so] -- evaluator [args] host-request host-response
```

The evaluator and wrapper see the same host filesystem namespace. DBI does not
create a mount namespace, chroot, user namespace or network namespace.

The adapter retains a bounded 64 KiB stdout diagnostic tail. `ProcessWorker`
uses an unbounded `bytes.Buffer` for stderr in the current implementation, so a
noisy worker can consume host memory.

Sources:

- [`fileAdapter.Send`](../../internal/execution/supervisor/adapter_file.go#L107-L256)
- [diagnostic tail](../../internal/execution/supervisor/adapter_file.go#L40-L82)
- [worker stderr buffer](../../internal/execution/worker/worker.go#L196-L215)

## 5. RPC path: instrumented process reuse

For RPC, Shimmy builds a dedicated dispatcher and boots the wrapped process
before serving requests. The adapter:

1. injects `EVAL_IO=rpc` and transport-specific endpoint variables;
2. creates stdio pipes when `stdio` is selected;
3. starts `drrun -- evaluator`;
4. dials the evaluator RPC endpoint with retry;
5. invokes `rpc.Client.CallContext` per request under the send timeout;
6. keeps the process alive until worker/runtime shutdown.

Supported transports are stdio, IPC, TCP, HTTP and WebSocket. Stdio framing
limits payloads to 4 MiB and cumulative header bytes to 64 KiB.

Sources:

- [`rpcAdapter.Start` and `Send`](../../internal/execution/supervisor/adapter_rpc.go#L110-L214)
- [RPC dialing](../../internal/execution/supervisor/adapter_rpc.go#L224-L310)
- [frame limits](../../internal/protocol/limits.go#L5-L10)
- [`headerPrefixPipe`](../../internal/execution/supervisor/adapter_rpc_pipe.go#L23-L125)

A request error does not automatically recycle a persistent DBI RPC process.
The dedicated dispatcher normally releases the same supervisor, so a failed or
partially contaminated worker can remain alive. Use an invocation-scoped
lifecycle if process freshness is required; do not infer it from DBI enablement.

## 6. DynamoRIO callback path

The checked-in bounded client is
[`demo/compose/dbi-lean/path_policy.c`](../../demo/compose/dbi-lean/path_policy.c).
Its lifecycle is:

```text
drrun loads client.so
    │
    ├─ dr_client_main
    │    ├─ set client name
    │    ├─ register syscall filter
    │    ├─ register pre-syscall callback
    │    └─ write initialization marker
    │
application performs syscall
    │
    ├─ event_filter_syscall(sysnum)
    │       false → DynamoRIO does not invoke this client's policy callback
    │       true  → event_pre_syscall(ctx, sysnum)
    │
    └─ policy
         allow → return true
         deny  → audit; set result to -EPERM; return false
```

`deny` calls:

```c
dr_syscall_set_result(ctx, -EPERM);
return false;
```

so the evaluator observes a failed syscall with `errno=EPERM`; the original
kernel syscall is not executed.

### 6.1 Filtered syscall numbers

The x86_64 evidence client filters:

```text
open, openat, creat, write, connect,
clone, fork, vfork, execve, execveat, mprotect
```

It does not filter every authority-bearing syscall. For example, this client has
no `openat2` rule.

### 6.2 Implemented decisions

| Rule | Trigger | Decision |
|---|---|---|
| `fd_write` | `write` to fd 198 | `EPERM` |
| `network_connect` | any `connect` | `EPERM` |
| `process_create` | `clone`/`fork`/`vfork`, unless allow marker exists | `EPERM` |
| `process_exec` | `execve`/`execveat` | `EPERM` |
| `executable_memory` | `mprotect` adds `PROT_EXEC` | `EPERM` |
| `filesystem_read` | exact path `/tmp/shimmy-dbi-denied.txt` | `EPERM` |
| `filesystem_write` | exact path `/tmp/shimmy-dbi-denied-write.txt` | `EPERM` |

Path bytes are copied from application memory with `dr_safe_read` into a fixed
256-byte buffer. An unreadable or unterminated path is allowed. Paths are exact
string comparisons; no canonicalization, symlink resolution or traversal check
is performed.

Sources:

- [filter](../../demo/compose/dbi-lean/path_policy.c#L56-L61)
- [deny helper and policy](../../demo/compose/dbi-lean/path_policy.c#L63-L95)
- [safe string read](../../demo/compose/dbi-lean/path_policy.c#L44-L54)

## 7. Audit and marker ownership

Every denial attempts to append one line to:

```text
/tmp/shimmy-dbi-policy-audit.jsonl
```

with shape:

```json
{"decision":"deny","rule":"...","syscall":N,"errno":1}
```

The client also writes:

```text
/tmp/shimmy-dbi-client-init.marker
/tmp/shimmy-dbi-policy-block.marker
/tmp/shimmy-dbi-allow-exec-child.marker
```

These are fixed global paths, not request-, process- or tenant-scoped. Parallel
file workers can append to the same audit file and overwrite the same markers.
The helper ignores audit open/write/close failures. Policy denial still returns
`EPERM` even if the audit record is lost.

Source: [`audit` and `write_file_text`](../../demo/compose/dbi-lean/path_policy.c#L29-L42).

## 8. Failure and cleanup sequence

### 8.1 Pre-launch failures

Shimmy fails before worker creation for:

- malformed DBI enablement;
- unsupported interface;
- empty native command;
- unreadable policy config;
- failure closing the probed config file.

Bad `drrun` or client paths fail later at process start.

### 8.2 File worker failures

`fileAdapter.Send` propagates request-file, process-start, wait, non-zero-exit and
response-decode failures. It defers request-directory cleanup. Its `Stop` method
is a no-op because `Send` has already waited for the transient process.

### 8.3 RPC startup rollback

If worker start or RPC dialing fails, the adapter closes transport resources and
attempts to stop and wait for the process. Dial rollback uses a fixed five-second
wait. The startup rollback path does not explicitly escalate to `SIGKILL` after
that wait timeout.

### 8.4 Runtime shutdown

The supervisor:

1. calls adapter `Stop`;
2. cancels the owned worker context if stop fails;
3. waits for process exit;
4. races the wait against `StopParams.Timeout`;
5. uses a background wait context so caller cancellation cannot make a live
   process look reaped.

On Unix, signals target the process group where available.

Sources:

- [file errors and cleanup](../../internal/execution/supervisor/adapter_file.go#L107-L265)
- [RPC rollback](../../internal/execution/supervisor/adapter_rpc.go#L161-L188)
- [supervisor stop/wait](../../internal/execution/supervisor/supervisor.go#L335-L369)
- [process-group signals](../../internal/execution/worker/worker_unix.go#L10-L20)

## 9. Security boundary

### 9.1 Implemented

- Transparent wrapping preserves exact native evaluator binaries and protocols.
- DynamoRIO can invoke a client before selected syscalls.
- The bounded client can replace selected syscall results with `-EPERM`.
- File lifecycle can provide per-request process destruction.
- QEMU and DBI cannot be enabled together accidentally.

### 9.2 Not implemented by the wrapper

The Go wrapper does not provide:

- complete syscall mediation;
- environment allowlisting or secret removal;
- descriptor closure/allowlisting;
- mount, user, PID or network namespaces;
- seccomp;
- cgroup/resource enforcement;
- filesystem path canonicalization;
- whole-process snapshot/reset;
- RPC process replacement after every error;
- audit delivery guarantees.

Native workers inherit the host environment plus configured additions. The
checked-in client is Linux x86_64 evidence code with hard-coded syscall numbers.
DynamoRIO is an instrumentation substrate; policy coverage is defined by the
client, not by the wrapper name.

### 9.3 Exact scope of repository evidence

The in-repository compose smoke verifies service health, successful Lean
evaluation and client initialization. It does not execute every denial rule and
validate all `EPERM` results and audit lines. Broader Lambda results cited by the
older DBI document live in a companion evidence ledger and are version-bound.
They are not reproduced by this architecture document.

## 10. Build and deployment shape

The demo image:

- pins DynamoRIO `11.91.20545`;
- verifies its archive digest;
- compiles `path_policy.so` against the DynamoRIO headers/libraries;
- includes the launcher and runtime libraries;
- is Linux/amd64 only;
- creates DynamoRIO config/log directories before executing Shimmy.

Sources:

- [Docker build stages](../../demo/compose/Dockerfile#L114-L174)
- [entrypoint](../../demo/compose/dbi-lean/entrypoint.sh)
- [compose service](../../demo/compose/compose.yaml#L73-L98)

`tools/dbi/dynamorio_open_log.c` is a separate logging-only client. It observes
open-family syscalls and does not enforce a policy.

## 11. Verification map

### Go tests

```bash
go test ./internal/execution \
  -run 'TestApplyDBISecurityConfig|TestApplyExecutionWrappers' -count=1

go test ./internal/execution/supervisor \
  -run 'Test(FileAdapter|RpcAdapter|StdioAdapter|HeaderPrefix|Supervisor_)' \
  -count=1

go test ./internal/execution/worker -count=1
```

Coverage includes:

- file/RPC wrapping without interface mutation;
- disabled wrapper no-op;
- unsupported-interface, missing-command and invalid-boolean rejection;
- readable/missing policy config;
- DBI/QEMU mutual exclusion;
- transient/persistent/invocation/eager supervisor lifecycles;
- file cleanup and process exit;
- RPC rollback and framing limits;
- Unix process stop/wait behavior.

These tests do not execute a real DynamoRIO denial matrix.

### Container path

```bash
docker compose -f demo/compose/compose.yaml --profile dbi up --build dbi-lean
scripts/smoke-compose-demo.sh
```

This requires Linux/amd64 or CI and may perform network/package operations.

## 12. Source map

| Concern | Source |
|---|---|
| wrapper precedence | [`dispatcher_qemu.go`](../../internal/execution/dispatcher_qemu.go#L20-L38) |
| DBI env parsing and argv | [`dispatcher_dbi.go`](../../internal/execution/dispatcher_dbi.go#L14-L73) |
| dispatcher selection | [`dispatcher.go`](../../internal/execution/dispatcher.go#L42-L180) |
| dedicated RPC dispatcher | [`dispatcher_dedicated.go`](../../internal/execution/dispatcher/dispatcher_dedicated.go#L38-L127) |
| pooled file dispatcher | [`dispatcher_pooled.go`](../../internal/execution/dispatcher/dispatcher_pooled.go#L44-L203) |
| lifecycle ownership | [`supervisor.go`](../../internal/execution/supervisor/supervisor.go#L96-L480) |
| file protocol | [`adapter_file.go`](../../internal/execution/supervisor/adapter_file.go#L94-L265) |
| RPC protocol | [`adapter_rpc.go`](../../internal/execution/supervisor/adapter_rpc.go#L110-L360) |
| native process creation | [`worker.go`](../../internal/execution/worker/worker.go#L103-L461) |
| process-group signals | [`worker_unix.go`](../../internal/execution/worker/worker_unix.go#L10-L27) |
| bounded policy client | [`path_policy.c`](../../demo/compose/dbi-lean/path_policy.c#L29-L105) |
| logging-only client | [`dynamorio_open_log.c`](../../tools/dbi/dynamorio_open_log.c#L15-L41) |
| wrapper tests | [`dispatcher_dbi_test.go`](../../internal/execution/dispatcher_dbi_test.go) |

## 13. Open design questions

1. Should `FUNCTION_DBI_CONFIG_PATH` become a typed client contract rather than a
   readability probe?
2. Should `drrun` and client artifacts be prevalidated at startup?
3. Should production policy use normalized descriptor-relative paths and
   `openat2`?
4. Should audit sinks be per invocation and fail closed when persistence fails?
5. Should DBI RPC default to invocation-scoped replacement rather than persistent
   reuse when security mode is enabled?
6. Should environment and descriptor allowlists be enforced before `exec`?
7. How should syscall-number portability be represented beyond Linux x86_64?
8. Which denial E2E must be checked into this repository rather than referenced
   from an external evidence ledger?

## 14. Core invariants

1. DBI is a wrapper around `file`/`rpc`, never an automatic retry path.
2. QEMU and DBI cannot be active together.
3. A DBI client, not DynamoRIO by itself, defines policy coverage.
4. `file` freshness comes from process exit; DBI does not snapshot native memory.
5. Persistent RPC state remains persistent unless supervisor lifecycle replaces
   the worker.
6. A denial returns `EPERM`; audit persistence is separate and currently
   best-effort.
7. Checked-in policy rules are representative bounded evidence, not exhaustive
   hostile-native containment.
