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
	"sync"
	"sync/atomic"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"go.uber.org/zap"
)

// The packaged NumPy Reactor retains about 128 MiB per prepared instance.
// These experimental limits leave conservative headroom while bounding
// memory.grow at 1 GiB.
const (
	wasmPageSizeBytes               = 64 * 1024
	pythonReactorDefaultMemoryBytes = 512 * 1024 * 1024
	pythonReactorMaxMemoryBytes     = 1024 * 1024 * 1024
	pythonReactorDefaultMemoryPages = pythonReactorDefaultMemoryBytes / wasmPageSizeBytes
	pythonReactorMaxMemoryPages     = pythonReactorMaxMemoryBytes / wasmPageSizeBytes
	pythonReactorDiagnosticMaxBytes = 16 * 1024
)

// PythonReactorDispatcher consumes the clean Python Reactor Runtime v1 artifact.
// The artifact is compiled once. Module ownership is selected explicitly by
// PythonLifecycle: fresh, never-served single-use candidates, or prepared
// linear-memory snapshot restore.
type PythonReactorDispatcher struct {
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
	artifact         *PythonReactorArtifact
	script           string
	slots            chan struct{}
	prepared         chan *pythonReactorModuleSlot
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

type pythonReactorModuleSlot struct {
	id               uint64
	module           api.Module
	diagnostic       *pythonReactorDiagnosticBuffer
	strategy         SnapshotStrategy
	baselineSize     uint32
	snapshotSelected string
}

func (slot *pythonReactorModuleSlot) close(ctx context.Context) error {
	if slot == nil {
		return nil
	}
	var moduleErr, strategyErr error
	if slot.module != nil {
		moduleErr = slot.module.Close(ctx)
		slot.module = nil
	}
	if slot.strategy != nil {
		strategyErr = slot.strategy.Close()
		slot.strategy = nil
	}
	return errors.Join(moduleErr, strategyErr)
}

func NewPythonReactorDispatcher(cfg Config, log *zap.Logger) *PythonReactorDispatcher {
	if log == nil {
		log = zap.NewNop()
	}
	return &PythonReactorDispatcher{
		cfg:      cfg,
		log:      log.Named("dispatcher_python_reactor"),
		closedCh: make(chan struct{}),
	}
}

func (d *PythonReactorDispatcher) Start(ctx context.Context) error {
	d.mu.Lock()
	startupObserver := d.cfg.PythonReactorObserver
	var startupEvents []PythonReactorPhaseEvent
	if startupObserver != nil {
		// Start serializes dispatcher state under d.mu, but external observers must
		// never run in that lock domain: they may synchronously inspect or shut down
		// the dispatcher. Capture already-timed immutable events and flush them in
		// order after releasing the lock.
		d.cfg.PythonReactorObserver = func(event PythonReactorPhaseEvent) {
			startupEvents = append(startupEvents, event)
		}
	}
	defer func() {
		d.cfg.PythonReactorObserver = startupObserver
		d.mu.Unlock()
		for _, event := range startupEvents {
			d.emitPythonReactorPhaseEvent(startupObserver, event)
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
		d.cfg.MaxMemoryPages = pythonReactorDefaultMemoryPages
	}
	if d.cfg.MaxMemoryPages > pythonReactorMaxMemoryPages {
		return fmt.Errorf("python-reactor: memory limit %d pages exceeds hard bound %d", d.cfg.MaxMemoryPages, pythonReactorMaxMemoryPages)
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
	d.cfg.applyPythonReactorDefaults()
	if err := d.cfg.validatePythonReactorLifecycle(); err != nil {
		return fmt.Errorf("python-reactor: %w", err)
	}
	if len(d.cfg.AllowedPaths) != 0 {
		return errors.New("python-reactor does not expose Host filesystem paths; unset FUNCTION_WASM_ALLOWED_PATHS")
	}
	if d.cfg.PythonScriptPath == "" {
		return errors.New("python-reactor: PythonScriptPath must be set (FUNCTION_WASM_PYTHON_SCRIPT)")
	}
	scriptBytes, err := os.ReadFile(d.cfg.PythonScriptPath)
	if err != nil {
		return fmt.Errorf("python-reactor: read script %q: %w", d.cfg.PythonScriptPath, err)
	}
	if len(scriptBytes) == 0 || len(scriptBytes) > pythonReactorPayloadMaxBytes {
		return fmt.Errorf("python-reactor: trusted script size %d is outside the 1 MiB guest bound", len(scriptBytes))
	}

	phaseStart := time.Now()
	artifact, err := verifyPythonReactorArtifact(d.cfg.ModulePath, d.cfg.PythonReactorManifestPath)
	d.observePythonReactorPhase(PythonReactorPhaseObservation{
		Phase: PythonReactorPhaseArtifactVerify, Purpose: PythonReactorPurposeStartup,
		Started: phaseStart, Outcome: pythonReactorPhaseOutcome(err), Err: err,
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
	d.observePythonReactorPhase(PythonReactorPhaseObservation{
		Phase: PythonReactorPhaseRuntimeCreate, Purpose: PythonReactorPurposeStartup,
		Started: phaseStart, Outcome: PythonReactorOutcomeOK,
	})
	closePartial := func() {
		_ = wasmRuntime.Close(context.Background())
		if cache != nil {
			_ = cache.Close(context.Background())
		}
	}
	phaseStart = time.Now()
	_, err = wasi_snapshot_preview1.Instantiate(ctx, wasmRuntime)
	d.observePythonReactorPhase(PythonReactorPhaseObservation{
		Phase: PythonReactorPhaseWASIImports, Purpose: PythonReactorPurposeStartup,
		Started: phaseStart, Outcome: pythonReactorPhaseOutcome(err), Err: err,
	})
	if err != nil {
		closePartial()
		return fmt.Errorf("python-reactor: instantiate WASI imports: %w", err)
	}
	phaseStart = time.Now()
	compiled, err := wasmRuntime.CompileModule(ctx, artifact.WasmBytes)
	d.observePythonReactorPhase(PythonReactorPhaseObservation{
		Phase: PythonReactorPhaseCompile, Purpose: PythonReactorPurposeStartup,
		Started: phaseStart, Outcome: pythonReactorPhaseOutcome(err), Err: err,
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
		d.prepared = make(chan *pythonReactorModuleSlot, d.cfg.MaxInstances)
		for i := 0; i < d.cfg.MaxInstances; i++ {
			slot, err := d.newPreparedModuleSlotWithPrepareTimeout(ctx, true, PythonReactorPurposeStartup, 0)
			if err != nil {
				_ = d.closeRuntime(context.Background())
				return err
			}
			d.snapshotSelected = "memcpy"
			d.prepared <- slot
		}
	case "single-use":
		d.prepared = make(chan *pythonReactorModuleSlot, d.cfg.PythonPreparedCapacity)
		for i := 0; i < d.cfg.PythonPreparedCapacity; i++ {
			slot, err := d.newPreparedModuleSlotWithPrepareTimeout(ctx, false, PythonReactorPurposeStartup, 0)
			if err != nil {
				_ = d.closeRuntime(context.Background())
				return err
			}
			d.prepared <- slot
		}
	case "fresh":
		// Probe the exact artifact and trusted script before reporting readiness.
		slot, err := d.newPreparedModuleSlotWithPrepareTimeout(ctx, false, PythonReactorPurposeStartup, 0)
		if err != nil {
			_ = d.closeRuntime(context.Background())
			return err
		}
		_ = slot.close(context.Background())
	}

	d.started = true
	d.log.Info("python-reactor dispatcher ready",
		zap.String("artifact_sha256", artifact.SHA256),
		zap.String("producer_commit", artifact.ProducerCommit),
		zap.String("artifact_profile", artifact.Profile),
		zap.Int("max_instances", d.cfg.MaxInstances),
		zap.Duration("request_timeout", d.cfg.Timeout),
		zap.String("lifecycle", d.cfg.PythonLifecycle),
		zap.String("snapshot_mode", d.snapshotMode()),
		zap.String("reset_mode", d.resetMode()),
	)
	return nil
}

func (d *PythonReactorDispatcher) Send(ctx context.Context, method string, params map[string]any) (map[string]any, error) {
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
				"snapshot_mode":     d.snapshotMode(),
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
	request, err := buildShimmyPythonRunRequest(method, params)
	if err != nil {
		return nil, err
	}

	runContext, cancel := context.WithTimeout(ctx, d.cfg.Timeout)
	defer cancel()

	var slot *pythonReactorModuleSlot
	checkoutStart := time.Now()
	switch d.cfg.PythonLifecycle {
	case "snapshot":
		slot, err = acquirePythonReactorSnapshotSlot(
			runContext,
			d.prepared,
			d.closedCh,
			d.snapshotRefillInFlight,
			func(createContext context.Context) (*pythonReactorModuleSlot, error) {
				return d.newPreparedModuleSlot(createContext, true, PythonReactorPurposeReplacement, requestID)
			},
		)
		if err != nil {
			return nil, err
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
			slot, err = d.newPreparedModuleSlot(runContext, false, PythonReactorPurposeFresh, requestID)
			if err != nil {
				return nil, err
			}
		}
	case "fresh":
		slot, err = d.newPreparedModuleSlot(runContext, false, PythonReactorPurposeFresh, requestID)
		if err != nil {
			return nil, err
		}
	}
	d.observePythonReactorPhase(PythonReactorPhaseObservation{
		Phase: PythonReactorPhaseCheckout, Purpose: PythonReactorPurposeRequest,
		RequestID: requestID, SlotID: slot.id, Started: checkoutStart,
		MemoryBytes: uint64(slot.module.Memory().Size()), SnapshotSelected: slot.snapshotSelected,
		Outcome: PythonReactorOutcomeOK,
	})
	if d.cfg.PythonLifecycle != "snapshot" {
		defer func() {
			phaseStart := time.Now()
			closeErr := slot.close(context.Background())
			d.observePythonReactorPhase(PythonReactorPhaseObservation{
				Phase: PythonReactorPhaseClose, Purpose: PythonReactorPurposeRequest,
				RequestID: requestID, SlotID: slot.id, Started: phaseStart,
				Outcome: pythonReactorPhaseOutcome(closeErr), Err: closeErr,
			})
		}()
	}

	phaseStart := time.Now()
	payload, callErr := callPythonReactorExecute(runContext, slot.module, d.artifact.ExecuteExport, request)
	if callErr != nil && runContext.Err() != nil {
		callErr = errors.Join(callErr, runContext.Err())
	}
	d.observePythonReactorPhase(PythonReactorPhaseObservation{
		Phase: PythonReactorPhaseExecute, Purpose: PythonReactorPurposeRequest,
		RequestID: requestID, SlotID: slot.id, Started: phaseStart,
		MemoryBytes: uint64(slot.module.Memory().Size()), SnapshotSelected: slot.snapshotSelected,
		Outcome: pythonReactorPhaseOutcome(callErr), Err: callErr,
	})

	if d.cfg.PythonLifecycle == "snapshot" {
		var restoreErr error
		if callErr == nil {
			phaseStart = time.Now()
			restoreErr = restorePythonReactorSnapshot(slot)
			d.observePythonReactorPhase(PythonReactorPhaseObservation{
				Phase: PythonReactorPhaseRestore, Purpose: PythonReactorPurposeRequest,
				RequestID: requestID, SlotID: slot.id, Started: phaseStart,
				MemoryBytes: uint64(slot.module.Memory().Size()), SnapshotSelected: slot.snapshotSelected,
				Outcome: pythonReactorPhaseOutcome(restoreErr), Err: restoreErr,
			})
		}
		if callErr != nil || restoreErr != nil {
			diagnostic := slot.diagnostic.String()
			d.discardSnapshotSlotAsync(slot, requestID)
			d.scheduleSnapshotRefill(requestID)
			return nil, withPythonReactorDiagnostic(errors.Join(callErr, restoreErr), diagnostic)
		}
		slot.diagnostic.Reset()
		d.prepared <- slot
	}
	if callErr != nil {
		return nil, withPythonReactorDiagnostic(callErr, slot.diagnostic.String())
	}
	phaseStart = time.Now()
	result, err := decodeShimmyPythonResponse(payload)
	d.observePythonReactorPhase(PythonReactorPhaseObservation{
		Phase: PythonReactorPhaseDecode, Purpose: PythonReactorPurposeRequest,
		RequestID: requestID, SlotID: slot.id, Started: phaseStart,
		SnapshotSelected: slot.snapshotSelected,
		Outcome:          pythonReactorPhaseOutcome(err), Err: err,
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"command": method, "result": result}, nil
}

func acquirePythonReactorSnapshotSlot(
	ctx context.Context,
	prepared <-chan *pythonReactorModuleSlot,
	closed <-chan struct{},
	refillInFlight func() bool,
	create func(context.Context) (*pythonReactorModuleSlot, error),
) (*pythonReactorModuleSlot, error) {
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
	if refillInFlight != nil && refillInFlight() {
		select {
		case slot := <-prepared:
			if slot != nil {
				return slot, nil
			}
		case <-closed:
			return nil, errors.New("python-reactor: dispatcher is shut down")
		case <-ctx.Done():
			return nil, fmt.Errorf("python-reactor: wait for snapshot refill: %w", ctx.Err())
		}
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

func (d *PythonReactorDispatcher) resetMode() string {
	switch d.cfg.PythonLifecycle {
	case "snapshot":
		return "linear-memory-" + d.snapshotSelected
	case "single-use":
		return "single-use-prepared"
	default:
		return "fresh-instance"
	}
}

func (d *PythonReactorDispatcher) snapshotMode() string {
	if d.cfg.PythonLifecycle == "snapshot" {
		return "memcpy"
	}
	return ""
}

func (d *PythonReactorDispatcher) tryBeginSend() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.started || d.closed {
		return false
	}
	d.pending.Add(1)
	return true
}

func (d *PythonReactorDispatcher) newInitializedModule(
	ctx context.Context,
	purpose PythonReactorPurpose,
	requestID uint64,
	slotID uint64,
) (api.Module, *pythonReactorDiagnosticBuffer, error) {
	diagnostic := &pythonReactorDiagnosticBuffer{}
	phaseStart := time.Now()
	module, err := d.runtime.InstantiateModule(
		ctx,
		d.compiled,
		wazero.NewModuleConfig().WithName("").WithRandSource(cryptorand.Reader).WithStderr(diagnostic),
	)
	memoryBytes := uint64(0)
	if module != nil && module.Memory() != nil {
		memoryBytes = uint64(module.Memory().Size())
	}
	d.observePythonReactorPhase(PythonReactorPhaseObservation{
		Phase: PythonReactorPhaseInstantiate, Purpose: purpose, RequestID: requestID, SlotID: slotID,
		Started: phaseStart, MemoryBytes: memoryBytes, Outcome: pythonReactorPhaseOutcome(err), Err: err,
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
	err = callPythonReactorNoArgs(ctx, module, "_initialize")
	d.observePythonReactorPhase(PythonReactorPhaseObservation{
		Phase: PythonReactorPhaseInitialize, Purpose: purpose, RequestID: requestID, SlotID: slotID,
		Started: phaseStart, MemoryBytes: uint64(module.Memory().Size()), Outcome: pythonReactorPhaseOutcome(err), Err: err,
	})
	if err != nil {
		return nil, diagnostic, err
	}
	phaseStart = time.Now()
	err = callPythonReactorNoArgsValue(ctx, module, "shimmy_python_runtime_identity", 0x53505231)
	if err == nil {
		err = callPythonReactorNoArgsValue(ctx, module, d.artifact.InitExport, 0)
	}
	d.observePythonReactorPhase(PythonReactorPhaseObservation{
		Phase: PythonReactorPhaseRuntimeInit, Purpose: purpose, RequestID: requestID, SlotID: slotID,
		Started: phaseStart, MemoryBytes: uint64(module.Memory().Size()), Outcome: pythonReactorPhaseOutcome(err), Err: err,
	})
	if err != nil {
		return nil, diagnostic, err
	}
	phaseStart = time.Now()
	err = callPythonReactorStatus(ctx, module, d.artifact.PrepareExport, []byte(d.script))
	d.observePythonReactorPhase(PythonReactorPhaseObservation{
		Phase: PythonReactorPhaseRuntimePrepare, Purpose: purpose, RequestID: requestID, SlotID: slotID,
		Started: phaseStart, MemoryBytes: uint64(module.Memory().Size()), Outcome: pythonReactorPhaseOutcome(err), Err: err,
	})
	if err != nil {
		return nil, diagnostic, err
	}
	diagnostic.Reset()
	failed = false
	return module, diagnostic, nil
}

func reservePythonReactorSnapshotHeadroom(ctx context.Context, module api.Module, bytes uint64) (retErr error) {
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

func (d *PythonReactorDispatcher) newPreparedModuleSlotWithPrepareTimeout(
	ctx context.Context,
	takeSnapshot bool,
	purpose PythonReactorPurpose,
	requestID uint64,
) (*pythonReactorModuleSlot, error) {
	prepareCtx, cancel := context.WithTimeout(ctx, d.cfg.PythonPrepareTimeout)
	defer cancel()
	return d.newPreparedModuleSlot(prepareCtx, takeSnapshot, purpose, requestID)
}

func (d *PythonReactorDispatcher) newPreparedModuleSlot(
	ctx context.Context,
	takeSnapshot bool,
	purpose PythonReactorPurpose,
	requestID uint64,
) (*pythonReactorModuleSlot, error) {
	slotID := d.slotCounter.Add(1)
	module, diagnostic, err := d.newInitializedModule(ctx, purpose, requestID, slotID)
	if err != nil {
		return nil, withPythonReactorDiagnostic(err, diagnostic.String())
	}
	slot := &pythonReactorModuleSlot{
		id:         slotID,
		module:     module,
		diagnostic: diagnostic,
	}
	if !takeSnapshot {
		return slot, nil
	}
	phaseStart := time.Now()
	err = reservePythonReactorSnapshotHeadroom(ctx, module, d.cfg.PythonSnapshotHeadroomBytes)
	d.observePythonReactorPhase(PythonReactorPhaseObservation{
		Phase: PythonReactorPhaseHeadroom, Purpose: purpose, RequestID: requestID, SlotID: slotID,
		Started: phaseStart, MemoryBytes: uint64(module.Memory().Size()), Outcome: pythonReactorPhaseOutcome(err), Err: err,
	})
	if err != nil {
		_ = slot.close(context.Background())
		return nil, err
	}
	phaseStart = time.Now()
	slot.strategy = NewFullMemcpyStrategy()
	slot.snapshotSelected = "memcpy"
	d.observePythonReactorPhase(PythonReactorPhaseObservation{
		Phase: PythonReactorPhaseStrategySelect, Purpose: purpose, RequestID: requestID, SlotID: slotID,
		Started: phaseStart, MemoryBytes: uint64(module.Memory().Size()), SnapshotSelected: slot.snapshotSelected,
		Outcome: PythonReactorOutcomeOK,
	})
	phaseStart = time.Now()
	err = slot.strategy.Take(module.Memory())
	d.observePythonReactorPhase(PythonReactorPhaseObservation{
		Phase: PythonReactorPhaseSnapshotTake, Purpose: purpose, RequestID: requestID, SlotID: slotID,
		Started: phaseStart, MemoryBytes: uint64(module.Memory().Size()), SnapshotSelected: slot.snapshotSelected,
		Outcome: pythonReactorPhaseOutcome(err), Err: err,
	})
	if err != nil {
		_ = slot.close(context.Background())
		return nil, fmt.Errorf("python-reactor: take prepared snapshot: %w", err)
	}
	slot.baselineSize = module.Memory().Size()
	return slot, nil
}

func restorePythonReactorSnapshot(slot *pythonReactorModuleSlot) error {
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

func (d *PythonReactorDispatcher) discardSnapshotSlotAsync(slot *pythonReactorModuleSlot, requestID uint64) {
	// Send already holds one pending count, so this Add cannot race Shutdown's
	// Wait. Closing a context-cancelled wazero module or its snapshot strategy
	// can block and must not extend the request deadline.
	d.pending.Add(1)
	go func() {
		defer d.pending.Done()
		phaseStart := time.Now()
		closeErr := slot.close(context.Background())
		d.observePythonReactorPhase(PythonReactorPhaseObservation{
			Phase: PythonReactorPhaseClose, Purpose: PythonReactorPurposeRequest,
			RequestID: requestID, SlotID: slot.id, Started: phaseStart,
			Outcome: pythonReactorPhaseOutcome(closeErr), Err: closeErr,
		})
	}()
}

func (d *PythonReactorDispatcher) snapshotRefillInFlight() bool {
	d.refillMu.Lock()
	defer d.refillMu.Unlock()
	return d.refillInFlight > 0
}

func (d *PythonReactorDispatcher) scheduleSnapshotRefill(requestID uint64) {
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

		slot, err := d.newPreparedModuleSlotWithPrepareTimeout(refillCtx, true, PythonReactorPurposeReplacement, requestID)
		if err != nil {
			if refillCtx.Err() == nil {
				d.log.Warn("python-reactor snapshot refill failed", zap.Error(err))
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

func (d *PythonReactorDispatcher) scheduleSingleUseRefill(requestID uint64) {
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

		slot, err := d.newPreparedModuleSlotWithPrepareTimeout(refillCtx, false, PythonReactorPurposeRefill, requestID)
		if err != nil {
			if refillCtx.Err() == nil {
				d.log.Warn("python-reactor single-use refill failed", zap.Error(err))
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

func (d *PythonReactorDispatcher) Shutdown(ctx context.Context) error {
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

func (d *PythonReactorDispatcher) closeRuntime(ctx context.Context) error {
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

func callPythonReactorNoArgs(ctx context.Context, module api.Module, name string) error {
	function := module.ExportedFunction(name)
	if function == nil {
		return fmt.Errorf("python-reactor: required export %q is missing", name)
	}
	if _, err := function.Call(ctx); err != nil {
		return fmt.Errorf("python-reactor: call %s: %w", name, err)
	}
	return nil
}

func callPythonReactorNoArgsValue(ctx context.Context, module api.Module, name string, expected uint32) error {
	function := module.ExportedFunction(name)
	if function == nil {
		return fmt.Errorf("python-reactor: required export %q is missing", name)
	}
	results, err := function.Call(ctx)
	if err != nil {
		return fmt.Errorf("python-reactor: call %s: %w", name, err)
	}
	if len(results) != 1 || uint32(results[0]) != expected {
		return fmt.Errorf("python-reactor: %s returned identity/status %v; want %d", name, results, expected)
	}
	return nil
}

func callPythonReactorStatus(ctx context.Context, module api.Module, name string, data []byte) error {
	results, release, err := callPythonReactorWithBytes(ctx, module, name, data)
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

func callPythonReactorExecute(ctx context.Context, module api.Module, name string, request []byte) ([]byte, error) {
	if name == "" {
		name = "execute"
	}
	results, release, err := callPythonReactorWithBytes(ctx, module, name, request)
	if release != nil {
		defer release()
	}
	if err != nil {
		return nil, err
	}
	if len(results) != 1 {
		return nil, errors.New("python-reactor: execute returned an unexpected result count")
	}
	return readPythonReactorResponse(module.Memory(), uint32(results[0]))
}

func callPythonReactorWithBytes(ctx context.Context, module api.Module, name string, data []byte) ([]uint64, func(), error) {
	if len(data) == 0 || len(data) > pythonReactorPayloadMaxBytes || len(data) > math.MaxUint32 {
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
		// A failed guest call is followed by module disposal in every caller.
		// Calling guest dealloc here can itself consume another deadline and
		// delay the timeout response without reclaiming reusable memory.
		return nil, nil, fmt.Errorf("python-reactor: call %s: %w", name, err)
	}
	return results, release, nil
}

func readPythonReactorResponse(memory api.Memory, pointer uint32) ([]byte, error) {
	if memory == nil {
		return nil, errors.New("python-reactor: guest module has no linear memory")
	}
	header, ok := memory.Read(pointer, 4)
	if !ok {
		return nil, errors.New("python-reactor: response length prefix is out of bounds")
	}
	length := binary.LittleEndian.Uint32(header)
	if length > pythonReactorPayloadMaxBytes {
		return nil, fmt.Errorf("python-reactor: response payload length %d exceeds limit %d", length, pythonReactorPayloadMaxBytes)
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

type pythonReactorDiagnosticBuffer struct {
	data []byte
}

func (buffer *pythonReactorDiagnosticBuffer) Write(data []byte) (int, error) {
	length := len(data)
	if length >= pythonReactorDiagnosticMaxBytes {
		buffer.data = append(buffer.data[:0], data[length-pythonReactorDiagnosticMaxBytes:]...)
		return length, nil
	}
	if overflow := len(buffer.data) + length - pythonReactorDiagnosticMaxBytes; overflow > 0 {
		copy(buffer.data, buffer.data[overflow:])
		buffer.data = buffer.data[:len(buffer.data)-overflow]
	}
	buffer.data = append(buffer.data, data...)
	return length, nil
}

func (buffer *pythonReactorDiagnosticBuffer) String() string { return string(buffer.data) }
func (buffer *pythonReactorDiagnosticBuffer) Reset()         { buffer.data = buffer.data[:0] }

func withPythonReactorDiagnostic(base error, diagnostic string) error {
	if diagnostic == "" {
		return base
	}
	return fmt.Errorf("%w; guest stderr: %s", base, diagnostic)
}
