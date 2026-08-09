package wasm

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestAgentPythonObserverExactArtifactPhaseOrder(t *testing.T) {
	wasmPath := os.Getenv("AGENT_PYTHON_RUNTIME_WASM")
	manifestPath := os.Getenv("AGENT_PYTHON_RUNTIME_MANIFEST")
	if wasmPath == "" || manifestPath == "" {
		t.Skip("AGENT_PYTHON_RUNTIME_WASM and AGENT_PYTHON_RUNTIME_MANIFEST are required")
	}

	scriptPath := filepath.Join(t.TempDir(), "observer_eval.py")
	require.NoError(t, os.WriteFile(scriptPath, []byte(`_counter = 0
def dispatch(method, payload):
    if method != "eval":
        raise LookupError("unsupported method: " + method)
    global _counter
    _counter += 1
    return {"is_correct": payload.get("response") == payload.get("answer"), "guest_invocation_count": _counter}
`), 0o600))

	var mu sync.Mutex
	var events []AgentPythonPhaseEvent
	dispatcher := NewAgentPythonDispatcher(Config{
		ModulePath:              wasmPath,
		AgentPythonManifestPath: manifestPath,
		PythonScriptPath:        scriptPath,
		PythonLifecycle:         "snapshot",
		SnapshotMode:            "memcpy",
		MaxInstances:            1,
		MaxMemoryPages:          8192,
		Timeout:                 2 * time.Minute,
		AgentPythonObserver: func(event AgentPythonPhaseEvent) {
			mu.Lock()
			events = append(events, event)
			mu.Unlock()
		},
	}, zap.NewNop())
	require.NoError(t, dispatcher.Start(context.Background()))
	t.Cleanup(func() { require.NoError(t, dispatcher.Shutdown(context.Background())) })

	response, err := dispatcher.Send(context.Background(), "eval", map[string]any{
		"response": "42",
		"answer":   "42",
		"params":   map[string]any{},
	})
	require.NoError(t, err)
	assert.Equal(t, float64(1), response["result"].(map[string]any)["guest_invocation_count"])

	mu.Lock()
	got := append([]AgentPythonPhaseEvent(nil), events...)
	mu.Unlock()

	assertObservedPhaseSubsequence(t, got, AgentPythonPurposeStartup, []AgentPythonPhase{
		AgentPythonPhaseArtifactVerify,
		AgentPythonPhaseRuntimeCreate,
		AgentPythonPhaseWASIImports,
		AgentPythonPhaseHostImports,
		AgentPythonPhaseCompile,
		AgentPythonPhaseInstantiate,
		AgentPythonPhaseInitialize,
		AgentPythonPhaseRuntimeInit,
		AgentPythonPhaseRuntimePrepare,
		AgentPythonPhaseHeadroom,
		AgentPythonPhaseSnapshotTake,
	})
	assertObservedPhaseSubsequence(t, got, AgentPythonPurposeRequest, []AgentPythonPhase{
		AgentPythonPhaseCheckout,
		AgentPythonPhaseExecute,
		AgentPythonPhaseRestore,
		AgentPythonPhaseDecode,
	})
	for _, event := range got {
		assert.NotEqual(t, AgentPythonOutcomeError, event.Outcome, "%+v", event)
		assert.GreaterOrEqual(t, event.Duration, time.Duration(0))
	}
}

func assertObservedPhaseSubsequence(t *testing.T, events []AgentPythonPhaseEvent, purpose AgentPythonPurpose, want []AgentPythonPhase) {
	t.Helper()
	index := 0
	for _, event := range events {
		if event.Purpose != purpose || index == len(want) {
			continue
		}
		if event.Phase == want[index] {
			index++
		}
	}
	if index != len(want) {
		t.Fatalf("observed %d/%d phases for purpose %q; events=%+v", index, len(want), purpose, events)
	}
}
