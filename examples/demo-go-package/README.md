# Go Package Evaluator for Shimmy WASM

This example is intentionally package-shaped rather than a single source file:

```text
go.mod
cmd/evaluator/main.go
internal/compare/compare.go
```

It demonstrates the intended boundary for real evaluators: the package build
recipe emits an `eval.wasm` artifact, and Shimmy's WASM backend only loads that
pre-built module.

## Build

```bash
GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o eval.wasm ./cmd/evaluator
```

## Test

From the repository root:

```bash
go test ./internal/execution/wasm -run TestGoPackageExample_CompilesAndRunsThroughDispatcher -v
```
