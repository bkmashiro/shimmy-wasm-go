package wasm

import (
	"context"
	"os/exec"
	"path/filepath"
	"runtime"
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

func TestCppCompareExample_CompilesAndRunsThroughDispatcher(t *testing.T) {
	modulePath := buildCppCompareExample(t)

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
	assert.Equal(t, "Correct!", correctResult["feedback"])
	assert.EqualValues(t, 1, correctResult["guest_invocation_count"])
	assert.Equal(t, true, correctResult["snapshot_isolation_ok"])

	assert.Equal(t, "eval", wrong["command"])
	assert.Equal(t, false, wrongResult["is_correct"])
	assert.Equal(t, "Try again.", wrongResult["feedback"])
	assert.EqualValues(t, 1, wrongResult["guest_invocation_count"])
	assert.Equal(t, true, wrongResult["snapshot_isolation_ok"])
}
