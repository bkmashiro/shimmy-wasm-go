package wasm

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func repoRootFromTest(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	require.True(t, ok, "runtime.Caller failed")
	return filepath.Clean(filepath.Join(filepath.Dir(filename), "..", "..", ".."))
}

func buildCppCompareExample(t *testing.T) string {
	t.Helper()
	zig, err := exec.LookPath("zig")
	if err != nil {
		t.Skip("zig is required to compile the C++ WASM example")
	}

	root := repoRootFromTest(t)
	src := filepath.Join(root, "examples", "demo-cpp-compare", "evaluator.cpp")
	out := filepath.Join(t.TempDir(), "eval.wasm")

	cmd := exec.Command(zig, "c++",
		"-target", "wasm32-freestanding",
		"-Oz",
		"-nostdlib",
		"-fno-exceptions",
		"-fno-rtti",
		"-Wl,--no-entry",
		"-Wl,--export=alloc",
		"-Wl,--export=evaluate",
		"-Wl,--export-memory",
		"-Wl,--initial-memory=2097152",
		"-o", out,
		src,
	)
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "compile C++ WASM example:\n%s", string(output))
	return out
}

func buildGoStatefulExample(t *testing.T) string {
	t.Helper()
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go is required to compile the Go WASM example")
	}

	root := repoRootFromTest(t)
	out := filepath.Join(t.TempDir(), "eval.wasm")
	cmd := exec.Command(goBin, "build", "-buildmode=c-shared", "-o", out, "./examples/demo-stateful")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm")
	output, err := cmd.CombinedOutput()
	if err != nil && strings.Contains(string(output), "requires go1.24 or later") {
		t.Skipf("go toolchain does not support //go:wasmexport:\n%s", string(output))
	}
	require.NoError(t, err, "compile Go WASM example:\n%s", string(output))
	return out
}

func buildRustCompareExample(t *testing.T) string {
	t.Helper()
	rustc, err := exec.LookPath("rustc")
	if err != nil {
		t.Skip("rustc is required to compile the Rust WASM example")
	}

	root := repoRootFromTest(t)
	src := filepath.Join(root, "examples", "demo-rust-compare", "evaluator.rs")
	require.FileExists(t, src, "Rust WASM example source must exist")
	out := filepath.Join(t.TempDir(), "eval.wasm")
	cmd := exec.Command(rustc,
		"--target", "wasm32-unknown-unknown",
		"--crate-type", "cdylib",
		"-C", "panic=abort",
		"-O",
		"-o", out,
		src,
	)
	output, err := cmd.CombinedOutput()
	if err != nil && strings.Contains(string(output), "target may not be installed") {
		t.Skipf("rust target wasm32-unknown-unknown is not installed:\n%s", string(output))
	}
	require.NoError(t, err, "compile Rust WASM example:\n%s", string(output))
	return out
}

func buildGoPackageExample(t *testing.T) string {
	t.Helper()
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go is required to compile the Go package WASM example")
	}

	root := repoRootFromTest(t)
	packageDir := filepath.Join(root, "examples", "demo-go-package")
	require.DirExists(t, packageDir, "Go package WASM example must exist")
	out := filepath.Join(t.TempDir(), "eval.wasm")
	cmd := exec.Command(goBin, "build", "-buildmode=c-shared", "-o", out, "./cmd/evaluator")
	cmd.Dir = packageDir
	cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm")
	output, err := cmd.CombinedOutput()
	if err != nil && strings.Contains(string(output), "requires go1.24 or later") {
		t.Skipf("go toolchain does not support //go:wasmexport:\n%s", string(output))
	}
	require.NoError(t, err, "compile Go package WASM example:\n%s", string(output))
	return out
}

func buildRustPackageExample(t *testing.T) string {
	t.Helper()
	cargo, err := exec.LookPath("cargo")
	if err != nil {
		t.Skip("cargo is required to compile the Rust package WASM example")
	}

	root := repoRootFromTest(t)
	packageDir := filepath.Join(root, "examples", "demo-rust-package")
	require.DirExists(t, packageDir, "Rust package WASM example must exist")
	cmd := exec.Command(cargo, "build", "--target", "wasm32-unknown-unknown", "--release")
	cmd.Dir = packageDir
	output, err := cmd.CombinedOutput()
	if err != nil && strings.Contains(string(output), "target may not be installed") {
		t.Skipf("rust target wasm32-unknown-unknown is not installed:\n%s", string(output))
	}
	require.NoError(t, err, "compile Rust package WASM example:\n%s", string(output))
	return filepath.Join(packageDir, "target", "wasm32-unknown-unknown", "release", "demo_rust_package.wasm")
}

func buildCppPackageExample(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("zig"); err != nil {
		t.Skip("zig is required to compile the C++ package WASM example")
	}
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make is required to compile the C++ package WASM example")
	}

	root := repoRootFromTest(t)
	packageDir := filepath.Join(root, "examples", "demo-cpp-package")
	require.DirExists(t, packageDir, "C++ package WASM example must exist")
	out := filepath.Join(t.TempDir(), "eval.wasm")
	cmd := exec.Command("make", "wasm", "OUT="+out)
	cmd.Dir = packageDir
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "compile C++ package WASM example:\n%s", string(output))
	return out
}

func assertCompareEvaluatorRunsThroughDispatcher(t *testing.T, modulePath string) {
	t.Helper()

	d := NewDispatcher(Config{
		ModulePath:   modulePath,
		MaxInstances: 1,
		Timeout:      5 * time.Second,
	}, newTestLogger(t))
	require.NoError(t, d.Start(context.Background()))
	t.Cleanup(func() { _ = d.Shutdown(context.Background()) })

	correct, err := d.Send(context.Background(), "eval", map[string]any{
		"response": "42",
		"answer":   "42",
		"params": map[string]any{
			"correct_response_feedback":   "Correct!",
			"incorrect_response_feedback": "Try again.",
		},
	})
	require.NoError(t, err)

	wrong, err := d.Send(context.Background(), "eval", map[string]any{
		"response": "41",
		"answer":   "42",
		"params": map[string]any{
			"correct_response_feedback":   "Correct!",
			"incorrect_response_feedback": "Try again.",
		},
	})
	require.NoError(t, err)

	correctResult, ok := correct["result"].(map[string]any)
	require.True(t, ok, "correct response result must be an object: %#v", correct)
	wrongResult, ok := wrong["result"].(map[string]any)
	require.True(t, ok, "wrong response result must be an object: %#v", wrong)

	assert.Equal(t, "eval", correct["command"])
	assert.Equal(t, true, correctResult["is_correct"])
	assert.NotEmpty(t, correctResult["feedback"])
	assert.EqualValues(t, 1, correctResult["guest_invocation_count"])
	assert.Equal(t, true, correctResult["snapshot_isolation_ok"])

	assert.Equal(t, "eval", wrong["command"])
	assert.Equal(t, false, wrongResult["is_correct"])
	assert.NotEmpty(t, wrongResult["feedback"])
	assert.EqualValues(t, 1, wrongResult["guest_invocation_count"])
	assert.Equal(t, true, wrongResult["snapshot_isolation_ok"])
}

func TestCppCompareExample_CompilesAndRunsThroughDispatcher(t *testing.T) {
	assertCompareEvaluatorRunsThroughDispatcher(t, buildCppCompareExample(t))
}

func TestGoStatefulExample_CompilesAndRunsThroughDispatcher(t *testing.T) {
	assertCompareEvaluatorRunsThroughDispatcher(t, buildGoStatefulExample(t))
}

func TestRustCompareExample_CompilesAndRunsThroughDispatcher(t *testing.T) {
	assertCompareEvaluatorRunsThroughDispatcher(t, buildRustCompareExample(t))
}

func TestGoPackageExample_CompilesAndRunsThroughDispatcher(t *testing.T) {
	assertCompareEvaluatorRunsThroughDispatcher(t, buildGoPackageExample(t))
}

func TestRustPackageExample_CompilesAndRunsThroughDispatcher(t *testing.T) {
	assertCompareEvaluatorRunsThroughDispatcher(t, buildRustPackageExample(t))
}

func TestCppPackageExample_CompilesAndRunsThroughDispatcher(t *testing.T) {
	assertCompareEvaluatorRunsThroughDispatcher(t, buildCppPackageExample(t))
}
