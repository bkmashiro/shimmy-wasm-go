package wasm

// TestDispatcher_Send_JS exercises the JS evaluation path end-to-end.
//
// The JS eval function is compiled to a WASM module by javy (QuickJS compiled
// to wasm32-wasi).  The module speaks the LSP-framed JSON-RPC 2.0 protocol
// over stdin/stdout — exactly the same protocol shimmy's rpc dispatcher uses
// for subprocess workers.
//
// This test spawns wazero as a subprocess (the same way shimmy would in
// production with FUNCTION_INTERFACE=rpc FUNCTION_COMMAND="wazero run ..."),
// sends JSON-RPC requests through its stdin, and reads the framed responses
// from its stdout.
//
// We use the wazero CLI rather than the Go API here because javy-produced WASM
// modules are WASI command modules (not reactors): they call proc_exit after
// main() returns.  Running them as an in-process wazero.Module would close the
// module after the first request.  The subprocess approach mirrors what shimmy
// actually does: spin up one process per pool slot, keep it alive for the
// lifetime of the slot, and multiplex requests through its stdin/stdout.
//
// Architecture note
// -----------------
// This is *not* the WASM dispatcher ABI (alloc/dispatch).  The JS runner uses
// the RPC dispatcher ABI (JSON-RPC 2.0 framed with Content-Length headers).
// The two paths are independent; this test verifies the JS-specific path works
// correctly from an external-protocol perspective.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// evalJsRunnerPath returns the absolute path to the pre-built javy eval-js
// runner fixture in testdata/.
func evalJsRunnerPath(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	require.True(t, ok, "runtime.Caller failed")
	return filepath.Join(filepath.Dir(filename), "testdata", "eval-js-runner.wasm")
}

// wazeroPath returns the path to the wazero CLI, either from the environment
// variable WAZERO_PATH or by searching PATH.
func wazeroPath(t *testing.T) (string, bool) {
	t.Helper()
	if p := os.Getenv("WAZERO_PATH"); p != "" {
		return p, true
	}
	p, err := exec.LookPath("wazero")
	if err != nil {
		return "", false
	}
	return p, true
}

// jsRPCProcess wraps a wazero subprocess running the eval-js runner and
// provides helpers for sending/receiving LSP-framed JSON-RPC messages.
type jsRPCProcess struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
}

func startJsRunner(t *testing.T, wazeroBin, wasmPath string) *jsRPCProcess {
	t.Helper()
	cmd := exec.Command(wazeroBin, "run", wasmPath)
	cmd.Stderr = os.Stderr // surface QuickJS error output in test log

	stdinPipe, err := cmd.StdinPipe()
	require.NoError(t, err, "StdinPipe")

	stdoutPipe, err := cmd.StdoutPipe()
	require.NoError(t, err, "StdoutPipe")

	require.NoError(t, cmd.Start(), "start wazero runner")

	p := &jsRPCProcess{
		cmd:    cmd,
		stdin:  stdinPipe,
		stdout: bufio.NewReader(stdoutPipe),
	}

	t.Cleanup(func() {
		_ = stdinPipe.Close()
		// Give the process a moment to flush and exit cleanly.
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			_ = cmd.Process.Kill()
		}
	})

	return p
}

// send writes a single LSP-framed JSON-RPC request to the process stdin.
func (p *jsRPCProcess) send(t *testing.T, id int, method string, params any) {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  []any{params},
	})
	require.NoError(t, err, "marshal request")

	header := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(body))
	_, err = fmt.Fprint(p.stdin, header)
	require.NoError(t, err, "write header")

	_, err = p.stdin.Write(body)
	require.NoError(t, err, "write body")
}

// recv reads one LSP-framed JSON-RPC response and returns the parsed result.
func (p *jsRPCProcess) recv(t *testing.T) map[string]any {
	t.Helper()

	// Scan lines until we find a Content-Length header, skipping any stray output.
	var contentLength int
	for {
		line, err := p.stdout.ReadString('\n')
		require.NoError(t, err, "read header line")
		line = strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(line, "Content-Length:") {
			parts := strings.SplitN(line, ":", 2)
			n, err := strconv.Atoi(strings.TrimSpace(parts[1]))
			require.NoError(t, err, "parse Content-Length")
			contentLength = n
			break
		}
	}

	// Drain remaining header lines until blank separator.
	for {
		line, err := p.stdout.ReadString('\n')
		require.NoError(t, err, "read separator")
		if strings.TrimRight(line, "\r\n") == "" {
			break
		}
	}

	// Read the body.
	body := make([]byte, contentLength)
	_, err := io.ReadFull(p.stdout, body)
	require.NoError(t, err, "read body")

	var rpc struct {
		Result map[string]any `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(body, &rpc), "unmarshal response")
	require.Nil(t, rpc.Error, "unexpected JSON-RPC error: %+v", rpc.Error)
	return rpc.Result
}

// TestDispatcher_Send_JS sends several requests through the javy-compiled
// eval-js runner and verifies the evaluation results.
func TestDispatcher_Send_JS(t *testing.T) {
	wazeroBin, ok := wazeroPath(t)
	if !ok {
		t.Skip("wazero not found on PATH (set WAZERO_PATH or install wazero)")
	}

	wasmPath := evalJsRunnerPath(t)
	if _, err := os.Stat(wasmPath); err != nil {
		t.Skipf("eval-js-runner.wasm not found at %s (run examples/eval-js/build-runner.sh first)", wasmPath)
	}

	_, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	proc := startJsRunner(t, wazeroBin, wasmPath)

	tests := []struct {
		name      string
		response  string
		answer    string
		wantRight bool
	}{
		{"exact match", "42", "42", true},
		{"float match", "3.14159", "3.14159", true},
		{"wrong answer", "2.5", "3.0", false},
		{"non-numeric", "abc", "42", false},
	}

	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			proc.send(t, i+1, "eval", map[string]any{
				"response": tc.response,
				"answer":   tc.answer,
			})

			result := proc.recv(t)
			require.NotNil(t, result, "result must not be nil")

			isCorrect, ok := result["is_correct"].(bool)
			require.True(t, ok, "is_correct must be a bool, got %T", result["is_correct"])
			assert.Equal(t, tc.wantRight, isCorrect,
				"is_correct mismatch for response=%q answer=%q", tc.response, tc.answer)

			feedback, ok := result["feedback"].(string)
			assert.True(t, ok, "feedback must be a string")
			assert.NotEmpty(t, feedback, "feedback must not be empty")
		})
	}

	// Healthcheck
	t.Run("healthcheck", func(t *testing.T) {
		proc.send(t, 99, "healthcheck", map[string]any{})
		result := proc.recv(t)
		assert.Equal(t, "ok", result["status"])
	})
}
