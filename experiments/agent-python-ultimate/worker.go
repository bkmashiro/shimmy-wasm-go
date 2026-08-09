package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	shimmyconfig "github.com/lambda-feedback/shimmy/config"
	shimmyhandler "github.com/lambda-feedback/shimmy/handler"
	wasmexec "github.com/lambda-feedback/shimmy/internal/execution/wasm"
	shimmyruntime "github.com/lambda-feedback/shimmy/runtime"
	"go.uber.org/zap"
)

const workerResultSchema = "agent-python-ultimate-worker-result/v1"

type WorkerInput struct {
	Schema          string  `json:"schema"`
	Row             PlanRow `json:"row"`
	ArtifactPath    string  `json:"artifact_path"`
	ManifestPath    string  `json:"manifest_path"`
	CompileCacheDir string  `json:"compile_cache_dir,omitempty"`
	Seed            int64   `json:"seed"`
}

type RequestSample struct {
	Index      int           `json:"index"`
	StartedUTC string        `json:"started_utc"`
	Duration   time.Duration `json:"duration_ns"`
	Outcome    string        `json:"outcome"`
	Error      string        `json:"error,omitempty"`
}

type WorkerResult struct {
	Schema            string                           `json:"schema"`
	Row               PlanRow                          `json:"row"`
	Status            string                           `json:"status"`
	StartedUTC        string                           `json:"started_utc"`
	FinishedUTC       string                           `json:"finished_utc"`
	StartupDuration   time.Duration                    `json:"startup_duration_ns"`
	HTTPReadyDuration time.Duration                    `json:"http_ready_duration_ns,omitempty"`
	ShutdownDuration  time.Duration                    `json:"shutdown_duration_ns"`
	Requests          []RequestSample                  `json:"requests"`
	Phases            []wasmexec.AgentPythonPhaseEvent `json:"phases"`
	SnapshotRequested string                           `json:"snapshot_requested,omitempty"`
	SnapshotSelected  string                           `json:"snapshot_selected,omitempty"`
	Error             string                           `json:"error,omitempty"`
	Before            ProcessMetrics                   `json:"process_before"`
	BeforeShutdown    ProcessMetrics                   `json:"process_before_shutdown"`
	After             ProcessMetrics                   `json:"process_after"`
	PreparedHits      uint64                           `json:"prepared_hits,omitempty"`
	PreparedMisses    uint64                           `json:"prepared_misses,omitempty"`
	PreparedRefills   uint64                           `json:"prepared_refills,omitempty"`
}

type dispatcherRuntime struct {
	dispatcher *wasmexec.AgentPythonDispatcher
}

func (runtime dispatcherRuntime) Handle(ctx context.Context, request shimmyruntime.EvaluationRequest) (shimmyruntime.EvaluationResponse, error) {
	response, err := runtime.dispatcher.Send(ctx, string(request.Command), request.Data)
	return shimmyruntime.EvaluationResponse(response), err
}

func (dispatcherRuntime) Start(context.Context) error    { return nil }
func (dispatcherRuntime) Shutdown(context.Context) error { return nil }

type phaseCollector struct {
	mu     sync.Mutex
	events []wasmexec.AgentPythonPhaseEvent
}

func (c *phaseCollector) observe(event wasmexec.AgentPythonPhaseEvent) {
	c.mu.Lock()
	c.events = append(c.events, event)
	c.mu.Unlock()
}

func (c *phaseCollector) snapshot() []wasmexec.AgentPythonPhaseEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]wasmexec.AgentPythonPhaseEvent(nil), c.events...)
}

func ParseWorkerInput(raw []byte) (*WorkerInput, error) {
	var input WorkerInput
	if err := decodeStrictJSON(raw, &input); err != nil {
		return nil, err
	}
	if input.Schema != "agent-python-ultimate-worker-input/v1" {
		return nil, fmt.Errorf("unsupported worker input schema %q", input.Schema)
	}
	plan := &Plan{Schema: PlanSchemaVersion, Seed: 1, Rows: []PlanRow{input.Row}}
	if err := ValidatePlan(plan); err != nil {
		return nil, err
	}
	if input.ArtifactPath == "" || input.ManifestPath == "" {
		return nil, errors.New("artifact_path and manifest_path are required")
	}
	return &input, nil
}

func RunWorker(ctx context.Context, input *WorkerInput) (result WorkerResult) {
	result = WorkerResult{
		Schema:     workerResultSchema,
		Row:        input.Row,
		Status:     "failed",
		StartedUTC: time.Now().UTC().Format(time.RFC3339Nano),
		Before:     collectProcessMetrics(),
	}
	defer func() {
		result.After = collectProcessMetrics()
		result.FinishedUTC = time.Now().UTC().Format(time.RFC3339Nano)
	}()

	arenaMiB := 0
	if input.Row.ArenaMiB != nil {
		arenaMiB = *input.Row.ArenaMiB
	}
	profile := input.Row.CPUProfile
	if profile == "" {
		profile = "none"
	}
	script, err := RenderEvaluatorTemplate(EvaluatorTemplateData{
		ArenaMiB: arenaMiB, Sentinel: 0xd3, Seed: input.Seed, CPUProfile: profile,
	})
	if err != nil {
		result.Error = err.Error()
		return result
	}
	tempDir, err := os.MkdirTemp("", "agent-python-ultimate-worker-")
	if err != nil {
		result.Error = err.Error()
		return result
	}
	defer os.RemoveAll(tempDir)
	scriptPath := filepath.Join(tempDir, "evaluator.py")
	if err := os.WriteFile(scriptPath, []byte(script), 0o600); err != nil {
		result.Error = err.Error()
		return result
	}

	collector := &phaseCollector{}
	lifecycle, snapshotMode := productionLifecycle(input.Row.Lifecycle)
	cacheDir := input.CompileCacheDir
	if input.Row.CacheState == "cold" {
		cacheDir = filepath.Join(tempDir, "cold-compile-cache")
	}
	dispatcher := wasmexec.NewAgentPythonDispatcher(wasmexec.Config{
		ModulePath:                  input.ArtifactPath,
		AgentPythonManifestPath:     input.ManifestPath,
		PythonScriptPath:            scriptPath,
		PythonLifecycle:             lifecycle,
		SnapshotMode:                snapshotMode,
		MaxInstances:                input.Row.Pool,
		PythonPreparedCapacity:      input.Row.PreparedCapacity,
		PythonPreloadMode:           "evaluator",
		PythonSnapshotHeadroomBytes: 32 << 20,
		MaxMemoryPages:              16384,
		Timeout:                     2 * time.Minute,
		CompileCacheDir:             cacheDir,
		AgentPythonObserver:         collector.observe,
	}, zap.NewNop())

	startup := time.Now()
	err = dispatcher.Start(ctx)
	result.StartupDuration = time.Since(startup)
	if err != nil {
		result.Error = err.Error()
		result.Phases = collector.snapshot()
		return result
	}
	defer func() {
		result.BeforeShutdown = collectProcessMetrics()
		if health, healthErr := dispatcher.Send(context.Background(), "healthcheck", nil); healthErr == nil {
			result.PreparedHits, result.PreparedMisses, result.PreparedRefills = healthcheckPoolStats(health)
		}
		shutdown := time.Now()
		shutdownErr := dispatcher.Shutdown(context.Background())
		result.ShutdownDuration = time.Since(shutdown)
		if shutdownErr != nil && result.Error == "" {
			result.Error = shutdownErr.Error()
			result.Status = "failed"
		}
		result.Phases = collector.snapshot()
	}()

	var httpServer *httptest.Server
	var httpClient *http.Client
	if input.Row.Surface == "http" {
		runtimeHandler, handlerErr := shimmyruntime.NewRuntimeHandler(shimmyruntime.HandlerParams{
			Runtime: dispatcherRuntime{dispatcher: dispatcher}, Log: zap.NewNop(),
		})
		if handlerErr != nil {
			result.Error = handlerErr.Error()
			return result
		}
		commandHandler := shimmyhandler.NewCommandHandler(shimmyhandler.CommandHandlerParams{
			Handler: runtimeHandler, Config: shimmyconfig.Config{}, Log: zap.NewNop(),
		})
		mux := http.NewServeMux()
		mux.Handle("/health", http.HandlerFunc(shimmyhandler.HealthHandler))
		mux.Handle("/", commandHandler)
		httpServer = httptest.NewServer(mux)
		defer httpServer.Close()
		httpClient = httpServer.Client()
		readyStart := time.Now()
		readyResponse, readyErr := httpClient.Get(httpServer.URL + "/health")
		result.HTTPReadyDuration = time.Since(readyStart)
		if readyErr != nil {
			result.Error = readyErr.Error()
			return result
		}
		_, _ = io.Copy(io.Discard, readyResponse.Body)
		_ = readyResponse.Body.Close()
		if readyResponse.StatusCode != http.StatusOK {
			result.Error = fmt.Sprintf("HTTP ready status %d", readyResponse.StatusCode)
			return result
		}
	}

	result.SnapshotRequested = snapshotMode
	for _, event := range collector.snapshot() {
		if event.Phase == wasmexec.AgentPythonPhaseSnapshotTake {
			result.SnapshotSelected = event.SnapshotSelected
		}
	}
	if snapshotMode != "" && result.SnapshotSelected != input.Row.SnapshotSelected {
		result.Status = "unavailable"
		result.Error = fmt.Sprintf("requested snapshot %q selected %q", input.Row.SnapshotSelected, result.SnapshotSelected)
		return result
	}

	response, err := workerResponse(input.Row, input.Seed)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	concurrency := 1
	if input.Row.Concurrency != nil {
		concurrency = *input.Row.Concurrency
	}
	calls := input.Row.Repeat
	if calls < concurrency {
		calls = concurrency
	}
	result.Requests = make([]RequestSample, calls)

	var wg sync.WaitGroup
	sem := make(chan struct{}, concurrency)
	for i := 0; i < calls; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			result.Requests[index] = runWorkerRequest(ctx, dispatcher, input.Row, response, input.Seed, index, httpServer, httpClient)
		}(i)
	}
	wg.Wait()

	failed := 0
	for _, sample := range result.Requests {
		if sample.Outcome != "ok" {
			failed++
		}
	}
	if failed != 0 {
		result.Error = fmt.Sprintf("%d/%d requests failed", failed, len(result.Requests))
		return result
	}
	result.Status = "ok"
	return result
}

func productionLifecycle(lifecycle Lifecycle) (string, string) {
	switch lifecycle {
	case LifecycleFresh:
		return "fresh", ""
	case LifecycleSingleUse:
		return "single-use", ""
	case LifecycleSnapshotMemcpy:
		return "snapshot", "memcpy"
	case LifecycleSnapshotCow:
		return "snapshot", "cow"
	default:
		return string(lifecycle), ""
	}
}

func workerResponse(row PlanRow, seed int64) (any, error) {
	if row.InputBytes == nil {
		return map[string]any{"label": row.ID, "seed": seed}, nil
	}
	shape := row.PayloadShape
	if shape == "" {
		shape = PayloadShapeFlatASCII
	}
	raw, err := BuildInputPayloadByShape(shape, *row.InputBytes)
	if err != nil {
		return nil, err
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	return value, nil
}

func runWorkerRequest(
	ctx context.Context,
	dispatcher *wasmexec.AgentPythonDispatcher,
	row PlanRow,
	response any,
	seed int64,
	index int,
	httpServer *httptest.Server,
	httpClient *http.Client,
) RequestSample {
	sample := RequestSample{Index: index, StartedUTC: time.Now().UTC().Format(time.RFC3339Nano), Outcome: "failed"}
	baseParams := map[string]any{
		"seed": seed + int64(index), "cpu_profile": row.CPUProfile,
		"dirty_pattern": row.DirtyPattern, "output_bytes": intValue(row.OutputBytes),
	}
	if row.DirtyBps != nil {
		baseParams["dirty_bps"] = *row.DirtyBps
	}
	started := time.Now()
	if row.Fault != "" {
		faultName := row.Fault
		if faultName == "recovery" {
			faultName = "exception"
		}
		faultParams := cloneAnyMap(baseParams)
		faultParams["fault"] = faultName
		faultResponse := response
		if faultName == "oversized-input" {
			faultResponse = map[string]any{"payload": string(make([]byte, MaxPayloadBytes+1))}
		}
		if faultName == "oversized-output" {
			faultParams["output_bytes"] = MaxPayloadBytes + 1
		}
		if faultName == "memory-growth" {
			faultParams["growth_bytes"] = 128 << 20
		}
		faultCtx := ctx
		cancelFault := func() {}
		switch faultName {
		case "timeout":
			faultCtx, cancelFault = context.WithTimeout(ctx, time.Millisecond)
			faultParams["cpu_profile"] = "python-5m"
		case "cancel":
			faultCtx, cancelFault = context.WithCancel(ctx)
			cancelFault()
		}
		faultPayload := workerCallPayload(faultResponse, faultParams)
		_, faultErr := executeWorkerCall(faultCtx, dispatcher, faultPayload, httpServer, httpClient)
		cancelFault()
		expectFaultError := faultName != "memory-growth" || row.Lifecycle == LifecycleSnapshotMemcpy || row.Lifecycle == LifecycleSnapshotCow
		faultObserved := faultErr != nil
		if expectFaultError && !faultObserved {
			sample.Duration = time.Since(started)
			sample.Error = fmt.Sprintf("fault %q unexpectedly succeeded", faultName)
			return sample
		}
		if !expectFaultError && faultObserved {
			sample.Duration = time.Since(started)
			sample.Error = fmt.Sprintf("fault %q unexpectedly failed: %v", faultName, faultErr)
			return sample
		}
	}
	normalPayload := workerCallPayload(response, baseParams)
	_, err := executeWorkerCall(ctx, dispatcher, normalPayload, httpServer, httpClient)
	sample.Duration = time.Since(started)
	if err != nil {
		sample.Error = err.Error()
		return sample
	}
	sample.Outcome = "ok"
	return sample
}

func workerCallPayload(response any, params map[string]any) map[string]any {
	return map[string]any{"response": response, "answer": "expected", "params": params}
}

func cloneAnyMap(source map[string]any) map[string]any {
	clone := make(map[string]any, len(source))
	for key, value := range source {
		clone[key] = value
	}
	return clone
}

func executeWorkerCall(
	ctx context.Context,
	dispatcher *wasmexec.AgentPythonDispatcher,
	payload map[string]any,
	httpServer *httptest.Server,
	httpClient *http.Client,
) (map[string]any, error) {
	if httpServer == nil {
		return dispatcher.Send(ctx, "eval", payload)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, httpServer.URL+"/", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Command", "eval")
	response, err := httpClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(response.Body, 2*MaxPayloadBytes+1))
	if readErr != nil {
		return nil, readErr
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP status %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, fmt.Errorf("decode HTTP response: %w", err)
	}
	return decoded, nil
}

func intValue(value *int) int {
	if value == nil {
		return 0
	}
	return *value
}

func healthcheckPoolStats(health map[string]any) (uint64, uint64, uint64) {
	result, _ := health["result"].(map[string]any)
	profile, _ := result["profile"].(map[string]any)
	return uintFromAny(profile["prepared_hits"]), uintFromAny(profile["prepared_misses"]), uintFromAny(profile["prepared_refills"])
}

func uintFromAny(value any) uint64 {
	switch typed := value.(type) {
	case uint64:
		return typed
	case int:
		if typed >= 0 {
			return uint64(typed)
		}
	case float64:
		if typed >= 0 {
			return uint64(typed)
		}
	}
	return 0
}

func WriteWorkerResult(path string, result WorkerResult) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	temp := path + ".tmp"
	if err := os.WriteFile(temp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(temp, path)
}
