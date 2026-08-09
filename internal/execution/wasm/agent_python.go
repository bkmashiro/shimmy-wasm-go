package wasm

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"go.uber.org/zap"
)

const (
	agentPythonDefaultMemoryPages = 8192
	agentPythonMaxMemoryPages     = 16384
	agentPythonDiagnosticMax      = 16 * 1024
)

// AgentPythonDispatcher consumes the clean Agent Python Runtime v1 artifact.
// The artifact is compiled once. Module ownership is selected explicitly by
// PythonLifecycle: fresh, never-served single-use candidates, or prepared
// linear-memory snapshot restore.
type AgentPythonDispatcher struct {
	cfg Config
	log *zap.Logger

	mu       sync.Mutex
	started  bool
	closed   bool
	closedCh chan struct{}
	pending  sync.WaitGroup

	runtime          wazero.Runtime
	compiled         wazero.CompiledModule
	cache            wazero.CompilationCache
	artifact         *AgentPythonArtifact
	script           string
	slots            chan struct{}
	prepared         chan *agentPythonModuleSlot
	snapshotSelected string

	refillCtx       context.Context
	refillCancel    context.CancelFunc
	refillMu        sync.Mutex
	refillInFlight  int
	refills         sync.WaitGroup
	preparedHits    atomic.Uint64
	preparedMisses  atomic.Uint64
	preparedRefills atomic.Uint64

	runCounter  atomic.Uint64
	slotCounter atomic.Uint64
}

type agentPythonModuleSlot struct {
	id               uint64
	module           api.Module
	diagnostic       *agentPythonDiagnosticBuffer
	strategy         SnapshotStrategy
	baselineSize     uint32
	snapshotSelected string
	cowSupport       *cowRuntimeSupport
	cowImage         *cowImageCoordinator
}

func (slot *agentPythonModuleSlot) close(ctx context.Context) error {
	if slot == nil {
		return nil
	}
	var moduleErr, strategyErr, supportErr, imageErr error
	if slot.module != nil {
		moduleErr = slot.module.Close(ctx)
		slot.module = nil
	}
	if slot.strategy != nil {
		strategyErr = slot.strategy.Close()
		slot.strategy = nil
	}
	if slot.cowSupport != nil {
		supportErr = slot.cowSupport.Close()
		slot.cowSupport = nil
	}
	if slot.cowImage != nil {
		imageErr = slot.cowImage.Close()
		slot.cowImage = nil
	}
	return errors.Join(moduleErr, strategyErr, supportErr, imageErr)
}

func NewAgentPythonDispatcher(cfg Config, log *zap.Logger) *AgentPythonDispatcher {
	if log == nil {
		log = zap.NewNop()
	}
	return &AgentPythonDispatcher{
		cfg:      cfg,
		log:      log.Named("dispatcher_agent_python"),
		closedCh: make(chan struct{}),
	}
}

func (d *AgentPythonDispatcher) Start(ctx context.Context) error {
	d.mu.Lock()
	startupObserver := d.cfg.AgentPythonObserver
	var startupEvents []AgentPythonPhaseEvent
	if startupObserver != nil {
		// Start serializes dispatcher state under d.mu, but external observers must
		// never run in that lock domain: they may synchronously inspect or shut down
		// the dispatcher. Capture already-timed immutable events and flush them in
		// order after releasing the lock.
		d.cfg.AgentPythonObserver = func(event AgentPythonPhaseEvent) {
			startupEvents = append(startupEvents, event)
		}
	}
	defer func() {
		d.cfg.AgentPythonObserver = startupObserver
		d.mu.Unlock()
		for _, event := range startupEvents {
			d.emitAgentPythonPhaseEvent(startupObserver, event)
		}
	}()
	if d.closed {
		return errors.New("python-reactor: dispatcher is shut down")
	}
	if d.started {
		return nil
	}

	d.cfg.applyEnv()
	if d.cfg.Timeout == 0 {
		d.cfg.Timeout = 30 * time.Second
	}
	if d.cfg.MaxMemoryPages == 0 {
		d.cfg.MaxMemoryPages = agentPythonDefaultMemoryPages
	}
	if d.cfg.MaxMemoryPages > agentPythonMaxMemoryPages {
		return fmt.Errorf("python-reactor: memory limit %d pages exceeds hard bound %d", d.cfg.MaxMemoryPages, agentPythonMaxMemoryPages)
	}
	if d.cfg.MaxInstances <= 0 {
		d.cfg.MaxInstances = runtime.NumCPU()
		if d.cfg.MaxInstances > 4 {
			d.cfg.MaxInstances = 4
		}
		if d.cfg.MaxInstances < 1 {
			d.cfg.MaxInstances = 1
		}
	}
	if d.cfg.PythonPreloadMode == "" {
		d.cfg.PythonPreloadMode = "evaluator"
	}
	d.cfg.applyAgentPythonDefaults()
	if err := d.cfg.validatePythonPreloadMode(); err != nil {
		return fmt.Errorf("python-reactor: %w", err)
	}
	if err := d.cfg.validateAgentPythonLifecycle(); err != nil {
		return fmt.Errorf("python-reactor: %w", err)
	}
	if d.cfg.PythonLifecycle == "snapshot" {
		if err := d.cfg.validateSnapshotMode(d.cfg.MaxInstances); err != nil {
			return fmt.Errorf("python-reactor: %w", err)
		}
	}
	if len(d.cfg.AllowedPaths) != 0 {
		return errors.New("agent-python does not expose Host filesystem paths; unset FUNCTION_WASM_ALLOWED_PATHS")
	}
	if d.cfg.PythonScriptPath == "" {
		return errors.New("python-reactor: PythonScriptPath must be set (FUNCTION_WASM_PYTHON_SCRIPT)")
	}
	scriptBytes, err := os.ReadFile(d.cfg.PythonScriptPath)
	if err != nil {
		return fmt.Errorf("python-reactor: read script %q: %w", d.cfg.PythonScriptPath, err)
	}
	if len(scriptBytes) == 0 || len(scriptBytes) > agentPythonPayloadMax {
		return fmt.Errorf("python-reactor: trusted script size %d is outside the 1 MiB guest bound", len(scriptBytes))
	}

	phaseStart := time.Now()
	artifact, err := verifyAgentPythonArtifact(d.cfg.ModulePath, d.cfg.AgentPythonManifestPath)
	d.observeAgentPythonPhase(AgentPythonPhaseObservation{
		Phase: AgentPythonPhaseArtifactVerify, Purpose: AgentPythonPurposeStartup,
		Started: phaseStart, Outcome: agentPythonPhaseOutcome(err), Err: err,
	})
	if err != nil {
		return err
	}

	runtimeConfig := wazero.NewRuntimeConfig().
		WithCloseOnContextDone(true).
		WithMemoryLimitPages(d.cfg.MaxMemoryPages)
	var cache wazero.CompilationCache
	if d.cfg.CompileCacheDir != "" {
		cache, err = wazero.NewCompilationCacheWithDir(d.cfg.CompileCacheDir)
		if err != nil {
			return fmt.Errorf("python-reactor: create compilation cache: %w", err)
		}
		runtimeConfig = runtimeConfig.WithCompilationCache(cache)
	}

	phaseStart = time.Now()
	wasmRuntime := wazero.NewRuntimeWithConfig(ctx, runtimeConfig)
	d.observeAgentPythonPhase(AgentPythonPhaseObservation{
		Phase: AgentPythonPhaseRuntimeCreate, Purpose: AgentPythonPurposeStartup,
		Started: phaseStart, Outcome: AgentPythonOutcomeOK,
	})
	closePartial := func() {
		_ = wasmRuntime.Close(context.Background())
		if cache != nil {
			_ = cache.Close(context.Background())
		}
	}
	phaseStart = time.Now()
	_, err = wasi_snapshot_preview1.Instantiate(ctx, wasmRuntime)
	d.observeAgentPythonPhase(AgentPythonPhaseObservation{
		Phase: AgentPythonPhaseWASIImports, Purpose: AgentPythonPurposeStartup,
		Started: phaseStart, Outcome: agentPythonPhaseOutcome(err), Err: err,
	})
	if err != nil {
		closePartial()
		return fmt.Errorf("python-reactor: instantiate WASI imports: %w", err)
	}
	phaseStart = time.Now()
	_, err = wasmRuntime.NewHostModuleBuilder("agent_runtime_v1").
		NewFunctionBuilder().
		WithFunc(agentPythonDeniedHostCall).
		Export("host_call").
		Instantiate(ctx)
	d.observeAgentPythonPhase(AgentPythonPhaseObservation{
		Phase: AgentPythonPhaseHostImports, Purpose: AgentPythonPurposeStartup,
		Started: phaseStart, Outcome: agentPythonPhaseOutcome(err), Err: err,
	})
	if err != nil {
		closePartial()
		return fmt.Errorf("python-reactor: instantiate Host imports: %w", err)
	}
	phaseStart = time.Now()
	compiled, err := wasmRuntime.CompileModule(ctx, artifact.WasmBytes)
	d.observeAgentPythonPhase(AgentPythonPhaseObservation{
		Phase: AgentPythonPhaseCompile, Purpose: AgentPythonPurposeStartup,
		Started: phaseStart, Outcome: agentPythonPhaseOutcome(err), Err: err,
	})
	if err != nil {
		closePartial()
		return fmt.Errorf("python-reactor: compile guest: %w", err)
	}
	if err := verifyCompiledPythonReactorArtifact(compiled, artifact); err != nil {
		closePartial()
		return err
	}

	d.runtime = wasmRuntime
	d.compiled = compiled
	d.cache = cache
	d.artifact = artifact
	d.script = string(scriptBytes)
	d.slots = make(chan struct{}, d.cfg.MaxInstances)
	d.refillCtx, d.refillCancel = context.WithCancel(context.Background())

	switch d.cfg.PythonLifecycle {
	case "snapshot":
		d.prepared = make(chan *agentPythonModuleSlot, d.cfg.MaxInstances)
		for i := 0; i < d.cfg.MaxInstances; i++ {
			slot, err := d.newPreparedModuleSlot(ctx, true, AgentPythonPurposeStartup, 0)
			if err != nil {
				_ = d.closeRuntime(context.Background())
				return err
			}
			if i == 0 {
				d.snapshotSelected = slot.snapshotSelected
			} else if slot.snapshotSelected != d.snapshotSelected {
				_ = slot.close(context.Background())
				_ = d.closeRuntime(context.Background())
				return fmt.Errorf("python-reactor: snapshot strategy selected inconsistently across slots: %q then %q", d.snapshotSelected, slot.snapshotSelected)
			}
			d.prepared <- slot
		}
	case "single-use":
		d.prepared = make(chan *agentPythonModuleSlot, d.cfg.PythonPreparedCapacity)
		for i := 0; i < d.cfg.PythonPreparedCapacity; i++ {
			slot, err := d.newPreparedModuleSlot(ctx, false, AgentPythonPurposeStartup, 0)
			if err != nil {
				_ = d.closeRuntime(context.Background())
				return err
			}
			d.prepared <- slot
		}
	case "fresh":
		// Probe the exact artifact and trusted script before reporting readiness.
		slot, err := d.newPreparedModuleSlot(ctx, false, AgentPythonPurposeStartup, 0)
		if err != nil {
			_ = d.closeRuntime(context.Background())
			return err
		}
		_ = slot.close(context.Background())
	}

	d.started = true
	d.log.Info("agent-python dispatcher ready",
		zap.String("artifact_sha256", artifact.SHA256),
		zap.String("producer_commit", artifact.ProducerCommit),
		zap.String("artifact_profile", artifact.Profile),
		zap.Int("max_instances", d.cfg.MaxInstances),
		zap.String("lifecycle", d.cfg.PythonLifecycle),
		zap.String("snapshot_mode", d.cfg.SnapshotMode),
		zap.String("reset_mode", d.resetMode()),
	)
	return nil
}

func (d *AgentPythonDispatcher) Send(ctx context.Context, method string, params map[string]any) (map[string]any, error) {
	if method == "healthcheck" {
		d.mu.Lock()
		ready := d.started && !d.closed
		profile := ""
		if d.artifact != nil {
			profile = d.artifact.Profile
		}
		preparedReady := len(d.prepared)
		d.mu.Unlock()
		if !ready {
			return nil, errors.New("python-reactor: dispatcher is not ready")
		}
		return map[string]any{
			"command": "healthcheck",
			"result": map[string]any{
				"status":            "ok",
				"profile":           profile,
				"lifecycle":         d.cfg.PythonLifecycle,
				"snapshot_mode":     d.cfg.SnapshotMode,
				"snapshot_selected": d.snapshotSelected,
				"reset_mode":        d.resetMode(),
				"prepared_ready":    preparedReady,
				"prepared_hits":     d.preparedHits.Load(),
				"prepared_misses":   d.preparedMisses.Load(),
				"prepared_refills":  d.preparedRefills.Load(),
			},
		}, nil
	}
	if !d.tryBeginSend() {
		return nil, errors.New("python-reactor: dispatcher is not ready")
	}
	defer d.pending.Done()

	select {
	case d.slots <- struct{}{}:
		defer func() { <-d.slots }()
	case <-d.closedCh:
		return nil, errors.New("python-reactor: dispatcher is shut down")
	case <-ctx.Done():
		return nil, fmt.Errorf("python-reactor: acquire execution slot: %w", ctx.Err())
	}

	requestID := d.runCounter.Add(1)
	runID := fmt.Sprintf("shimmy-%s-%d", d.artifact.SHA256[:12], requestID)
	scriptInRequest := ""
	if d.cfg.PythonPreloadMode == "off" {
		scriptInRequest = d.script
	}
	request, err := buildAgentPythonRunRequest(runID, method, params, scriptInRequest)
	if err != nil {
		return nil, err
	}

	runContext, cancel := context.WithTimeout(ctx, d.cfg.Timeout)
	defer cancel()

	var slot *agentPythonModuleSlot
	checkoutStart := time.Now()
	switch d.cfg.PythonLifecycle {
	case "snapshot":
		slot, err = acquireAgentPythonSnapshotSlot(
			runContext,
			d.prepared,
			d.closedCh,
			func(createContext context.Context) (*agentPythonModuleSlot, error) {
				return d.newPreparedModuleSlot(createContext, true, AgentPythonPurposeReplacement, requestID)
			},
		)
		if err != nil {
			return nil, err
		}
		if slot.snapshotSelected != d.snapshotSelected {
			_ = slot.close(context.Background())
			return nil, fmt.Errorf("python-reactor: replenished snapshot strategy %q, want %q", slot.snapshotSelected, d.snapshotSelected)
		}
	case "single-use":
		select {
		case slot = <-d.prepared:
			d.preparedHits.Add(1)
		default:
			d.preparedMisses.Add(1)
		}
		d.scheduleSingleUseRefill(requestID)
		if slot == nil {
			slot, err = d.newPreparedModuleSlot(runContext, false, AgentPythonPurposeFresh, requestID)
			if err != nil {
				return nil, err
			}
		}
	case "fresh":
		slot, err = d.newPreparedModuleSlot(runContext, false, AgentPythonPurposeFresh, requestID)
		if err != nil {
			return nil, err
		}
	}
	d.observeAgentPythonPhase(AgentPythonPhaseObservation{
		Phase: AgentPythonPhaseCheckout, Purpose: AgentPythonPurposeRequest,
		RequestID: requestID, SlotID: slot.id, Started: checkoutStart,
		MemoryBytes: uint64(slot.module.Memory().Size()), SnapshotSelected: slot.snapshotSelected,
		Outcome: AgentPythonOutcomeOK,
	})
	if d.cfg.PythonLifecycle != "snapshot" {
		defer func() {
			phaseStart := time.Now()
			closeErr := slot.close(context.Background())
			d.observeAgentPythonPhase(AgentPythonPhaseObservation{
				Phase: AgentPythonPhaseClose, Purpose: AgentPythonPurposeRequest,
				RequestID: requestID, SlotID: slot.id, Started: phaseStart,
				Outcome: agentPythonPhaseOutcome(closeErr), Err: closeErr,
			})
		}()
	}

	phaseStart := time.Now()
	payload, callErr := callAgentPythonExecute(runContext, slot.module, request)
	if callErr != nil && runContext.Err() != nil {
		callErr = errors.Join(callErr, runContext.Err())
	}
	d.observeAgentPythonPhase(AgentPythonPhaseObservation{
		Phase: AgentPythonPhaseExecute, Purpose: AgentPythonPurposeRequest,
		RequestID: requestID, SlotID: slot.id, Started: phaseStart,
		MemoryBytes: uint64(slot.module.Memory().Size()), SnapshotSelected: slot.snapshotSelected,
		Outcome: agentPythonPhaseOutcome(callErr), Err: callErr,
	})

	if d.cfg.PythonLifecycle == "snapshot" {
		var restoreErr error
		if callErr == nil {
			phaseStart = time.Now()
			restoreErr = restoreAgentPythonSnapshot(slot)
			d.observeAgentPythonPhase(AgentPythonPhaseObservation{
				Phase: AgentPythonPhaseRestore, Purpose: AgentPythonPurposeRequest,
				RequestID: requestID, SlotID: slot.id, Started: phaseStart,
				MemoryBytes: uint64(slot.module.Memory().Size()), SnapshotSelected: slot.snapshotSelected,
				Outcome: agentPythonPhaseOutcome(restoreErr), Err: restoreErr,
			})
		}
		if callErr != nil || restoreErr != nil {
			diagnostic := slot.diagnostic.String()
			_ = slot.close(context.Background())
			replacementErr := d.replaceSnapshotSlot(requestID)
			return nil, withAgentPythonDiagnostic(errors.Join(callErr, restoreErr, replacementErr), diagnostic)
		}
		slot.diagnostic.Reset()
		d.prepared <- slot
	}
	if callErr != nil {
		return nil, withAgentPythonDiagnostic(callErr, slot.diagnostic.String())
	}
	phaseStart = time.Now()
	result, err := decodeAgentPythonResponse(payload)
	d.observeAgentPythonPhase(AgentPythonPhaseObservation{
		Phase: AgentPythonPhaseDecode, Purpose: AgentPythonPurposeRequest,
		RequestID: requestID, SlotID: slot.id, Started: phaseStart,
		SnapshotSelected: slot.snapshotSelected,
		Outcome:          agentPythonPhaseOutcome(err), Err: err,
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"command": method, "result": result}, nil
}

func acquireAgentPythonSnapshotSlot(
	ctx context.Context,
	prepared <-chan *agentPythonModuleSlot,
	closed <-chan struct{},
	create func(context.Context) (*agentPythonModuleSlot, error),
) (*agentPythonModuleSlot, error) {
	select {
	case slot := <-prepared:
		if slot != nil {
			return slot, nil
		}
	case <-closed:
		return nil, errors.New("python-reactor: dispatcher is shut down")
	case <-ctx.Done():
		return nil, fmt.Errorf("python-reactor: acquire prepared module: %w", ctx.Err())
	default:
	}

	slot, err := create(ctx)
	if err != nil {
		return nil, fmt.Errorf("python-reactor: replenish missing prepared snapshot slot: %w", err)
	}
	if slot == nil {
		return nil, errors.New("python-reactor: replenish missing prepared snapshot slot returned nil")
	}
	return slot, nil
}

func (d *AgentPythonDispatcher) resetMode() string {
	switch d.cfg.PythonLifecycle {
	case "snapshot":
		return "linear-memory-" + d.snapshotSelected
	case "single-use":
		return "single-use-prepared"
	default:
		return "fresh-instance"
	}
}

func (d *AgentPythonDispatcher) tryBeginSend() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.started || d.closed {
		return false
	}
	d.pending.Add(1)
	return true
}

func (d *AgentPythonDispatcher) newInitializedModule(
	ctx context.Context,
	prepare bool,
	cowSupport *cowRuntimeSupport,
	purpose AgentPythonPurpose,
	requestID uint64,
	slotID uint64,
) (api.Module, *agentPythonDiagnosticBuffer, error) {
	diagnostic := &agentPythonDiagnosticBuffer{}
	instantiateContext := ctx
	if cowSupport != nil {
		instantiateContext = cowSupport.instantiateContext(ctx)
	}
	phaseStart := time.Now()
	module, err := d.runtime.InstantiateModule(
		instantiateContext,
		d.compiled,
		wazero.NewModuleConfig().WithName("").WithRandSource(cryptorand.Reader).WithStderr(diagnostic),
	)
	memoryBytes := uint64(0)
	if module != nil && module.Memory() != nil {
		memoryBytes = uint64(module.Memory().Size())
	}
	d.observeAgentPythonPhase(AgentPythonPhaseObservation{
		Phase: AgentPythonPhaseInstantiate, Purpose: purpose, RequestID: requestID, SlotID: slotID,
		Started: phaseStart, MemoryBytes: memoryBytes, Outcome: agentPythonPhaseOutcome(err), Err: err,
	})
	if err != nil {
		return nil, diagnostic, fmt.Errorf("python-reactor: instantiate guest: %w", err)
	}
	failed := true
	defer func() {
		if failed {
			_ = module.Close(context.Background())
		}
	}()
	phaseStart = time.Now()
	err = callAgentPythonNoArgs(ctx, module, "_initialize")
	d.observeAgentPythonPhase(AgentPythonPhaseObservation{
		Phase: AgentPythonPhaseInitialize, Purpose: purpose, RequestID: requestID, SlotID: slotID,
		Started: phaseStart, MemoryBytes: uint64(module.Memory().Size()), Outcome: agentPythonPhaseOutcome(err), Err: err,
	})
	if err != nil {
		return nil, diagnostic, err
	}
	phaseStart = time.Now()
	err = callAgentPythonStatus(ctx, module, "runtime_init", []byte("{}"))
	d.observeAgentPythonPhase(AgentPythonPhaseObservation{
		Phase: AgentPythonPhaseRuntimeInit, Purpose: purpose, RequestID: requestID, SlotID: slotID,
		Started: phaseStart, MemoryBytes: uint64(module.Memory().Size()), Outcome: agentPythonPhaseOutcome(err), Err: err,
	})
	if err != nil {
		return nil, diagnostic, err
	}
	if prepare {
		phaseStart = time.Now()
		err = callAgentPythonStatus(ctx, module, "runtime_prepare", []byte(d.script))
		d.observeAgentPythonPhase(AgentPythonPhaseObservation{
			Phase: AgentPythonPhaseRuntimePrepare, Purpose: purpose, RequestID: requestID, SlotID: slotID,
			Started: phaseStart, MemoryBytes: uint64(module.Memory().Size()), Outcome: agentPythonPhaseOutcome(err), Err: err,
		})
		if err != nil {
			return nil, diagnostic, err
		}
	}
	diagnostic.Reset()
	failed = false
	return module, diagnostic, nil
}

func reserveAgentPythonSnapshotHeadroom(ctx context.Context, module api.Module, bytes uint64) (retErr error) {
	if bytes == 0 {
		return nil
	}
	if bytes > math.MaxUint32 {
		return fmt.Errorf("python-reactor: snapshot headroom %d exceeds wasm32 allocation limit", bytes)
	}
	allocate := module.ExportedFunction("alloc")
	deallocate := module.ExportedFunction("dealloc")
	if allocate == nil || deallocate == nil {
		return errors.New("python-reactor: snapshot headroom requires alloc and dealloc exports")
	}

	const chunkBytes = uint64(1024 * 1024)
	pointers := make([]uint64, 0, (bytes+chunkBytes-1)/chunkBytes)
	defer func() {
		for i := len(pointers) - 1; i >= 0; i-- {
			if _, err := deallocate.Call(context.Background(), pointers[i]); err != nil {
				retErr = errors.Join(retErr, fmt.Errorf("python-reactor: release snapshot headroom: %w", err))
			}
		}
	}()

	for remaining := bytes; remaining > 0; {
		chunk := chunkBytes
		if remaining < chunk {
			chunk = remaining
		}
		result, err := allocate.Call(ctx, chunk)
		if err != nil {
			return fmt.Errorf("python-reactor: reserve %d snapshot headroom bytes: %w", bytes, err)
		}
		if len(result) != 1 || result[0] == 0 {
			return fmt.Errorf("python-reactor: reserve %d snapshot headroom bytes: guest allocator returned no pointer", bytes)
		}
		pointers = append(pointers, result[0])
		remaining -= chunk
	}
	return nil
}

func (d *AgentPythonDispatcher) newPreparedModuleSlot(
	ctx context.Context,
	takeSnapshot bool,
	purpose AgentPythonPurpose,
	requestID uint64,
) (*agentPythonModuleSlot, error) {
	slotID := d.slotCounter.Add(1)
	var cowImage *cowImageCoordinator
	var cowSupport *cowRuntimeSupport
	if takeSnapshot && d.cfg.SnapshotMode == "cow" {
		cowImage = newCowImageCoordinator()
		cowSupport = newCowRuntimeSupport("cow", cowImage)
		if cowSupport == nil {
			_ = cowImage.Close()
			cowImage = nil
		}
	}

	module, diagnostic, err := d.newInitializedModule(
		ctx,
		d.cfg.PythonPreloadMode != "off",
		cowSupport,
		purpose,
		requestID,
		slotID,
	)
	if err != nil {
		if cowSupport != nil {
			_ = cowSupport.Close()
		}
		if cowImage != nil {
			_ = cowImage.Close()
		}
		return nil, withAgentPythonDiagnostic(err, diagnostic.String())
	}
	slot := &agentPythonModuleSlot{
		id:         slotID,
		module:     module,
		diagnostic: diagnostic,
		cowSupport: cowSupport,
		cowImage:   cowImage,
	}
	if !takeSnapshot {
		return slot, nil
	}
	phaseStart := time.Now()
	err = reserveAgentPythonSnapshotHeadroom(ctx, module, d.cfg.PythonSnapshotHeadroomBytes)
	d.observeAgentPythonPhase(AgentPythonPhaseObservation{
		Phase: AgentPythonPhaseHeadroom, Purpose: purpose, RequestID: requestID, SlotID: slotID,
		Started: phaseStart, MemoryBytes: uint64(module.Memory().Size()), Outcome: agentPythonPhaseOutcome(err), Err: err,
	})
	if err != nil {
		_ = slot.close(context.Background())
		return nil, err
	}
	phaseStart = time.Now()
	if cowSupport != nil {
		slot.strategy = cowSupport.snapshotStrategy(module.Memory(), d.log)
	} else {
		slot.strategy = selectSnapshotStrategy(d.cfg.SnapshotMode, module.Memory(), d.log)
	}
	slot.snapshotSelected = agentPythonSnapshotStrategyName(slot.strategy)
	d.observeAgentPythonPhase(AgentPythonPhaseObservation{
		Phase: AgentPythonPhaseStrategySelect, Purpose: purpose, RequestID: requestID, SlotID: slotID,
		Started: phaseStart, MemoryBytes: uint64(module.Memory().Size()), SnapshotSelected: slot.snapshotSelected,
		Outcome: AgentPythonOutcomeOK,
	})
	phaseStart = time.Now()
	err = slot.strategy.Take(module.Memory())
	slot.snapshotSelected = agentPythonSnapshotStrategyName(slot.strategy)
	d.observeAgentPythonPhase(AgentPythonPhaseObservation{
		Phase: AgentPythonPhaseSnapshotTake, Purpose: purpose, RequestID: requestID, SlotID: slotID,
		Started: phaseStart, MemoryBytes: uint64(module.Memory().Size()), SnapshotSelected: slot.snapshotSelected,
		Outcome: agentPythonPhaseOutcome(err), Err: err,
	})
	if err != nil {
		_ = slot.close(context.Background())
		return nil, fmt.Errorf("python-reactor: take prepared snapshot: %w", err)
	}
	slot.baselineSize = module.Memory().Size()
	return slot, nil
}

func restoreAgentPythonSnapshot(slot *agentPythonModuleSlot) error {
	if slot == nil || slot.module == nil || slot.strategy == nil {
		return errors.New("python-reactor: prepared snapshot slot is incomplete")
	}
	memory := slot.module.Memory()
	if memory == nil {
		return errors.New("python-reactor: prepared snapshot slot has no memory")
	}
	if memory.Size() != slot.baselineSize {
		return fmt.Errorf("python-reactor: memory size drift: got %d bytes, baseline %d", memory.Size(), slot.baselineSize)
	}
	return slot.strategy.Restore(memory)
}

type snapshotModeReporter interface {
	selectedSnapshotMode() string
}

func agentPythonSnapshotStrategyName(strategy SnapshotStrategy) string {
	if reporter, ok := strategy.(snapshotModeReporter); ok {
		if selected := reporter.selectedSnapshotMode(); selected != "" {
			return selected
		}
	}
	name := fmt.Sprintf("%T", strategy)
	switch {
	case strings.Contains(name, "CowSnapshotStrategy"):
		return "cow"
	case strings.Contains(name, "Uffd"):
		return "uffd"
	case strings.Contains(name, "SoftDirty"):
		return "soft-dirty"
	case strings.Contains(name, "Mprotect"):
		return "mprotect"
	default:
		return "memcpy"
	}
}

func (d *AgentPythonDispatcher) replaceSnapshotSlot(requestID uint64) error {
	d.mu.Lock()
	closed := d.closed
	d.mu.Unlock()
	if closed {
		return nil
	}
	timeout := d.cfg.Timeout
	if timeout < 30*time.Second {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	slot, err := d.newPreparedModuleSlot(ctx, true, AgentPythonPurposeReplacement, requestID)
	if err != nil {
		return fmt.Errorf("python-reactor: replace prepared snapshot slot: %w", err)
	}
	if slot.snapshotSelected != d.snapshotSelected {
		_ = slot.close(context.Background())
		return fmt.Errorf("python-reactor: replacement selected snapshot strategy %q, want %q", slot.snapshotSelected, d.snapshotSelected)
	}
	d.mu.Lock()
	closed = d.closed
	d.mu.Unlock()
	if closed {
		return slot.close(context.Background())
	}
	d.prepared <- slot
	return nil
}

func (d *AgentPythonDispatcher) scheduleSingleUseRefill(requestID uint64) {
	if d.refillCtx == nil || d.prepared == nil {
		return
	}
	d.refillMu.Lock()
	if len(d.prepared)+d.refillInFlight >= cap(d.prepared) {
		d.refillMu.Unlock()
		return
	}
	d.refillInFlight++
	d.refills.Add(1)
	refillCtx := d.refillCtx
	d.refillMu.Unlock()

	go func() {
		defer d.refills.Done()
		defer func() {
			d.refillMu.Lock()
			d.refillInFlight--
			d.refillMu.Unlock()
		}()

		timeout := d.cfg.Timeout
		if timeout < 30*time.Second {
			timeout = 30 * time.Second
		}
		ctx, cancel := context.WithTimeout(refillCtx, timeout)
		defer cancel()
		slot, err := d.newPreparedModuleSlot(ctx, false, AgentPythonPurposeRefill, requestID)
		if err != nil {
			if refillCtx.Err() == nil {
				d.log.Warn("agent-python single-use refill failed", zap.Error(err))
			}
			return
		}
		select {
		case d.prepared <- slot:
			d.preparedRefills.Add(1)
		case <-refillCtx.Done():
			_ = slot.close(context.Background())
		}
	}()
}

func (d *AgentPythonDispatcher) Shutdown(ctx context.Context) error {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil
	}
	d.closed = true
	close(d.closedCh)
	if d.refillCancel != nil {
		d.refillCancel()
	}
	d.mu.Unlock()

	d.pending.Wait()
	d.refills.Wait()
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.closeRuntime(ctx)
}

func (d *AgentPythonDispatcher) closeRuntime(ctx context.Context) error {
	if d.refillCancel != nil {
		d.refillCancel()
		d.refillCancel = nil
	}
	d.refillCtx = nil
	var slotErr error
	if d.prepared != nil {
		for {
			select {
			case slot := <-d.prepared:
				slotErr = errors.Join(slotErr, slot.close(ctx))
			default:
				d.prepared = nil
				goto preparedClosed
			}
		}
	}

preparedClosed:
	var compiledErr, runtimeErr, cacheErr error
	if d.compiled != nil {
		compiledErr = d.compiled.Close(ctx)
		d.compiled = nil
	}
	if d.runtime != nil {
		runtimeErr = d.runtime.Close(ctx)
		d.runtime = nil
	}
	if d.cache != nil {
		cacheErr = d.cache.Close(ctx)
		d.cache = nil
	}
	d.started = false
	return errors.Join(slotErr, compiledErr, runtimeErr, cacheErr)
}

func agentPythonDeniedHostCall(context.Context, api.Module, uint32, uint32, uint32, uint32) int32 {
	return -1
}

func callAgentPythonNoArgs(ctx context.Context, module api.Module, name string) error {
	function := module.ExportedFunction(name)
	if function == nil {
		return fmt.Errorf("python-reactor: required export %q is missing", name)
	}
	if _, err := function.Call(ctx); err != nil {
		return fmt.Errorf("python-reactor: call %s: %w", name, err)
	}
	return nil
}

func callAgentPythonStatus(ctx context.Context, module api.Module, name string, data []byte) error {
	results, release, err := callAgentPythonWithBytes(ctx, module, name, data)
	if release != nil {
		defer release()
	}
	if err != nil {
		return err
	}
	if len(results) != 1 || uint32(results[0]) != 0 {
		return fmt.Errorf("python-reactor: %s returned non-zero status", name)
	}
	return nil
}

func callAgentPythonExecute(ctx context.Context, module api.Module, request []byte) ([]byte, error) {
	results, release, err := callAgentPythonWithBytes(ctx, module, "execute", request)
	if release != nil {
		defer release()
	}
	if err != nil {
		return nil, err
	}
	if len(results) != 1 {
		return nil, errors.New("python-reactor: execute returned an unexpected result count")
	}
	return readAgentPythonResponse(module.Memory(), uint32(results[0]))
}

func callAgentPythonWithBytes(ctx context.Context, module api.Module, name string, data []byte) ([]uint64, func(), error) {
	if len(data) == 0 || len(data) > agentPythonPayloadMax || len(data) > math.MaxUint32 {
		return nil, nil, fmt.Errorf("python-reactor: %s input size %d is outside the guest bound", name, len(data))
	}
	allocate := module.ExportedFunction("alloc")
	deallocate := module.ExportedFunction("dealloc")
	function := module.ExportedFunction(name)
	if allocate == nil || deallocate == nil || function == nil {
		return nil, nil, fmt.Errorf("python-reactor: required allocation or %s export is missing", name)
	}
	allocated, err := allocate.Call(ctx, uint64(uint32(len(data))))
	if err != nil || len(allocated) != 1 || allocated[0] == 0 {
		return nil, nil, fmt.Errorf("python-reactor: guest allocation failed: %w", err)
	}
	pointer := uint32(allocated[0])
	var once sync.Once
	release := func() {
		once.Do(func() {
			releaseContext, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_, _ = deallocate.Call(releaseContext, uint64(pointer))
		})
	}
	if !module.Memory().Write(pointer, data) {
		release()
		return nil, nil, errors.New("python-reactor: guest input write is out of bounds")
	}
	results, err := function.Call(ctx, uint64(pointer), uint64(uint32(len(data))))
	if err != nil {
		release()
		return nil, nil, fmt.Errorf("python-reactor: call %s: %w", name, err)
	}
	return results, release, nil
}

func readAgentPythonResponse(memory api.Memory, pointer uint32) ([]byte, error) {
	if memory == nil {
		return nil, errors.New("python-reactor: guest module has no linear memory")
	}
	header, ok := memory.Read(pointer, 4)
	if !ok {
		return nil, errors.New("python-reactor: response length prefix is out of bounds")
	}
	length := binary.LittleEndian.Uint32(header)
	if length > agentPythonPayloadMax {
		return nil, fmt.Errorf("python-reactor: response payload length %d exceeds limit %d", length, agentPythonPayloadMax)
	}
	if uint64(pointer)+4+uint64(length) > uint64(memory.Size()) {
		return nil, errors.New("python-reactor: response frame is out of bounds")
	}
	payload, ok := memory.Read(pointer+4, length)
	if !ok {
		return nil, errors.New("python-reactor: response payload is out of bounds")
	}
	return append([]byte(nil), payload...), nil
}

type agentPythonDiagnosticBuffer struct {
	data []byte
}

func (buffer *agentPythonDiagnosticBuffer) Write(data []byte) (int, error) {
	length := len(data)
	if length >= agentPythonDiagnosticMax {
		buffer.data = append(buffer.data[:0], data[length-agentPythonDiagnosticMax:]...)
		return length, nil
	}
	if overflow := len(buffer.data) + length - agentPythonDiagnosticMax; overflow > 0 {
		copy(buffer.data, buffer.data[overflow:])
		buffer.data = buffer.data[:len(buffer.data)-overflow]
	}
	buffer.data = append(buffer.data, data...)
	return length, nil
}

func (buffer *agentPythonDiagnosticBuffer) String() string { return string(buffer.data) }
func (buffer *agentPythonDiagnosticBuffer) Reset()         { buffer.data = buffer.data[:0] }

func withAgentPythonDiagnostic(base error, diagnostic string) error {
	if diagnostic == "" {
		return base
	}
	return fmt.Errorf("%w; guest stderr: %s", base, diagnostic)
}
