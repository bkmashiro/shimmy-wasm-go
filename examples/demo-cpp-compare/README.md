# C++ Compare Evaluator for Shimmy WASM

This example is a minimal C++ evaluator that compiles to a WebAssembly module and runs through Shimmy's opt-in WASM backend.

It intentionally mirrors the shape of a simple Lambda Feedback evaluator:

- input: `response`, `answer`, and optional feedback strings in `params`
- output: `{ "command": "eval", "result": { "is_correct", "feedback" } }`

The evaluator also reports `guest_invocation_count` and `snapshot_isolation_ok` so the demo can prove that Shimmy reuses a warm WASM instance while restoring guest memory after each request.

## Build

The demo uses Zig's clang-compatible C++ driver because the default macOS Apple clang does not ship a WebAssembly target:

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

The source avoids libc/libc++ and implements only the small amount of JSON handling needed for this evaluator, so it can be built as a small freestanding WebAssembly module. Real C++ evaluators can use a richer build setup, but still need to expose the same Shimmy WASM ABI:

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

The script builds Shimmy, compiles this evaluator to `eval.wasm`, starts Shimmy with `FUNCTION_INTERFACE=wasm`, sends two HTTP requests, and asserts that both requests see `guest_invocation_count == 1`.
