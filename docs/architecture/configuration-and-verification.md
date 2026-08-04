# Configuration, observability and verification map

> **Scope.** Current implementation on `feat/wasm-backend` at `5057978`.
> This is an operator/source map for the architecture set, not a deployment
> qualification record.

[Architecture index](README.md) · [WASM](wasm-wazero-memory.md) ·
[DBI](dbi-native-path.md) · [QEMU](qemu-guest-path.md)

## 1. Route selection is explicit

```text
FUNCTION_INTERFACE
  wasm + FUNCTION_WASM_PROFILE
      generic       → Generic WASM dispatcher
      agent-python  → Agent Python dispatcher
  file              → transient native file worker
  rpc               → process-backed RPC worker

Optional native wrapper, selected before dispatcher construction:
  FUNCTION_DBI_SECURITY_ENABLED=true
  xor
  FUNCTION_QEMU_ENABLED=true
```

There is no request-time fallback, source inspection or performance probe.

## 2. Shared process settings

| Variable | Default/source | Meaning |
|---|---|---|
| `FUNCTION_COMMAND` | required by process-backed paths | evaluator or WASM artifact command/path |
| `FUNCTION_ARGS` | empty | typed repeated argv |
| `FUNCTION_WORKING_DIR` | process default | worker cwd |
| `FUNCTION_ENV` | empty | extra `KEY=VALUE` entries |
| `FUNCTION_MAX_PROCS` | `runtime.NumCPU()` when non-positive | pool/slot capacity |
| `FUNCTION_WORKER_SEND_TIMEOUT` | 30 s | request/call deadline |
| `FUNCTION_WORKER_STOP_TIMEOUT` | 5 s | process shutdown deadline |
| `FUNCTION_RPC_TRANSPORT` | stdio | stdio, IPC, TCP, HTTP or WebSocket |

Sources: [`cmd/root.go`](../../cmd/root.go#L47-L143) and
[`execution.Config`](../../internal/execution/dispatcher.go#L17-L39).

## 3. WASM settings

### Generic and shared

| Variable | Default | Guard/effect |
|---|---:|---|
| `FUNCTION_WASM_PROFILE` | `generic` for `FUNCTION_INTERFACE=wasm` | Generic or Agent Python aliases. |
| `FUNCTION_WASM_MODULE` | worker command/path | module bytes loaded and compiled once. |
| `FUNCTION_WASM_MAX_MEMORY_PAGES` | 256 (16 MiB) | passed to wazero runtime memory limit. |
| `FUNCTION_WASM_ALLOWED_PATHS` | none | comma-separated read-only WASI preopens. |
| `FUNCTION_WASM_ALLOWED_ENV` | none | comma-separated names copied from Host env. |
| `FUNCTION_WASM_SNAPSHOT_MODE` | `memcpy` behavior | memcpy, soft-dirty, mprotect, uffd or cow. |
| `FUNCTION_WASM_USE_UFFD` | false | deprecated alias; resolves to uffd if mode empty. |
| `FUNCTION_WASM_COMPILE_CACHE` | empty | optional wazero on-disk compilation cache. |

Invalid numeric strings for several WASM env settings are currently ignored by
the direct env loader instead of failing startup; the zero/default value then
applies. Deployment tooling should validate them before launch.

### Agent Python

| Variable | Default | Guard/effect |
|---|---:|---|
| `FUNCTION_WASM_MANIFEST` | required for pinned artifact flow | artifact/ABI verification input. |
| `FUNCTION_WASM_PYTHON_SCRIPT` | route dependent | trusted evaluator script or generated bundle. |
| `FUNCTION_WASM_PYTHON_PRELOAD` | `evaluator` | `evaluator` or `off`. |
| `FUNCTION_WASM_PYTHON_LIFECYCLE` | `snapshot` | fresh, single-use or snapshot. |
| `FUNCTION_WASM_PYTHON_PREPARED_CAPACITY` | 1 | supported range 1..4. |
| `FUNCTION_WASM_PYTHON_SNAPSHOT_HEADROOM_BYTES` | 8 MiB | reserved before prepared snapshot. |

Agent Python rejects more than four max instances. Snapshot strategy is legal
only with lifecycle `snapshot`. Soft-dirty and mprotect reject pool sizes above
one because their tracking state is process-global.

Sources: [`wasm/config.go`](../../internal/execution/wasm/config.go#L24-L262).

## 4. DBI settings

| Variable | Default | Guard/effect |
|---|---|---|
| `FUNCTION_DBI_SECURITY_ENABLED` | false | Go boolean; file/RPC only. |
| `FUNCTION_DBI_DRRUN` | `drrun` | launcher command. |
| `FUNCTION_DBI_CLIENT` | empty | optional `-c client.so`. |
| `FUNCTION_DBI_OPTIONS` | empty | split with `strings.Fields`. |
| `FUNCTION_DBI_CONFIG_PATH` | empty | optional readability probe only. |

Fail-closed startup checks cover malformed boolean, unsupported interface,
missing original command and unreadable configured policy file. Launcher/client
existence and actual client policy are checked only by process launch/client
behavior.

Source: [`dispatcher_dbi.go`](../../internal/execution/dispatcher_dbi.go#L12-L73).

## 5. QEMU settings

| Variable | Default | Guard/effect |
|---|---:|---|
| `FUNCTION_QEMU_ENABLED` | false | file/RPC only; conflicts with DBI. |
| `FUNCTION_QEMU_RUNNER` | `shimmy-qemu-runner` | outer wrapper command. |
| `FUNCTION_QEMU_BINARY` | required | readable-path precheck plus runner launch. |
| `FUNCTION_QEMU_ROOTFS` | required | must match manifest rootfs identity. |
| `FUNCTION_QEMU_IMAGE_MANIFEST` | required | strict schema/digest validation. |
| `FUNCTION_QEMU_RESET_POLICY` | Lambda: lazy; otherwise off | off, lazy or eager. |
| `FUNCTION_QEMU_ACCELERATOR` | auto | KVM if available, otherwise TCG. Explicit KVM fails if unavailable. |
| `FUNCTION_QEMU_MEMORY_MB` | 512 | 1..32768. |
| `FUNCTION_QEMU_VCPUS` | 1 | 1..64. |
| `FUNCTION_QEMU_MAX_FRAME_BYTES` | 4 MiB | 1 KiB..64 MiB. |
| `FUNCTION_QEMU_BOOT_TIMEOUT` | 60 s | 10 ms..10 min. |
| `FUNCTION_QEMU_SHUTDOWN_TIMEOUT` | 10 s | 10 ms..2 min. |
| `FUNCTION_QEMU_WORK_ROOT` | `/tmp/shimmy-qemu` | private per-VM directories below this root. |
| `FUNCTION_QEMU_NETWORK_PROFILE` | none | none or explicit inherit/user NAT. |

Eager requires RPC and is rejected in Lambda. Lazy selects invocation-scoped
worker replacement. Off leaves ordinary RPC persistent.

Sources:

- [`dispatcher_qemu.go`](../../internal/execution/dispatcher_qemu.go#L14-L130)
- [`qemurun/runtime.go`](../../internal/execution/qemurun/runtime.go#L24-L156)

## 6. Startup gates by path

### WASM

```text
load module bytes
→ create runtime/config/cache
→ instantiate WASI
→ compile module
→ instantiate every requested slot
→ run initialization exports
→ select snapshot strategy
→ Take prepared baseline
→ only then report dispatcher ready
```

Agent Python additionally verifies the pinned manifest/artifact, installs the
Host ABI, runs runtime initialization, trusted prepare and headroom reservation.
A partial pool is not published.

### DBI

```text
validate wrapper config
→ build typed drrun argv
→ ordinary file/RPC worker startup
→ DynamoRIO loads client
→ client initialization is client-defined
```

The Go wrapper has no standard “client ready” handshake. A client load failure
normally appears as worker startup/exit failure.

### QEMU

```text
validate wrapper config and readable artifacts
→ load strict manifest and recompute digests
→ resolve runtime bounds/accelerator/network
→ create private VM directory/socket
→ start QEMU
→ hello/ready + BootID
→ start exact evaluator bridge
→ only then expose capacity
```

A live QEMU PID without `ready` is not ready capacity.

## 7. Snapshot selection observability

Generic snapshot selection emits structured log fields:

```text
snapshot_requested
snapshot_selected
fallback_reason
```

A selected name must be read from runtime state; configuration alone does not
prove the mechanism remained active. Unsupported Linux/kernel/runtime cases can
select memcpy fallback.

Agent Python health/status adds:

```text
python_lifecycle
snapshot_mode
snapshot_selected
reset_mode
prepared_ready
prepared_hits
prepared_misses
prepared_refills
```

Its phase observer can record runtime creation, compilation, instantiation,
initialization, preparation, headroom, strategy selection, snapshot take,
checkout, execute, restore and decode with request/slot IDs, duration, memory
bytes, selected strategy and outcome.

Sources:

- [`snapshot_selection_log.go`](../../internal/execution/wasm/snapshot_selection_log.go)
- [Agent Python health](../../internal/execution/wasm/agent_python.go#L314-L342)
- [`agent_python_observer.go`](../../internal/execution/wasm/agent_python_observer.go#L57-L111)

## 8. Verification layers

Keep four evidence levels separate.

### Layer A: source/config contracts

```bash
go test ./internal/execution -count=1
go test ./internal/execution/supervisor ./internal/execution/worker -count=1
```

This verifies route construction, wrapper guards and process lifecycle mechanics.
It does not instantiate wazero COW, DynamoRIO or QEMU.

### Layer B: WASM package behavior

Portable:

```bash
go test ./internal/execution/wasm -count=1
```

Linux COW and dirty-page focus:

```bash
go test ./internal/execution/wasm \
  -run 'TestCow|TestAgentPython.*Snapshot|TestUffd|TestSoftDirty' \
  -count=1
```

Race checking:

```bash
go test -race ./internal/execution/wasm -count=1
```

COW syscall behavior must be tested on Linux. macOS exercises build-tagged
fallbacks, not memfd/MAP_FIXED behavior.

### Layer C: DBI process/wrapper behavior

```bash
go test ./internal/execution \
  -run 'TestApplyDBISecurityConfig|TestApplyExecutionWrappers' -count=1

go test ./internal/execution/supervisor ./internal/execution/worker -count=1
```

Real DynamoRIO enforcement requires Linux/amd64 plus the built client. The
compose smoke demonstrates loading/evaluation but is not an exhaustive denial
matrix.

### Layer D: QEMU package and guest behavior

```bash
go test -race ./internal/execution/qemurun ./internal/execution/qemuguest

go test ./cmd/shimmy-qemu-runner ./cmd/shimmy-qemu-guest \
  ./experiments/qemu-fallback/file-evaluator \
  ./experiments/qemu-fallback/rpc-evaluator

go vet ./internal/execution/qemurun ./internal/execution/qemuguest \
  ./cmd/shimmy-qemu-runner ./cmd/shimmy-qemu-guest
```

Full boot/image/transport parity additionally needs Linux, QEMU and generated
artifacts. The CI contract is in
[`.github/workflows/qemu-fallback.yml`](../../.github/workflows/qemu-fallback.yml#L60-L145).

## 9. Documentation gates

Run before merging architecture changes:

```bash
git diff --check

go test ./internal/execution/... ./cmd/shimmy-qemu-runner ./cmd/shimmy-qemu-guest

go vet ./internal/execution/... ./cmd/shimmy-qemu-runner ./cmd/shimmy-qemu-guest
```

Then verify every relative Markdown link resolves. Source line anchors are
navigation aids and must be updated if implementation edits move symbols.

Do not commit:

```text
.hermes/
dist/
local benchmark output
runtime credentials or copied environment dumps
```

## 10. Evidence and claim labels

Use these labels in reports and presentations:

| Label | Required backing |
|---|---|
| code-verified current | source and tests at the stated commit |
| lifecycle-validated | startup/request/reset/shutdown behavior, not performance |
| canonical measured | pinned artifact/plan/host and raw result source |
| bounded experiment | named fixture, platform and policy/mechanism scope |
| target/design | not implemented or not independently qualified |

Examples:

- Generic/Agent Python linear-memory restore is code-verified current.
- Linux COW mechanism is code-verified on Linux only; archived timings are not
  automatically current workload results.
- Checked-in DBI client coverage is bounded, not exhaustive native containment.
- QEMU source implements TCG/KVM selection, but historical TCG evidence does not
  qualify current KVM or Lambda deployment.

## 11. Operational failure checklist

### WASM request fails

1. Read requested and selected snapshot modes.
2. Check memory size drift and slot health.
3. Confirm response bytes were copied before restore.
4. Check whether the slot was retired and replacement creation completed.
5. For COW, confirm image coordinator remains open and image digest/size matched.

### DBI worker fails

1. Inspect final typed argv and interface without shell reconstruction.
2. Distinguish launcher failure, client load failure, policy denial and evaluator
   application error.
3. Confirm file vs RPC lifecycle before claiming fresh state.
4. Treat missing audit as separate from deny result.
5. Verify old process was reaped before replacement.

### QEMU worker fails

1. Separate artifact/config, VM start, hello/ready, evaluator start, transport,
   protocol and guest exit phases.
2. Record boot ID and reset policy.
3. Check whether network was explicitly enabled.
4. In lazy mode, prove old runner/QEMU exited before replacement.
5. In eager mode, inspect refill error and ensure shutdown did not publish a late
   replacement.

## 12. Release checklist

- [ ] route is explicit and DBI/QEMU conflict is tested;
- [ ] exact artifacts and digests are recorded;
- [ ] requested and selected reset mechanisms are observable;
- [ ] response ownership precedes reset/destruction;
- [ ] timeout/trap/protocol failure retires unsafe capacity;
- [ ] old process/guest exit is proven before clean replacement;
- [ ] external side effects are declared outside reset scope;
- [ ] evidence labels match what was actually run;
- [ ] portable, Linux-only and full-integration gates are distinguished;
- [ ] no credentials or local runtime artifacts enter documentation commits.
