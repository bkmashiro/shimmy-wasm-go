# Rust Compare Evaluator for Shimmy WASM

This example is a minimal, self-contained Rust evaluator that compiles to a
WebAssembly module and runs through Shimmy's opt-in WASM backend. It is not a
port of an existing Lambda Feedback repository; it is a small Rust artifact
example for validating the generic WASM execution path when a Rust WASM target
is installed.

It intentionally mirrors the shape of a simple Lambda Feedback evaluator:

- input: `response`, `answer`, and optional feedback strings in `params`
- output: `{ "command": "eval", "result": { "is_correct", "feedback" } }`

The evaluator also reports `guest_invocation_count` and `snapshot_isolation_ok`
so the integration test can prove that Shimmy reuses a warm WASM instance while
restoring guest memory after each request.

## Build

The reference environment is Linux, or a Linux container, with Rust installed and
the `wasm32-unknown-unknown` target available:

```bash
cd examples/demo-rust-compare
rustup target add wasm32-unknown-unknown
rustc \
  --target wasm32-unknown-unknown \
  --crate-type cdylib \
  -C panic=abort \
  -O \
  -o eval.wasm \
  evaluator.rs
```

The source is `#![no_std]` and implements only the small amount of JSON handling
needed for this evaluator. Real Rust evaluators can use richer build setup, but
still need to expose the same Shimmy WASM ABI:

```text
memory
alloc(size: i32) -> i32
evaluate(req_ptr: i32, req_len: i32) -> i32
```

`evaluate` returns a pointer to `[uint32 little-endian response_len][response JSON bytes]`.

## Test

From the repository root:

```bash
go test ./internal/execution/wasm -run TestRustCompareExample_CompilesAndRunsThroughDispatcher -v
```

The test skips when `rustc` or the `wasm32-unknown-unknown` target is unavailable.
