# QEMU path: whole-guest execution and replacement lifecycle

> **Scope.** Current implementation on `feat/wasm-backend` at `5057978`.
> Historical QEMU evidence is version-bound; this document describes current
> source behavior and does not claim current KVM or Lambda qualification.

[Architecture index](README.md) · [WASM path](wasm-wazero-memory.md) ·
[DBI path](dbi-native-path.md)

## 1. Position in Shimmy

QEMU is a pre-dispatch command wrapper around an existing `file` or `rpc`
evaluator contract. It is not a new application protocol:

```text
original evaluator command + file/RPC contract
                  │
                  ▼
          applyExecutionWrappers
                  │ QEMU enabled; DBI must be disabled
                  ▼
          applyQEMUFallbackConfig
                  │
                  ▼
shimmy-qemu-runner -- original-command [original args]
                  │
                  ▼
       QEMU process + Linux guest
                  │
                  ▼
  shimmy-qemu-guest starts exact evaluator
```

Sources:

- [`applyExecutionWrappers`](../../internal/execution/dispatcher_qemu.go#L20-L38)
- [`applyQEMUFallbackConfig`](../../internal/execution/dispatcher_qemu.go#L41-L117)
- [`NewDispatcher`](../../internal/execution/dispatcher.go#L42-L180)

The wrapper preserves the outer file/RPC interface, cwd, environment and
transport selection. The runner command defaults to `shimmy-qemu-runner`.
Arguments remain typed Go argv; no shell joins or re-parses them.

## 2. Configuration surface

| Variable | Current behavior |
|---|---|
| `FUNCTION_QEMU_ENABLED` | Boolean enablement. |
| `FUNCTION_QEMU_RUNNER` | Host runner; default `shimmy-qemu-runner`. |
| `FUNCTION_QEMU_BINARY` | QEMU executable path; required. |
| `FUNCTION_QEMU_ROOTFS` | Configured rootfs; required and compared with manifest rootfs. |
| `FUNCTION_QEMU_IMAGE_MANIFEST` | Image manifest; required. |
| `FUNCTION_QEMU_RESET_POLICY` | `off`, `lazy` or `eager`. |
| `FUNCTION_QEMU_ACCELERATOR` | `auto`, `tcg` or `kvm`; default `auto`. |
| `FUNCTION_QEMU_MEMORY_MB` | Default 512; range 1..32768. |
| `FUNCTION_QEMU_VCPUS` | Default 1; range 1..64. |
| `FUNCTION_QEMU_NETWORK_PROFILE` | `none` or explicit `inherit`/`user`. |
| `FUNCTION_QEMU_MAX_FRAME_BYTES` | Default 4 MiB; range 1 KiB..64 MiB. |
| `FUNCTION_QEMU_BOOT_TIMEOUT` | Default 60 s; bounded 10 ms..10 min. |
| `FUNCTION_QEMU_SHUTDOWN_TIMEOUT` | Default 10 s; bounded 10 ms..2 min. |
| `FUNCTION_QEMU_WORK_ROOT` | Default `/tmp/shimmy-qemu`. |

The outer wrapper first checks that binary, rootfs and manifest paths are
readable. Full manifest and runtime validation happen inside the runner.

Sources:

- [wrapper validation](../../internal/execution/dispatcher_qemu.go#L55-L117)
- [`LoadRuntimeConfig`](../../internal/execution/qemurun/runtime.go#L24-L164)

## 3. Host/guest ownership topology

```text
Shimmy HTTP/Lambda handler
  └─ EvaluationRuntime
      └─ Dispatcher
          └─ WorkerSupervisor
              └─ shimmy-qemu-runner process
                  ├─ private work directory
                  ├─ control.sock
                  └─ QEMU child process
                      ├─ read-only guest rootfs
                      ├─ virtio-serial control port
                      └─ shimmy-qemu-guest
                          └─ exact evaluator process
```

Each VM receives:

```text
FUNCTION_QEMU_WORK_ROOT/
  vm-<random>/             mode 0700
    control.sock           host Unix socket
```

`VM.Close` owns control-connection close, QEMU termination, wait and directory
removal. The runner defers it for every execution path.

The guest image contains BusyBox/init, `shimmy-qemu-guest`, evaluator binaries,
`/dev`, `/proc`, `/sys`, `/run` and `/tmp`. Guest init mounts `/run` and `/tmp` as
`nosuid,nodev` tmpfs and waits for `/dev/virtio-ports/org.shimmy.control`.

Sources:

- [VM directory ownership](../../internal/execution/qemurun/vm.go#L42-L146)
- [guest image construction](../../experiments/qemu-fallback/build-image.sh#L90-L139)
- [guest executable entry](../../cmd/shimmy-qemu-guest/main.go#L26-L45)

## 4. Manifest and image validation

The schema-v1 manifest contains architecture, source-lock digest, and kernel,
initrd and rootfs path/digest records. Runtime loading:

1. limits the manifest to 1 MiB;
2. rejects unknown JSON fields and trailing values;
3. requires schema version 1;
4. requires `x86_64`;
5. allows rootfs format `raw` or `qcow2`;
6. resolves artifacts;
7. requires regular files;
8. recomputes SHA-256 for kernel, initrd and rootfs;
9. requires configured rootfs and manifest rootfs to be the same file.

`source_lock_sha256` is syntax/length checked but the runtime does not load a
source-lock file and compare it. Absolute artifact paths are accepted; the
loader does not require containment beneath the manifest directory.

Sources:

- [`LoadImageManifest`](../../internal/execution/qemurun/manifest.go#L46-L194)
- [manifest schema](../../experiments/qemu-fallback/image-manifest.schema.json)

## 5. QEMU command and isolation-relevant devices

`BuildQEMUCommand` constructs direct argv equivalent to:

```text
-no-user-config
-nodefaults
-nographic
-display none
-monitor none
-serial none
-no-reboot
-machine accel=<tcg|kvm>
-smp <vcpus>
-m <memory>M
-kernel <kernel>
-initrd <initrd>
-append "root=/dev/vda ro rootfstype=squashfs init=/init panic=-1 reboot=t"
-drive file=<rootfs>,format=<raw|qcow2>,if=virtio,readonly=on
-device virtio-serial-pci
-chardev socket,id=shimmy,path=<control.sock>,server=on,wait=off
-device virtserialport,chardev=shimmy,name=org.shimmy.control
-nic none
```

The explicit user-network profile substitutes:

```text
-nic user,model=virtio-net-pci
```

There is no monitor, serial console, display or default device set. The rootfs
is read-only. Network is absent by default; enabling user networking grants NAT
egress and is not a destination allowlist.

Source: [`BuildQEMUCommand`](../../internal/execution/qemurun/command.go#L46-L115).

## 6. Control protocol

All host/guest traffic uses a private virtio-serial channel backed by the host
Unix socket. A frame is:

```text
4-byte big-endian body length
1-byte frame type
4-byte big-endian stream ID
payload bytes
```

The body length includes the five metadata bytes. Control frames require stream
ID 0; stream frames require non-zero IDs.

Frame types:

```text
hello, ready, start, exit,
file_request, file_result,
open, data, half_close, close, stream_error, cancel
```

The codec rejects oversized payloads, truncated frames, unknown types and wrong
control/stream IDs. `StreamTracker` bounds active streams and validates duplicate,
unknown and repeated-half-close transitions. `SerializedFrameWriter` prevents
concurrent writers from interleaving bytes.

Sources:

- [`Codec`](../../internal/execution/qemurun/protocol.go#L11-L161)
- [`StreamTracker`](../../internal/execution/qemurun/protocol.go#L195-L279)
- [`SerializedFrameWriter`](../../internal/execution/qemurun/writer.go)

## 7. Boot and readiness

Process existence is not guest readiness. The sequence is:

```text
runner starts QEMU
  → waits for control.sock
  → connects and sets boot deadline
  → sends hello {version: 1}
  → guest validates protocol
  → guest sends ready {version: 1, boot_id}
  → runner validates version
  → runner clears boot deadline
  → runner sends start
```

The guest reads `boot_id` from `/proc/sys/kernel/random/boot_id`, with fallback
when unavailable. `BootID` distinguishes guest boots. The QEMU protocol currently
contains no Lambda invocation ID; stream IDs identify multiplexed connections,
not requests.

Sources:

- [runner boot sequence](../../cmd/shimmy-qemu-runner/main.go#L44-L75)
- [host client handshake](../../internal/execution/qemurun/client.go#L44-L62)
- [guest `Serve`](../../internal/execution/qemuguest/server.go#L21-L56)
- [boot ID](../../internal/execution/qemuguest/server.go#L117-L128)

## 8. File mode end to end

### 8.1 Outer host adapter

The ordinary file adapter creates host request/response files and launches:

```text
shimmy-qemu-runner -- evaluator [args] host-request host-response
```

with `EVAL_IO=FILE` and host path variables. The runner validates invocation
shape, manifest/runtime config and host paths, boots the VM and calls `RunFile`.

The host request must be a regular file and fit the frame bound. The host
response must initially be a regular file; it is reopened with
`O_WRONLY|O_TRUNC`. The current `Lstat` then `OpenFile` sequence does not use
`O_NOFOLLOW`, so the validation/open pair has a check/use race.

### 8.2 Guest execution

`qemuguest.ExecuteFile`:

1. creates `/run/shimmy` mode `0700`;
2. creates a per-request directory;
3. creates `request.json` and `response.json` mode `0600`;
4. writes raw request bytes;
5. appends guest paths to evaluator argv;
6. sets guest `EVAL_IO=FILE` and path variables;
7. runs the exact command with supplied cwd and env;
8. captures stdout/stderr and exit status;
9. reads the response;
10. removes the guest request directory.

The guest sends `file_result` then `exit`. The runner verifies both exit codes
agree, copies response bytes to the outer host response file, writes evaluator
stdout to runner stdout, and turns non-zero evaluator exit into
`RemoteExitError`.

Sources:

- [host runner file path](../../cmd/shimmy-qemu-runner/main.go#L44-L200)
- [`RunFile`](../../internal/execution/qemurun/client.go#L23-L95)
- [`ExecuteFile`](../../internal/execution/qemuguest/file.go#L39-L105)
- [guest server dispatch](../../internal/execution/qemuguest/server.go#L57-L91)

File mode is already one process per request in the outer supervisor. Because
each runner owns one VM and defers `VM.Close`, it is also one guest boot per
request.

## 9. RPC stdio mode

The outer RPC adapter connects its stdin/stdout pipes to the runner. QEMU acts as
a raw byte tunnel:

```text
outer JSON-RPC/LSP bytes
  ↔ runner stdin/stdout
  ↔ virtio data frames (stream 1)
  ↔ guest evaluator stdin/stdout
```

Sequence:

1. host performs `hello`/`ready`;
2. sends `start {mode: rpc, transport: stdio, command, args, cwd, env}`;
3. sends `open {stream_id: 1}`;
4. copies outer stdin bytes into `data` frames;
5. copies guest `data` frames to outer stdout;
6. maps outer EOF to `half_close`;
7. maps evaluator exit to `exit`.

Neither bridge parses the application RPC payload. JSON-RPC framing remains
above the QEMU tunnel. Guest evaluator stderr is retained as a bounded 64 KiB
tail for exit diagnostics.

Sources:

- [`RunRPCStdio`](../../internal/execution/qemurun/rpc_stdio.go#L12-L140)
- [guest stdio bridge](../../internal/execution/qemuguest/rpc.go#L19-L143)

## 10. RPC network modes

IPC, TCP, HTTP and WebSocket retain their outer transport shape. The host runner
listens on the configured endpoint and multiplexes accepted byte streams over
virtio frames. It supports at most 64 simultaneous streams in the runner entry.

Host flow:

```text
accept host connection
  → allocate non-zero stream ID
  → send open
  → connection bytes ↔ data frames
  → half-close/close/error mapping
  → guest exit terminates session
```

Guest endpoint translation:

| Outer transport | Guest evaluator endpoint |
|---|---|
| IPC | `/run/shimmy/evaluator.sock` |
| TCP | `127.0.0.1:<same port>` |
| HTTP | same URL shape, host rewritten to `127.0.0.1` |
| WebSocket | same URL shape, host rewritten to `127.0.0.1` |

The guest adds only the selected transport's `EVAL_RPC_*` override, starts the
exact evaluator and dials it locally. HTTP/WebSocket semantics are handled by
the evaluator and outer client; the QEMU bridge copies bytes.

Sources:

- [host network bridge](../../internal/execution/qemurun/rpc_network.go#L12-L228)
- [endpoint translation](../../internal/execution/qemurun/endpoint.go#L37-L64)
- [guest network bridge](../../internal/execution/qemuguest/rpc_network.go#L19-L235)

## 11. Reset policies and exact lifecycle

QEMU does not implement `savevm`, `loadvm`, guest RAM snapshots or memory COW.
A clean state means a new guest boot from the read-only image.

### 11.1 `off`

```text
Start: boot one RPC VM
Request N: reuse it
Request N+1: reuse the same boot ID and guest state
Shutdown: stop and reap VM
```

This is the non-Lambda default for RPC. It is a compatibility control, not an
independent-request policy. File mode remains transient because the file
supervisor itself is transient.

### 11.2 `lazy`

```text
Request
  → synchronously boot runner + VM if absent
  → wait hello/ready
  → execute
  → detach worker
  → Stop runner/VM
  → wait for process exit using background context
  → return from supervisor Send
Next request
  → boot a new runner + VM
```

The wrapper maps `lazy` to `WorkerLifecycleInvocation`. Lambda defaults to lazy.
`releaseBeforeReturn` makes old-worker termination part of the request completion
path: the next invocation cannot start while an old VM is merely signalled but
not reaped.

### 11.3 `eager`

Eager is RPC-only and non-Lambda-only:

```text
Start
  → synchronously boot first ready VM

Request N
  → use ready VM
  → detach served worker while sendLock is held
  → start background refill goroutine
       1. stop old worker
       2. wait for old process exit
       3. boot replacement
       4. publish replacement only if supervisor still open
  → return result

Request N+1
  → use published replacement, or wait for refill completion
```

A replacement is never booted before the old worker is fully stopped and waited.
If refill fails, the next request observes the refill error. Shutdown marks the
supervisor closed, stops any published ready worker and waits for in-flight
refill goroutines. A replacement finishing after shutdown is stopped rather than
published.

Eager returns the current request result while refill continues in the
background. It is rejected in Lambda because Shimmy does not own a post-response
Runtime API loop that could preserve this lifecycle safely.

Sources:

- [policy mapping](../../internal/execution/dispatcher_qemu.go#L82-L110)
- [`WorkerSupervisor.Send`](../../internal/execution/supervisor/supervisor.go#L180-L227)
- [invocation release/wait](../../internal/execution/supervisor/supervisor.go#L310-L369)
- [eager refill](../../internal/execution/supervisor/supervisor.go#L372-L428)
- [eager shutdown](../../internal/execution/supervisor/supervisor.go#L430-L458)

## 12. Destroy/reset operations and operating-system effects

The guest-reset hot path is process destruction, not a page syscall in Shimmy.
`VM.Close`:

1. closes the host control connection;
2. if QEMU is still live, sends `SIGTERM` to the QEMU process group on Unix;
3. waits up to `FUNCTION_QEMU_SHUTDOWN_TIMEOUT`;
4. sends `SIGKILL` on timeout and waits again;
5. joins run/close errors;
6. removes the private VM work directory and control socket.

`ProcessWorker` uses `exec.CommandContext` and a separate Unix process group so
stop signals target the wrapper/child group where possible. QEMU exits; the
kernel reclaims guest RAM mappings and device state. The next VM reconstructs
RAM from kernel/initrd/read-only rootfs and starts with a new boot ID.

There is no host syscall that rewinds guest RAM in place. No `mmap(MAP_FIXED)`,
`madvise`, UFFD, KSM or QEMU monitor command participates in this implementation.

Sources:

- [`VM.Close`](../../internal/execution/qemurun/vm.go#L96-L146)
- [Unix process-group termination](../../internal/execution/qemurun/vm_unix.go#L10-L26)
- [outer process worker](../../internal/execution/worker/worker.go#L103-L461)

## 13. Failure semantics

### Startup

Startup fails closed for malformed booleans, QEMU/DBI conflict, unsupported
interfaces, missing command/artifacts, invalid manifest/digests, bad runtime
bounds, missing QEMU binary, boot timeout, protocol mismatch or invalid ready
frame.

### Active request

Context cancellation propagates through the runner. Protocol, frame, guest exit,
copy and endpoint errors are returned to the outer adapter. In transient/lazy
mode, release still owns old-process destruction. In eager mode, refill failure
is recorded for the next acquire.

### Shutdown

Shutdown does not infer process death from caller cancellation. Stop/wait uses
background context and a separate timeout path. This preserves the invariant
that a replacement cannot overlap a merely signalled old guest.

## 14. Security and state boundary

### Implemented

- whole-guest process/device lifetime;
- read-only rootfs;
- digest-checked kernel/initrd/rootfs;
- no default network;
- no QEMU monitor, display or serial console;
- private host control socket/work directory;
- bounded framed protocol;
- explicit readiness and boot identity;
- fresh boot for file/lazy/eager replacement lifecycles.

### Not implied

- `off` RPC is stateful;
- user networking is not egress policy;
- file/socket/remote side effects outside the destroyed guest are not rolled back;
- no KVM qualification is established by source structure;
- no Lambda eager lifecycle exists;
- no snapshot/loadvm implementation exists;
- guest image contents are assumed to include the command, cwd, interpreter,
  libraries and data; runtime does not preflight all dependencies;
- current protocol has boot identity but no explicit invocation identity;
- artifact path containment and source-lock comparison are incomplete;
- whole-guest execution is not itself a proof against every QEMU/device/kernel
  vulnerability.

Historical `docs/qemu-fallback-evidence.json` records TCG parity and lazy boot-ID
isolation at another implementation commit. It explicitly excludes KVM and
production Lambda qualification. Do not treat it as a current-head benchmark.

## 15. Verification map

### Source/package gates

```bash
go test -race ./internal/execution/qemurun ./internal/execution/qemuguest

go test ./cmd/shimmy-qemu-runner ./cmd/shimmy-qemu-guest \
  ./experiments/qemu-fallback/file-evaluator \
  ./experiments/qemu-fallback/rpc-evaluator

go vet ./internal/execution/qemurun ./internal/execution/qemuguest \
  ./cmd/shimmy-qemu-runner ./cmd/shimmy-qemu-guest

bash -n experiments/qemu-fallback/build-image.sh \
  experiments/qemu-fallback/smoke-file.sh \
  experiments/qemu-fallback/smoke-rpc.sh \
  experiments/qemu-fallback/run-doc-profile.sh
```

Unit/package coverage includes:

- manifest strictness and digest verification;
- QEMU argv and network profile;
- hello/ready version and boot ID;
- codec/frame/stream bounds;
- file execution and response copying;
- RPC stdio and network bridges;
- context cancellation and non-zero exit;
- outer host response symlink rejection test;
- off/lazy/eager policy mapping;
- eager refill and shutdown ordering.

### Full guest integration

The repository workflow additionally builds the pinned image, checks repeatable
artifact digests, runs native-vs-QEMU file parity, all five RPC transports, and
lazy boot-ID isolation:

- [`.github/workflows/qemu-fallback.yml`](../../.github/workflows/qemu-fallback.yml#L60-L145)

Local full integration requires Linux, QEMU and built guest artifacts. Package
unit tests do not replace that environment.

## 16. Source map

| Concern | Source |
|---|---|
| wrapper and reset policy | [`dispatcher_qemu.go`](../../internal/execution/dispatcher_qemu.go#L20-L130) |
| manifest loader | [`qemurun/manifest.go`](../../internal/execution/qemurun/manifest.go#L46-L194) |
| runtime config | [`qemurun/runtime.go`](../../internal/execution/qemurun/runtime.go#L24-L164) |
| invocation parsing | [`qemurun/invocation.go`](../../internal/execution/qemurun/invocation.go#L14-L80) |
| QEMU argv | [`qemurun/command.go`](../../internal/execution/qemurun/command.go#L46-L115) |
| VM ownership | [`qemurun/vm.go`](../../internal/execution/qemurun/vm.go#L42-L146) |
| host runner | [`cmd/shimmy-qemu-runner/main.go`](../../cmd/shimmy-qemu-runner/main.go#L44-L200) |
| guest executable | [`cmd/shimmy-qemu-guest/main.go`](../../cmd/shimmy-qemu-guest/main.go#L26-L45) |
| protocol | [`qemurun/protocol.go`](../../internal/execution/qemurun/protocol.go#L11-L279) |
| host client | [`qemurun/client.go`](../../internal/execution/qemurun/client.go#L23-L95) |
| guest server | [`qemuguest/server.go`](../../internal/execution/qemuguest/server.go#L21-L128) |
| guest file path | [`qemuguest/file.go`](../../internal/execution/qemuguest/file.go#L39-L105) |
| host RPC stdio | [`qemurun/rpc_stdio.go`](../../internal/execution/qemurun/rpc_stdio.go#L12-L140) |
| guest RPC stdio | [`qemuguest/rpc.go`](../../internal/execution/qemuguest/rpc.go#L19-L143) |
| network bridges | [`qemurun/rpc_network.go`](../../internal/execution/qemurun/rpc_network.go#L12-L228) and [`qemuguest/rpc_network.go`](../../internal/execution/qemuguest/rpc_network.go#L19-L235) |
| lifecycle supervisor | [`supervisor.go`](../../internal/execution/supervisor/supervisor.go#L130-L458) |
| wrapper tests | [`dispatcher_qemu_test.go`](../../internal/execution/dispatcher_qemu_test.go) |

## 17. Open design questions

1. Should the protocol carry a host invocation ID in addition to `BootID`?
2. Should runtime load and compare the source-lock artifact rather than only
   validate a digest string?
3. Should manifest artifacts be confined beneath a declared image root?
4. Should response-file opening use descriptor-based no-follow semantics?
5. Should guest file output be streamed/bounded before complete buffering?
6. What explicit egress policy is required before enabling user networking?
7. How should evaluator dependency presence be verified before readiness?
8. Should eager Lambda wait for a real post-response runtime-loop integration?
9. Which current-head TCG/KVM/Lambda gates are required before promotion?

## 18. Core invariants

1. QEMU is an explicit wrapper around `file`/`rpc`, never an automatic retry.
2. QEMU and DBI are mutually exclusive.
3. A QEMU process is not ready until versioned `hello`/`ready` completes.
4. File and lazy lifecycles destroy and reap the old guest before replacement.
5. Eager never publishes replacement before the old guest is stopped and waited.
6. Guest reset means fresh boot; no RAM snapshot or in-place rewind exists.
7. `off` RPC intentionally preserves guest state and must not be described as
   independent-request isolation.
8. Historical TCG evidence is version-bound; it is not a current KVM or production
   qualification claim.
