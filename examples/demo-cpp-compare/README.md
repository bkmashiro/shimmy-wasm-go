# C++ Compare Evaluator for Shimmy WASM

This example is a minimal, self-contained C++ evaluator that compiles to a
WebAssembly module and runs through Shimmy's opt-in WASM backend. It is not a
port of an existing Lambda Feedback repository; it is a small Go/C++-style
artifact example for validating the generic WASM execution path.

It intentionally mirrors the shape of a simple Lambda Feedback evaluator:

- input: `response`, `answer`, and optional feedback strings in `params`
- output: `{ "command": "eval", "result": { "is_correct", "feedback" } }`

The evaluator also reports `guest_invocation_count` and `snapshot_isolation_ok`
so the demo and integration test can prove that Shimmy reuses a warm WASM
instance while restoring guest memory after each request.

## Build

The reference environment for this example is Linux, or a Linux container, with
Zig installed. The same command also works on macOS when Zig is installed; the
point is to rely on an explicit WASM-capable toolchain rather than the host's
default C++ compiler.

```bash
zig c++ \
  -target wasm32-freestanding \
  -Oz \
  -nostdlib \
  -fno-exceptions \
  -fno-rtti \
  -Wl,--no-entry \
  -Wl,--export=alloc \
  -Wl,--export=evaluate \
  -Wl,--export-memory \
  -Wl,--initial-memory=2097152 \
  -o eval.wasm \
  evaluator.cpp
```

The source avoids libc/libc++ and implements only the small amount of JSON
handling needed for this evaluator, so it can be built as a small freestanding
WebAssembly module. Real C++ evaluators can use a richer build setup, but still
need to expose the same Shimmy WASM ABI:

```text
memory
alloc(size: i32) -> i32
evaluate(req_ptr: i32, req_len: i32) -> i32
```

`evaluate` returns a pointer to `[uint32 little-endian response_len][response JSON bytes]`.

## Run the end-to-end demo

From the repository root:

```bash
./scripts/demo-cpp-wasm.sh
```

The script builds Shimmy, compiles this evaluator to `eval.wasm`, starts Shimmy
with `FUNCTION_INTERFACE=wasm`, sends two HTTP requests, and asserts that both
requests see `guest_invocation_count == 1`.

The Go test suite also compiles this example when `zig` is available:

```bash
go test ./internal/execution/wasm -run TestCppCompareExample_CompilesAndRunsThroughDispatcher -v
```
