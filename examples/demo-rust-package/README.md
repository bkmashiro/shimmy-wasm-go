# Rust Package Evaluator for Shimmy WASM

This example is intentionally crate-shaped rather than a single source file:

```text
Cargo.toml
src/lib.rs
src/compare.rs
```

It demonstrates the intended boundary for real evaluators: the crate build
recipe emits a `.wasm` artifact, and Shimmy's WASM backend only loads that
pre-built module.

## Build

```bash
cd examples/demo-rust-package
rustup target add wasm32-unknown-unknown
cargo build --target wasm32-unknown-unknown --release
```

The output module is:

```text
target/wasm32-unknown-unknown/release/demo_rust_package.wasm
```

## Test

From the repository root:

```bash
go test ./internal/execution/wasm -run TestRustPackageExample_CompilesAndRunsThroughDispatcher -v
```
