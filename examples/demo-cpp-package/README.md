# C++ Package Evaluator for Shimmy WASM

This example is intentionally package-shaped rather than a single source file:

```text
Makefile
include/compare.hpp
src/evaluator.cpp
src/compare.cpp
```

It demonstrates the intended boundary for real evaluators: the package build
recipe emits an `eval.wasm` artifact, and Shimmy's WASM backend only loads that
pre-built module.

## Build

```bash
make wasm
```

The Makefile uses Zig's clang-compatible C++ driver to produce a freestanding
WebAssembly module. Override the output path with:

```bash
make wasm OUT=/tmp/eval.wasm
```

## Test

From the repository root:

```bash
go test ./internal/execution/wasm -run TestCppPackageExample_CompilesAndRunsThroughDispatcher -v
```
