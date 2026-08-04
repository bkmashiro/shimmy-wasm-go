# WASM, wazero and prepared-memory reset

> **Scope.** Current implementation on `feat/wasm-backend` at `5057978`.
> This document covers Generic WASM and the Agent Python profile because both
> execute inside wazero and use the same snapshot strategy interface. The older
> resident `python-wasm` comparison path is not the canonical Python reactor.

## 1. What wazero is responsible for

Shimmy embeds wazero `v1.11.0`. Wazero supplies:

- WASM validation and compilation;
- one `wazero.Runtime` and one cached `wazero.CompiledModule` per dispatcher;
- per-slot `api.Module` instances;
- the WASI preview1 Host module;
- function lookup/call, context-driven interruption and module close;
- the `api.Memory` view used for request/response transfer;
- the experimental memory-allocator hook used by Linux prepared-memory COW.

Shimmy, not wazero, supplies:

- deployment-time profile selection;
- Host capabilities in `ModuleConfig`;
- pool size, checkout, timeout, replacement and shutdown;
- the guest ABI and response bounds checks;
- when a prepared baseline is captured;
- the linear-memory reset strategy;
- the decision to reuse, close or replace a slot.

The division is important: wazero isolates WASM address calculations, but Shimmy
owns lifecycle and reuse semantics.

## 2. Runtime and instance topology

### 2.1 Generic WASM

```text
Dispatcher
├── wazero.Runtime                         shared
├── wazero.CompiledModule                  shared
├── wazero.ModuleConfig                    shared template
├── cowImageCoordinator                    optional, shared in cow mode
└── pool chan *wasmSupervisor
    ├── slot 0
    │   ├── api.Module                     exclusive
    │   ├── api.Memory                     exclusive
    │   ├── wasmAdapter                    exclusive
    │   └── SnapshotStrategy               exclusive
    ├── slot 1
    └── ...
```

Source:
[`internal/execution/wasm/dispatcher.go`](../../internal/execution/wasm/dispatcher.go#L24-L59).

Compilation is paid once. Instantiation and mutable state are paid once per
ready slot. A slot mutex prevents concurrent calls into one module; the channel
pool provides concurrency across slots.

### 2.2 Agent Python

```text
AgentPythonDispatcher
├── wazero.Runtime                         shared
├── wazero.CompiledModule                  shared
├── verified runtime artifact/manifest     shared
├── trusted evaluator bundle               shared input
├── slots semaphore                        execution concurrency
└── prepared chan *agentPythonModuleSlot
    ├── slot
    │   ├── api.Module
    │   ├── prepared CPython heap in linear memory
    │   ├── SnapshotStrategy (snapshot lifecycle only)
    │   ├── baselineSize
    │   ├── diagnostic buffer
    │   └── cowImageCoordinator            per slot in this branch
    └── ...
```

The Python dispatcher exposes three explicit lifecycles:

- `snapshot`: restore the prepared linear-memory image and reuse the module;
- `single-use`: consume a never-served prepared module and refill capacity;
- `fresh`: initialize and close a module for every request.

Source:
[`AgentPythonDispatcher.Start`](../../internal/execution/wasm/agent_python.go#L111-L311)
and
[`AgentPythonDispatcher.Send`](../../internal/execution/wasm/agent_python.go#L314-L475).

## 3. Generic WASM startup

`Dispatcher.Start` performs these operations in order:

1. apply `FUNCTION_WASM_*` environment overrides and defaults;
2. validate snapshot mode, capability and memory settings;
3. read the `.wasm` bytes from `ModulePath`;
4. create the wazero runtime configuration;
5. instantiate WASI preview1 in that runtime;
6. compile the module once;
7. create a locked-down `ModuleConfig`;
8. validate strategy/process-wide concurrency restrictions;
9. create the optional dispatcher COW image coordinator;
10. instantiate and initialize every supervisor slot;
11. publish the ready slots into the bounded pool.

Source:
[`Dispatcher.Start`](../../internal/execution/wasm/dispatcher.go#L91-L218).

The `ModuleConfig` starts from no ambient filesystem or environment. Only
explicit allowlists are mounted/exported. Memory maximum pages are passed to
wazero's runtime configuration; this is separate from how a snapshot is stored.

## 4. Generic guest ABI and request memory

A Generic WASM guest exports:

```wat
(func (export "alloc") (param i32) (result i32))
(func (export "evaluate") (param i32 i32) (result i32))
(memory ...)
```

The request is JSON:

```json
{"method":"<method>","params":{}}
```

The response at returned pointer `P` is:

```text
P + 0 .. P + 3       uint32 little-endian payload length L
P + 4 .. P + 4 + L   UTF-8 JSON bytes
```

Source:
[`internal/execution/wasm/adapter.go`](../../internal/execution/wasm/adapter.go#L1-L25).

### 4.1 One request

`wasmAdapter.send` executes:

```text
Host map[string]any
  -> json.Marshal
  -> guest alloc(reqLen)
  -> api.Memory.Write(reqPtr, requestBytes)
  -> guest evaluate(reqPtr, reqLen)
  -> api.Memory.Read(responsePtr, 4)
  -> validate responsePtr + 4 + responseLen <= mem.Size()
  -> api.Memory.Read(responsePtr + 4, responseLen)
  -> json.Unmarshal into Host map
```

Source:
[`wasmAdapter.send`](../../internal/execution/wasm/adapter.go#L64-L170).

The returned Go map is Host-owned before `wasmSupervisor.Send` restores the
module. `api.Memory.Read` itself may expose a guest-backed slice; therefore the
adapter does not retain that slice after JSON unmarshal.

### 4.2 Timeout

The adapter derives a request context with `context.WithTimeout`. Wazero calls
receive that context. A timeout/trap is returned to the supervisor. The
supervisor still attempts reset; if state cannot be proven clean, it marks the
slot unhealthy and the dispatcher replaces it.

## 5. Generic snapshot timing

A Generic supervisor starts one module:

```text
InstantiateModule(compiled, moduleConfig)
  -> optional exported _initialize()
  -> optional exported _start()
  -> select snapshot strategy
  -> strategy.Take(module.Memory())
  -> ready
```

Source:
[`wasmSupervisor.Start`](../../internal/execution/wasm/supervisor.go#L70-L138).

The baseline therefore includes all guest state represented in linear memory
after module initialization and before the first untrusted request.

It does **not** include a complete clone of:

- mutable WASM globals;
- tables or reference-typed state;
- Host/WASI descriptor tables and offsets;
- Host RNG/clock state;
- goroutines, Go objects or external services.

Those are either excluded by the guest/profile contract or require slot
retirement rather than byte restoration.

## 6. Agent Python initialization and snapshot timing

Agent Python uses a richer runtime ABI:

```text
_initialize
runtime_init
runtime_prepare
alloc / dealloc
execute
agent_runtime_v1.host_call
```

A prepared module is constructed as:

```text
InstantiateModule
  -> _initialize
  -> runtime_init({})
  -> runtime_prepare(trusted evaluator)       when preload mode is enabled
  -> reserve allocator headroom
  -> release headroom allocations
  -> select snapshot strategy
  -> strategy.Take(linear memory)
  -> record baselineSize
  -> ready slot
```

Source:

- [`newInitializedModule`](../../internal/execution/wasm/agent_python.go#L527-L595);
- [`reserveAgentPythonSnapshotHeadroom`](../../internal/execution/wasm/agent_python.go#L597-L636);
- [`newPreparedModuleSlot`](../../internal/execution/wasm/agent_python.go#L638-L718).

### 6.1 Why reserve then deallocate headroom

The Host calls exported `alloc` in 1 MiB chunks until the configured headroom has
been committed, then calls `dealloc` in reverse order. The logical allocations
are released, but the WASM memory has already grown enough to serve expected
request allocations. Because snapshot/COW V1 requires fixed linear-memory size,
this moves planned growth into trusted preparation.

If a later request still changes `memory.Size()`, restore fails closed.

### 6.2 Python request/response ownership

`callAgentPythonExecute` reads the response frame, checks the maximum payload and
bounds, then performs:

```go
append([]byte(nil), payload...)
```

Source:
[`readAgentPythonResponse`](../../internal/execution/wasm/agent_python.go#L966-L990).

That explicit copy is why the dispatcher can restore guest memory before JSON
decode:

```text
execute
  -> validate and copy payload to Host []byte
  -> restore snapshot
  -> return slot to prepared pool
  -> decode Host-owned payload
```

Source:
[`AgentPythonDispatcher.Send`](../../internal/execution/wasm/agent_python.go#L428-L475).

## 7. SnapshotStrategy contract

Every strategy implements:

```go
Take(api.Memory) error
Restore(api.Memory) error
Close() error
```

Contract:

- `Take` precedes `Restore`;
- repeated `Take` replaces the baseline;
- strategies are not concurrently safe;
- the supervisor/slot serializes calls;
- restore validates the captured extent;
- `Close` releases OS resources.

Source:
[`internal/execution/wasm/snapshot.go`](../../internal/execution/wasm/snapshot.go#L10-L41).

The available modes are:

| Mode | Baseline storage | Dirty detection | Restore operation | Important restriction |
|---|---|---|---|---|
| `memcpy` | owned Go `[]byte` | none | copy every byte with `api.Memory.Write` | O(total extent) |
| `soft-dirty` | owned Go `[]byte` | Linux PTE soft-dirty bits | copy only pages reported by `/proc/self/pagemap` | process-wide bits; one instance |
| `mprotect` | owned Go `[]byte` | `PROT_READ` + SIGSEGV bitmap | copy dirty pages and reprotect | experimental cgo; process-wide handler; one instance |
| `uffd` | owned Go `[]byte` | userfaultfd write-protect events | copy dirty pages and re-arm full range | kernel/seccomp availability; one instance |
| `cow` | sealed memfd image | kernel private COW pages | replace mapping with fresh `MAP_PRIVATE` view | Linux allocator hook; fixed size after attach |

Selection and explicit fallbacks are logged by
[`logSnapshotSelection`](../../internal/execution/wasm/snapshot_selection_log.go).

## 8. Full-copy layout and reset

```text
Go heap snapshot:  [prepared bytes 0 ................................ N)
WASM live memory:  [request-mutated bytes 0 .......................... N)
```

`Take` reads `[0, mem.Size())`, allocates an owned Go slice and copies the bytes.
`Restore` first requires `mem.Size() == capturedSize`, then calls
`api.Memory.Write(0, snapshot)`.

Source:
[`FullMemcpyStrategy`](../../internal/execution/wasm/snapshot.go#L47-L111).

There is no Linux reset syscall in the hot restore path. The Go/wazero memory
copy writes the complete captured extent.

## 9. Prepared-memory COW: the two sides

COW is not process `fork`, not a wazero module clone and not dirty-page copying.
It is a fixed-address private mapping of an immutable prepared linear-memory
image.

### 9.1 Side A: canonical image

```text
cowImageCoordinator
  -> cowImage
      fd      = memfd "shimmy-cow-image"
      size    = prepared visible linear-memory length
      digest  = SHA-256(prepared bytes)
      seals   = SEAL_SEAL | SEAL_SHRINK | SEAL_GROW | SEAL_WRITE
```

The first prepared slot publishes the image. Later Generic WASM slots must match
both size and SHA-256 before attachment. A mismatch selects full-copy fallback
for that slot instead of attaching the wrong image.

Source:
[`PublishOrAttach`](../../internal/execution/wasm/snapshot_cow_linux.go#L61-L104).

Creation syscalls, in order:

```text
memfd_create("shimmy-cow-image", MFD_CLOEXEC | MFD_ALLOW_SEALING)
ftruncate(fd, preparedSize)
pwrite(fd, preparedBytes, offset)        loop until complete
fcntl(fd, F_ADD_SEALS,
      F_SEAL_SEAL | F_SEAL_SHRINK | F_SEAL_GROW | F_SEAL_WRITE)
```

Source:
[`newCowImage`](../../internal/execution/wasm/snapshot_cow_linux.go#L122-L155).

The fd remains open for the dispatcher/slot lifetime because reset needs to map
from it again. Existing mappings survive fd close, but future reset would fail;
therefore coordinator shutdown occurs only after all slots are closed.

### 9.2 Side B: each slot's stable virtual range

Before attachment, the custom wazero allocator owns:

```text
virtual base B

B                                                B + maximum
|----------------------------------------------------------|
| anonymous MAP_PRIVATE | MAP_ANON, PROT_READ | PROT_WRITE |
|----------------------------------------------------------|
|<---- visible length ---->|<----- reserved capacity ------>|
```

The allocation syscall is:

```text
mmap(NULL, maximum,
     PROT_READ | PROT_WRITE,
     MAP_PRIVATE | MAP_ANON,
     -1, 0)
```

Source:
[`newCowLinearMemory`](../../internal/execution/wasm/snapshot_cow_linux.go#L157-L182).

Wazero receives this region through
`experimental.WithMemoryAllocator`. During trusted initialization,
`Reallocate` can expose more of the reserved range and zero the newly visible
bytes. The base address remains stable.

Source:

- [`cowMemoryAllocator.Allocate`](../../internal/execution/wasm/snapshot_cow_linux.go#L332-L359);
- [`cowRuntimeSupport.instantiateContext`](../../internal/execution/wasm/snapshot_cow_linux.go#L519-L535).

### 9.3 Attachment

At snapshot time, the visible prefix is replaced in-place:

```text
before attach
B: [anonymous prepared bytes........................][anonymous reserved tail]

MAP_FIXED | MAP_PRIVATE from sealed memfd over visible prefix

B: [private view of immutable memfd image...........][anonymous reserved tail]
    ^ shared clean physical pages until a slot writes
```

The exact syscall is:

```text
mmap(B, preparedSize,
     PROT_READ | PROT_WRITE,
     MAP_PRIVATE | MAP_FIXED,
     imageFd, 0)
```

Source:
[`remapCowImage`](../../internal/execution/wasm/snapshot_cow_linux.go#L308-L330).

`MAP_FIXED` is necessary because wazero and compiled code already refer to the
stable base. The return address is checked against `B`; any drift is an error.

### 9.4 After a request writes

For two slots attached to the same Generic image:

```text
sealed memfd / page cache
  page 0  page 1  page 2  page 3  ...
    |       |       |       |
    +-------+-------+-------+-----------------------+
            clean shared mappings                    |
                                                     |
slot A page table: shared  private-A  shared  ...    |
slot B page table: shared  shared     private-B ...  |
```

A write to a clean `MAP_PRIVATE` page faults in the kernel and creates an
anonymous private page for that slot. The sealed file is unchanged. No Shimmy
SIGSEGV handler or dirty bitmap participates in COW mode.

The reserved capacity tail is not part of the baseline. After attachment,
`Reallocate` rejects any size change. This is why `memory.grow` fails in COW V1.

Source:
[`cowLinearMemory.Reallocate`](../../internal/execution/wasm/snapshot_cow_linux.go#L184-L200)
and test
[`TestCowWazeroMemoryGrowthFailsAfterPreparedImageAttach`](../../internal/execution/wasm/snapshot_cow_wazero_linux_test.go#L48-L72).

### 9.5 Reset syscall

Reset does not iterate dirty pages. It validates:

- the allocator has not been freed;
- an image is attached;
- the image fd remains open;
- visible length equals image size;
- the stable region still covers the visible prefix.

It then issues the same fixed-address mapping call:

```text
mmap(B, preparedSize,
     PROT_READ | PROT_WRITE,
     MAP_PRIVATE | MAP_FIXED,
     imageFd, 0)
```

The new mapping replaces the old private mapping at `B`. Kernel references to
request-private anonymous pages are dropped; clean pages again fault/read from
the sealed image. This is the entire COW reset operation.

Source:
[`cowLinearMemory.Reset`](../../internal/execution/wasm/snapshot_cow_linux.go#L288-L306).

Important consequences:

- reset cost is mapping/page-table work, not a Host copy of all bytes;
- later reads may pay page faults, so reset-only timing is not whole-request cost;
- virtual address is preserved;
- private pages are discarded by remap;
- the memfd baseline is never mutated;
- external state is unaffected.

### 9.6 Free and physical unmap

Wazero may call `experimental.LinearMemory.Free` while native call frames are
still unwinding. `Free` therefore only invalidates logical use; it does **not**
call `munmap` immediately. After module execution is over, allocator `Close`
invokes `Release`, which performs:

```text
munmap(entire reserved region)
```

Source:
[`Free` and `Release`](../../internal/execution/wasm/snapshot_cow_linux.go#L202-L236).

This separation avoids revoking memory while wazevo may still reference the
address during termination unwind.

## 10. Generic-shared versus Agent-Python-per-slot COW

### Generic WASM

`Dispatcher.Start` creates one `cowImageCoordinator` and passes it to every
supervisor. The first matching slot publishes; all later matching slots attach.
Replacement supervisors use the same coordinator.

Source:
[`internal/execution/wasm/dispatcher.go`](../../internal/execution/wasm/dispatcher.go#L187-L203).

### Agent Python on this branch

`newPreparedModuleSlot` creates a new coordinator when that slot is created in
snapshot+COW mode. The image is therefore sealed and private to the slot's
lifecycle, even though the mechanism is the same.

Source:
[`newPreparedModuleSlot`](../../internal/execution/wasm/agent_python.go#L638-L679).

Do not document this branch as having one shared Agent Python memfd across all
prepared slots. A later worktree may implement a different ownership topology.

## 11. Soft-dirty reset

Soft-dirty uses a full baseline copy plus Linux PTE metadata:

```text
Take:
  copy complete live range -> Go snapshot
  write "4" -> /proc/self/clear_refs

request:
  kernel sets soft-dirty PTE bit on writes

Restore:
  pread entries from /proc/self/pagemap
  identify bit 55
  copy only dirty pages from snapshot to live memory
  write "4" -> /proc/self/clear_refs
```

Source:
[`internal/execution/wasm/snapshot_soft_dirty_linux.go`](../../internal/execution/wasm/snapshot_soft_dirty_linux.go).

The memory backing is pinned while virtual addresses are used. Because
`clear_refs` is process-wide, configuration validation limits this mode to one
instance.

## 12. mprotect/SIGSEGV reset

This experimental cgo strategy:

1. installs a process-wide `sigaction(SIGSEGV, SA_SIGINFO | SA_ONSTACK | SA_NODEFER)`;
2. allocates a C atomic bitmap, one bit per page;
3. snapshots all bytes;
4. calls `mprotect(range, PROT_READ)`;
5. on a write fault inside the tracked range:
   - atomically marks the page dirty;
   - calls `mprotect(page, PROT_READ | PROT_WRITE)`;
   - returns so the faulting write retries;
6. restore copies dirty pages from the baseline;
7. calls `mprotect(range, PROT_READ)` again and clears the bitmap.

Source:
[`internal/execution/wasm/snapshot_mprotect_linux.go`](../../internal/execution/wasm/snapshot_mprotect_linux.go).

It is explicitly gated by
`FUNCTION_WASM_ALLOW_EXPERIMENTAL_MPROTECT=true`, is cgo-only and is limited to
one live strategy because the handler and bitmap are process-global. Unrelated
SIGSEGV handling is chained to the previous handler, which is why this mode is
not a default.

## 13. userfaultfd reset

UFFD write-protect tracking uses:

```text
userfaultfd(O_CLOEXEC | O_NONBLOCK)
ioctl(UFFDIO_API, request WP feature)
ioctl(UFFDIO_REGISTER, range, MODE_WP)
ioctl(UFFDIO_WRITEPROTECT, full range, WP=true)
```

A handler goroutine polls/reads `uffd_msg` page-fault events. For a WP fault it
marks the page dirty and removes WP for that page so execution continues.

Restore:

1. stop accepting request writes under the slot's serialized lifecycle;
2. copy only recorded dirty pages from the owned full baseline;
3. re-arm `UFFDIO_WRITEPROTECT` over the **entire registered range**;
4. clear the dirty bitmap for the next request.

Source:
[`internal/execution/wasm/snapshot_uffd_linux.go`](../../internal/execution/wasm/snapshot_uffd_linux.go).

UFFD is conditional on kernel features, sysctl/seccomp policy and registration
success. Selection failure is logged and falls back to full copy. It is not the
mechanism used by COW.

## 14. Per-request restore and health

Generic supervisor request order:

```text
lock slot
adapter.send
restoreSnapshot
unlock
```

`restoreSnapshot` checks exact memory size. If memory grew, it zeroes the grown
tail as defense in depth but returns `ErrSnapshotMemoryDrifted`; WASM memory
cannot shrink, so the slot is unhealthy and never reused.

Source:
[`wasmSupervisor.Send`](../../internal/execution/wasm/supervisor.go#L140-L168)
and
[`restoreSnapshot`](../../internal/execution/wasm/supervisor.go#L228-L257).

Agent Python snapshot mode similarly requires `memory.Size() == baselineSize`
before calling the strategy.

Source:
[`restoreAgentPythonSnapshot`](../../internal/execution/wasm/agent_python.go#L721-L733).

## 15. Strategy fallback behavior

Requested mode and selected mode are distinct observability fields. Examples:

- unsupported platform COW -> `memcpy`;
- failed custom allocator -> heap memory + `memcpy`;
- prepared-image digest mismatch -> per-slot `memcpy`;
- unavailable `/proc`, UFFD or mprotect prerequisites -> `memcpy`;
- invalid mode -> configuration error before startup.

A fallback is logged with:

```text
requested=<mode>
selected=memcpy
fallback_reason=<reason>
```

COW attachment loss **after** successful selection is not a fallback opportunity
during request reset. It is a restore failure, so the slot is retired.

## 16. Capability and memory boundaries

The linear-memory strategies do not replace WASI configuration. The runtime must
still bound:

- maximum WASM memory pages;
- request timeout;
- allowed preopened paths;
- allowed environment variables;
- input/output size;
- module/artifact identity;
- Host imports.

Reset restores only what is represented in the captured linear-memory image.
Mutable state elsewhere must be absent, trusted, independently reset or handled
by closing the entire slot.

## 17. Source and test map

| Behavior | Source/test |
|---|---|
| compile once, N instances | [`dispatcher.go`](../../internal/execution/wasm/dispatcher.go) |
| Generic ABI bounds | [`adapter.go`](../../internal/execution/wasm/adapter.go) |
| slot serialization and restore | [`supervisor.go`](../../internal/execution/wasm/supervisor.go) |
| snapshot interface/full copy | [`snapshot.go`](../../internal/execution/wasm/snapshot.go) |
| COW allocator/image/remap | [`snapshot_cow_linux.go`](../../internal/execution/wasm/snapshot_cow_linux.go) |
| real multi-instance shared image | [`snapshot_cow_wazero_linux_test.go`](../../internal/execution/wasm/snapshot_cow_wazero_linux_test.go) |
| COW VM accounting evidence | [`snapshot_cow_evidence_linux_test.go`](../../internal/execution/wasm/snapshot_cow_evidence_linux_test.go) |
| strategy selection/fallback | [`supervisor_linux.go`](../../internal/execution/wasm/supervisor_linux.go) |
| soft-dirty | [`snapshot_soft_dirty_linux.go`](../../internal/execution/wasm/snapshot_soft_dirty_linux.go) |
| mprotect/SIGSEGV | [`snapshot_mprotect_linux.go`](../../internal/execution/wasm/snapshot_mprotect_linux.go) |
| UFFD WP | [`snapshot_uffd_linux.go`](../../internal/execution/wasm/snapshot_uffd_linux.go) |
| Python prepared lifecycle | [`agent_python.go`](../../internal/execution/wasm/agent_python.go) |
| Python ABI/frame validation | [`agent_python_protocol.go`](../../internal/execution/wasm/agent_python_protocol.go) |

## 18. Verification commands

Portable/unit coverage:

```bash
go test ./internal/execution/wasm
```

Linux COW tests:

```bash
go test ./internal/execution/wasm \
  -run 'TestCow|TestAgentPython.*Cow|TestCowDispatcher'
```

Race-sensitive pool/lifecycle checks:

```bash
go test -race ./internal/execution/wasm
```

Evidence benchmarks are diagnostics and must not be converted into production
latency claims without their exact environment and workload:

```bash
go test ./internal/execution/wasm \
  -run '^$' \
  -bench 'BenchmarkCow|BenchmarkSnapshot'
```
