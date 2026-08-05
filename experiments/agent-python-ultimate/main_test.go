package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	wasmexec "github.com/lambda-feedback/shimmy/internal/execution/wasm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func validOKWorkerResult(row PlanRow) WorkerResult {
	calls := row.Repeat
	if row.Concurrency != nil && *row.Concurrency > calls {
		calls = *row.Concurrency
	}
	requests := make([]RequestSample, calls)
	for i := range requests {
		requests[i] = RequestSample{Index: i, StartedUTC: "2026-08-05T00:00:01Z", Duration: time.Millisecond, Outcome: "ok"}
	}
	return WorkerResult{
		Schema: workerResultSchema, Row: row, Status: "ok",
		StartedUTC: "2026-08-05T00:00:00Z", FinishedUTC: "2026-08-05T00:00:02Z",
		StartupDuration: time.Millisecond, ShutdownDuration: time.Millisecond,
		Requests: requests,
		Phases: []wasmexec.AgentPythonPhaseEvent{{
			Phase: wasmexec.AgentPythonPhaseExecute, Duration: time.Millisecond, Outcome: wasmexec.AgentPythonOutcomeOK,
		}},
	}
}

func TestValidWorkerResultRequiresResumableStatusAndExactRow(t *testing.T) {
	row := PlanRow{ID: "row-1", Campaign: "capability", Lifecycle: LifecycleFresh, Pool: 1, PreparedCapacity: 1, Repeat: 1, Surface: "direct"}
	path := filepath.Join(t.TempDir(), "row.json")

	result := WorkerResult{Schema: workerResultSchema, Row: row, Status: "failed"}
	require.NoError(t, writeJSON(path, result))
	assert.False(t, validWorkerResult(path, row, "ok", "unavailable", "unsupported"))

	result.Status = "ok"
	require.NoError(t, writeJSON(path, result))
	assert.False(t, validWorkerResult(path, row, "ok", "unavailable", "unsupported"), "evidence-free ok result must not be resumable")

	result = validOKWorkerResult(row)
	require.NoError(t, writeJSON(path, result))
	assert.True(t, validWorkerResult(path, row, "ok", "unavailable", "unsupported"))

	changed := row
	changed.Surface = "http"
	assert.False(t, validWorkerResult(path, changed, "ok", "unavailable", "unsupported"))
}

func TestValidateTerminalWorkerResultAcceptsOutOfOrderConcurrentSamples(t *testing.T) {
	concurrency := 2
	row := PlanRow{ID: "row-concurrent", Campaign: "throughput", Lifecycle: LifecycleFresh, Pool: 1, PreparedCapacity: 1, Repeat: 1, Concurrency: &concurrency, Surface: "direct"}
	result := validOKWorkerResult(row)
	result.Requests[0], result.Requests[1] = result.Requests[1], result.Requests[0]
	require.NoError(t, validateTerminalWorkerResult(result, row, "ok"))
}

func TestRebuildReportRejectsSameIDWithDifferentPlanRow(t *testing.T) {
	dir := t.TempDir()
	rowsDir := filepath.Join(dir, "rows")
	require.NoError(t, os.MkdirAll(rowsDir, 0o700))
	planned := PlanRow{ID: "row-1", Campaign: "capability", Lifecycle: LifecycleFresh, Pool: 1, PreparedCapacity: 1, Repeat: 1, Surface: "direct"}
	require.NoError(t, writeJSON(filepath.Join(dir, "plan.json"), Plan{Schema: PlanSchemaVersion, Seed: 1, Rows: []PlanRow{planned}}))
	require.NoError(t, writeJSON(filepath.Join(dir, "metadata.json"), RunMetadata{}))
	tampered := planned
	tampered.Campaign = "fault-recovery"
	tampered.Lifecycle = LifecycleSnapshotCow
	tampered.SnapshotSelected = "cow"
	require.NoError(t, writeJSON(filepath.Join(rowsDir, "row-1.json"), validOKWorkerResult(tampered)))

	_, err := rebuildReport(dir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exact plan row")
}

func TestRebuildReportRejectsMissingPlanRowResult(t *testing.T) {
	dir := t.TempDir()
	planned := PlanRow{ID: "row-1", Campaign: "capability", Lifecycle: LifecycleFresh, Pool: 1, PreparedCapacity: 1, Repeat: 1, Surface: "direct"}
	require.NoError(t, writeJSON(filepath.Join(dir, "plan.json"), Plan{Schema: PlanSchemaVersion, Seed: 1, Rows: []PlanRow{planned}}))
	require.NoError(t, writeJSON(filepath.Join(dir, "metadata.json"), RunMetadata{}))

	_, err := rebuildReport(dir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "row-1.json")
}

func TestValidateRunProvenanceBindsCompleteMetadataAndPreview(t *testing.T) {
	dir := t.TempDir()
	provenanceDir := filepath.Join(dir, "provenance")
	require.NoError(t, os.MkdirAll(provenanceDir, 0o700))
	row := PlanRow{ID: "row-1", Campaign: "capability", Lifecycle: LifecycleFresh, Pool: 1, PreparedCapacity: 1, Repeat: 1, Surface: "direct"}
	plan := Plan{Schema: PlanSchemaVersion, Seed: 7, Rows: []PlanRow{row}}
	planRaw, err := json.Marshal(plan)
	require.NoError(t, err)
	hashA := strings.Repeat("a", 64)
	hashB := strings.Repeat("b", 64)
	hashC := strings.Repeat("c", 64)
	hashD := strings.Repeat("d", 64)
	metadata := RunMetadata{
		StartedUTC: "2026-08-05T00:00:00Z", FinishedUTC: "2026-08-05T00:00:01Z",
		ExecutableSHA: hashA, ArtifactSHA: hashB, ManifestSHA: hashC, ConfigSHA: hashD,
		PlanSHA: bytesSHA256(planRaw), SourceCommit: strings.Repeat("e", 40), SlurmJobID: "123",
		RowsPlanned: 1, RowsCompleted: 1, Complete: true,
	}
	manifest := inputManifest{
		Schema: "shimmy-agent-python-doc-input/v1", SourceCommit: metadata.SourceCommit,
		ConfigPath: "experiments/agent-python-ultimate/configs/focused-dirty.json", PlanSeed: plan.Seed,
		Files: map[string]inputFileIdentity{
			"agent-python-ultimate":                {Bytes: 1, SHA256: hashA},
			"agent-python-runtime-numpy-core.wasm": {Bytes: 1, SHA256: hashB},
			"manifest.json":                        {Bytes: 1, SHA256: hashC},
			"ultimate.json":                        {Bytes: 1, SHA256: hashD},
		},
	}
	require.NoError(t, writeJSON(filepath.Join(provenanceDir, "input-manifest.json"), manifest))
	require.NoError(t, writeJSON(filepath.Join(provenanceDir, "plan.preview.json"), plan))
	require.NoError(t, validateRunProvenance(dir, metadata, &plan))

	metadata.Complete = false
	err = validateRunProvenance(dir, metadata, &plan)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "complete exact plan row set")
}

func TestValidateResumeIdentityRejectsChangedPlan(t *testing.T) {
	dir := t.TempDir()
	existing := RunMetadata{
		ExecutableSHA: "exe", ArtifactSHA: "artifact", ManifestSHA: "manifest",
		ConfigSHA: "config", PlanSHA: "old-plan", SourceCommit: strings.Repeat("a", 40),
	}
	require.NoError(t, writeJSON(filepath.Join(dir, "metadata.json"), existing))

	expected := existing
	expected.PlanSHA = "new-plan"
	err := validateResumeIdentity(dir, expected)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "plan_sha256")
}

func TestValidateResumeIdentityAcceptsExactImmutableInputs(t *testing.T) {
	dir := t.TempDir()
	existing := RunMetadata{
		ExecutableSHA: "exe", ArtifactSHA: "artifact", ManifestSHA: "manifest",
		ConfigSHA: "config", PlanSHA: "plan", SourceCommit: strings.Repeat("b", 40),
	}
	require.NoError(t, writeJSON(filepath.Join(dir, "metadata.json"), existing))
	require.NoError(t, validateResumeIdentity(dir, existing))
}

func TestValidateResumeIdentityRejectsRowsWithoutMetadata(t *testing.T) {
	dir := t.TempDir()
	rowsDir := filepath.Join(dir, "rows")
	require.NoError(t, os.MkdirAll(rowsDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(rowsDir, "stale.json"), []byte("{}\n"), 0o600))

	err := validateResumeIdentity(dir, RunMetadata{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "without metadata.json")
}

func TestValidateResumeIdentityRejectsStalePrewarmMarkerWithoutMetadata(t *testing.T) {
	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "compile-cache")
	require.NoError(t, os.MkdirAll(cacheDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(cacheDir, ".prewarmed"), []byte("stale\n"), 0o600))

	err := validateResumeIdentity(dir, RunMetadata{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "without metadata.json")
}

func TestFileSHA256ReturnsReadErrors(t *testing.T) {
	_, err := fileSHA256(filepath.Join(t.TempDir(), "missing"))
	require.Error(t, err)
}

func TestValidateExplicitSeedRejectsExplicitZero(t *testing.T) {
	flags := flag.NewFlagSet("seed-test", flag.ContinueOnError)
	seed := flags.Int64("seed", 0, "")
	require.NoError(t, flags.Parse([]string{"--seed", "0"}))
	err := validateExplicitSeed(flags, *seed)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "positive integer")
}

func TestValidateExplicitSeedAllowsOmittedOverride(t *testing.T) {
	flags := flag.NewFlagSet("seed-test", flag.ContinueOnError)
	seed := flags.Int64("seed", 0, "")
	require.NoError(t, flags.Parse(nil))
	require.NoError(t, validateExplicitSeed(flags, *seed))
}

func TestRunPlanWorkerProcessRecordsSignaledWorkerCrashAsFailedRow(t *testing.T) {
	dir := t.TempDir()
	executable := filepath.Join(dir, "crash-worker.sh")
	require.NoError(t, os.WriteFile(executable, []byte("#!/bin/sh\nkill -SEGV $$\n"), 0o700))
	row := PlanRow{ID: "fault-row", Campaign: "fault-recovery", Lifecycle: LifecycleSnapshotCow, Pool: 1, PreparedCapacity: 1, Repeat: 1, Surface: "direct", Fault: "timeout", SnapshotSelected: "cow"}
	resultPath := filepath.Join(dir, "result.json")

	require.NoError(t, runPlanWorkerProcess(
		executable,
		filepath.Join(dir, "input.json"),
		resultPath,
		filepath.Join(dir, "stdout"),
		filepath.Join(dir, "stderr"),
		row,
	))

	raw, err := os.ReadFile(resultPath)
	require.NoError(t, err)
	var result WorkerResult
	require.NoError(t, decodeStrictJSON(raw, &result))
	assert.Equal(t, workerResultSchema, result.Schema)
	assert.Equal(t, row, result.Row)
	assert.Equal(t, "failed", result.Status)
	assert.Contains(t, result.Error, "signal:")
	assert.NotEmpty(t, result.StartedUTC)
	assert.NotEmpty(t, result.FinishedUTC)
}

func TestRunPlanWorkerProcessRecordsNonProtocolExitAsFailedRow(t *testing.T) {
	dir := t.TempDir()
	executable := filepath.Join(dir, "exit-worker.sh")
	require.NoError(t, os.WriteFile(executable, []byte("#!/bin/sh\nexit 4\n"), 0o700))
	resultPath := filepath.Join(dir, "result.json")
	row := PlanRow{ID: "row"}

	require.NoError(t, runPlanWorkerProcess(
		executable,
		filepath.Join(dir, "input.json"),
		resultPath,
		filepath.Join(dir, "stdout"),
		filepath.Join(dir, "stderr"),
		row,
	))

	raw, err := os.ReadFile(resultPath)
	require.NoError(t, err)
	var result WorkerResult
	require.NoError(t, decodeStrictJSON(raw, &result))
	assert.Equal(t, row, result.Row)
	assert.Equal(t, "failed", result.Status)
	assert.Contains(t, result.Error, "exit status 4")
}

func TestRunPlanWorkerProcessRecordsObservedSplitStackFatalAsFailedRow(t *testing.T) {
	dir := t.TempDir()
	executable := filepath.Join(dir, "go-fatal-worker.sh")
	require.NoError(t, os.WriteFile(executable, []byte("#!/bin/sh\nprintf 'fatal error: runtime: split stack overflow\\nruntime.sigpanic()\\npanic during panic\\n' >&2\nexit 2\n"), 0o700))
	resultPath := filepath.Join(dir, "result.json")
	row := PlanRow{ID: "fault-row"}

	require.NoError(t, runPlanWorkerProcess(
		executable,
		filepath.Join(dir, "input.json"),
		resultPath,
		filepath.Join(dir, "stdout"),
		filepath.Join(dir, "stderr"),
		row,
	))

	raw, err := os.ReadFile(resultPath)
	require.NoError(t, err)
	var result WorkerResult
	require.NoError(t, decodeStrictJSON(raw, &result))
	assert.Equal(t, row, result.Row)
	assert.Equal(t, "failed", result.Status)
	assert.Contains(t, result.Error, "exit status 2")
}

func TestRunPlanWorkerProcessPropagatesReservedWorkerProtocolExit(t *testing.T) {
	dir := t.TempDir()
	executable := filepath.Join(dir, "protocol-error-worker.sh")
	require.NoError(t, os.WriteFile(executable, []byte("#!/bin/sh\nexit 90\n"), 0o700))
	resultPath := filepath.Join(dir, "result.json")

	err := runPlanWorkerProcess(
		executable,
		filepath.Join(dir, "input.json"),
		resultPath,
		filepath.Join(dir, "stdout"),
		filepath.Join(dir, "stderr"),
		PlanRow{ID: "row"},
	)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "exit status 90")
	_, statErr := os.Stat(resultPath)
	assert.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestRunMainUsesReservedExitForWorkerProtocolErrors(t *testing.T) {
	var stderr bytes.Buffer

	code := runMain([]string{"agent-python-ultimate", "worker"}, &stderr)

	assert.Equal(t, 90, code)
	assert.Contains(t, stderr.String(), "worker requires --input and --output")
}

func TestRunMainUsesOrdinaryExitForParentCommandErrors(t *testing.T) {
	var stderr bytes.Buffer

	code := runMain([]string{"agent-python-ultimate", "unknown"}, &stderr)

	assert.Equal(t, 1, code)
	assert.Contains(t, stderr.String(), "unknown command")
}
