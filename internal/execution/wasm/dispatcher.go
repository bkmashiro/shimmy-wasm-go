package wasm

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"go.uber.org/zap"

	"github.com/lambda-feedback/shimmy/internal/execution/dispatcher"
)

// ErrDispatcherClosed is returned by Send after the dispatcher has begun (or
// completed) Shutdown. Callers should treat it as a terminal error.
var ErrDispatcherClosed = fmt.Errorf("wasm: dispatcher is shut down")

// Dispatcher implements [dispatcher.Dispatcher] for the WASM execution
// backend. It compiles the .wasm module once at startup, then maintains a pool
// of pre-initialised [wasmSupervisor] instances (one compiled module, N module
// instances). Requests are dispatched by acquiring a supervisor from the pool,
// calling its Send, and returning it to the pool.
type Dispatcher struct {
	cfg      Config
	rt       wazero.Runtime
	compiled wazero.CompiledModule
	modCfg   wazero.ModuleConfig
	pool     chan *wasmSupervisor
	log      *zap.Logger

	// mu protects closed and serialises the closed/push transitions so that a
	// replacement supervisor cannot land in the pool after Shutdown has begun
	// draining it.
	mu     sync.Mutex
	closed bool
	// closedCh is closed atomically with closed=true (under mu) by Shutdown.
	// Send selects on it to (a) unblock a pool acquire that is racing Shutdown
	// and (b) avoid waiting on an empty pool that Shutdown is about to drain.
	closedCh chan struct{}
	// pending tracks BOTH in-flight Sends (Add in tryBeginSend, Done via Send's
	// defer) AND background goroutines spawned during a Send (replacement
	// spawns, discard-shutdowns). Shutdown waits on it before draining the
	// pool / closing the runtime.
	//
	// Invariant: every pending.Add is either (a) made under d.mu after
	// observing !closed, or (b) made by code that is itself holding a pending
	// count (e.g. discardAsync called from inside Send). This keeps Add from
	// racing Shutdown's Wait — if closed is already set, branch (a) skips the
	// Add and falls back to a synchronous close; in branch (b) Shutdown is
	// guaranteed to still be blocked at Wait on the caller's count.
	pending sync.WaitGroup
}

var _ dispatcher.Dispatcher = (*Dispatcher)(nil)

// NewDispatcher creates a new WASM dispatcher. Compilation and pool
// initialisation happen in Start.
func NewDispatcher(cfg Config, log *zap.Logger) *Dispatcher {
	return &Dispatcher{
		cfg:      cfg,
		log:      log.Named("dispatcher_wasm"),
		closedCh: make(chan struct{}),
	}
}

// tryBeginSend atomically checks the closed flag and increments pending. It
// returns false if Shutdown has begun (caller must abort with
// ErrDispatcherClosed); on true the caller MUST call pending.Done exactly
// once when finished. Holding a pending count across the entire Send keeps
// Shutdown's Wait blocked while the Send is mid-flight, which is what lets
// discardAsync inside Send safely Add to pending without racing Wait.
func (d *Dispatcher) tryBeginSend() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return false
	}
	d.pending.Add(1)
	return true
}

// Start reads and compiles the .wasm file, sets up WASI host functions, and
// pre-warms the supervisor pool.
func (d *Dispatcher) Start(ctx context.Context) error {
	// Pick up sandbox overrides from FUNCTION_WASM_* env vars (including
	// FUNCTION_WASM_MODULE as an alternative to FUNCTION_COMMAND), then apply
	// sensible defaults for any fields still at their zero values.
	d.cfg.applyEnv()
	d.cfg.applyDefaults()

	if d.cfg.ModulePath == "" {
		return fmt.Errorf("wasm: ModulePath must be set (FUNCTION_COMMAND or FUNCTION_WASM_MODULE)")
	}

	maxInstances := d.cfg.MaxInstances
	if maxInstances <= 0 {
		maxInstances = runtime.NumCPU()
	}

	d.log.Info("starting wasm dispatcher",
		zap.String("module", d.cfg.ModulePath),
		zap.Int("max_instances", maxInstances),
		zap.Uint32("max_memory_pages", d.cfg.MaxMemoryPages),
		zap.Duration("timeout", d.cfg.Timeout),
	)

	// Read the .wasm bytes from disk.
	wasmBytes, err := os.ReadFile(d.cfg.ModulePath)
	if err != nil {
		return fmt.Errorf("wasm: read module file %q: %w", d.cfg.ModulePath, err)
	}

	// Build the runtime config with memory limit and context-done interruption.
	rtCfg := wazero.NewRuntimeConfig().
		// WithCloseOnContextDone causes wazero to interrupt a running WASM module
		// when the call context is cancelled or times out, preventing goroutine leaks.
		WithCloseOnContextDone(true)
	if d.cfg.MaxMemoryPages > 0 {
		rtCfg = rtCfg.WithMemoryLimitPages(d.cfg.MaxMemoryPages)
	}

	// Wire in on-disk compilation cache when configured.
	if d.cfg.CompileCacheDir != "" {
		cache, err := wazero.NewCompilationCacheWithDir(d.cfg.CompileCacheDir)
		if err != nil {
			d.log.Warn("failed to create wazero compilation cache, continuing without cache",
				zap.String("dir", d.cfg.CompileCacheDir),
				zap.Error(err))
		} else {
			rtCfg = rtCfg.WithCompilationCache(cache)
			d.log.Info("wazero compilation cache enabled", zap.String("dir", d.cfg.CompileCacheDir))
		}
	}

	// Create a single wazero runtime shared by all instances.
	rt := wazero.NewRuntimeWithConfig(ctx, rtCfg)
	d.rt = rt

	// Instantiate WASI host functions. Most evaluation functions will need at
	// least minimal WASI support (e.g. for memory allocation helpers compiled
	// from C/Rust/TinyGo).
	if _, err := wasi_snapshot_preview1.Instantiate(ctx, rt); err != nil {
		_ = rt.Close(ctx)
		return fmt.Errorf("wasm: instantiate wasi: %w", err)
	}

	// Compile the module once; all instances share the compiled code.
	compiled, err := rt.CompileModule(ctx, wasmBytes)
	if err != nil {
		_ = rt.Close(ctx)
		return fmt.Errorf("wasm: compile module: %w", err)
	}
	d.compiled = compiled

	// Build a locked-down ModuleConfig: no filesystem, no env vars, no
	// stdin/stdout/stderr, no args. Only allow nanosleep and wall/mono clocks
	// which the Go runtime needs.
	modCfg := wazero.NewModuleConfig().
		WithName("").
		WithSysNanosleep().
		WithSysWalltime().
		WithSysNanotime()

	// Filesystem: mount allowed paths read-only; no access by default.
	fsCfg := wazero.NewFSConfig()
	for _, p := range d.cfg.AllowedPaths {
		fsCfg = fsCfg.WithReadOnlyDirMount(p, p)
	}
	modCfg = modCfg.WithFSConfig(fsCfg)

	// Env vars: expose only explicitly whitelisted variables.
	for _, key := range d.cfg.AllowedEnv {
		if val, ok := os.LookupEnv(key); ok {
			modCfg = modCfg.WithEnv(key, val)
		}
	}
	d.modCfg = modCfg

	// Build the pool.
	d.pool = make(chan *wasmSupervisor, maxInstances)

	for i := 0; i < maxInstances; i++ {
		sv := newWasmSupervisor(rt, compiled, modCfg, d.cfg.Timeout, d.log)

		if err := sv.Start(ctx); err != nil {
			// Clean up already-started supervisors.
			_ = drainBufferedPool(ctx, d.pool, d.log)
			_ = rt.Close(ctx)
			return fmt.Errorf("wasm: start instance %d: %w", i, err)
		}

		d.pool <- sv
	}

	d.log.Info("wasm dispatcher ready", zap.Int("instances", maxInstances))

	return nil
}

// Send acquires a supervisor from the pool, dispatches the request, and
// returns the supervisor to the pool.
func (d *Dispatcher) Send(
	ctx context.Context,
	method string,
	data map[string]any,
) (map[string]any, error) {
	if !d.tryBeginSend() {
		return nil, ErrDispatcherClosed
	}
	defer d.pending.Done()

	// Acquire a supervisor, honouring the caller's context AND the shutdown
	// signal so we never block forever on a drained pool.
	var sv *wasmSupervisor
	select {
	case sv = <-d.pool:
	case <-d.closedCh:
		return nil, ErrDispatcherClosed
	case <-ctx.Done():
		return nil, fmt.Errorf("wasm: acquire instance: %w", ctx.Err())
	}

	result, err := sv.Send(ctx, method, data)

	// Return the supervisor to the pool only if it is healthy.
	// If the snapshot restore failed inside Send, sv.healthy is false and the
	// supervisor's state is undefined — discard it and spawn a replacement so
	// pool capacity is eventually restored.
	if sv.IsHealthy() {
		d.returnOrDiscard(sv)
	} else {
		d.log.Warn("wasm supervisor unhealthy after request — dropping from pool, spawning replacement")
		d.discardAsync(sv)
		d.spawnReplacementAsync()
	}

	if err != nil {
		return nil, fmt.Errorf("wasm: send: %w", err)
	}

	return result, nil
}

// returnOrDiscard puts a healthy supervisor back in the pool unless Shutdown
// has begun, in which case the supervisor is closed asynchronously so it does
// not leak past a drained pool.
//
// Must be called from a goroutine that already holds a pending count (i.e.
// from inside Send) so that the Add issued by discardAsync is guaranteed to
// happen before Shutdown's pending.Wait can return.
func (d *Dispatcher) returnOrDiscard(sv *wasmSupervisor) {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		d.discardAsync(sv)
		return
	}
	// Push under the lock so it interleaves correctly with Shutdown's
	// closed=true → drainPool sequence: either we push before closed is set
	// (drainPool sees the supervisor) or we discard via the branch above.
	d.pool <- sv
	d.mu.Unlock()
}

// discardAsync closes a discarded supervisor in the background and tracks it
// via the pending WaitGroup so Shutdown can wait for the close to complete
// before tearing down the runtime.
//
// Must be called from a goroutine that already holds a pending count
// (Send, via tryBeginSend). That invariant keeps Shutdown.pending.Wait
// blocked across this Add, eliminating the Add-after-Wait race.
func (d *Dispatcher) discardAsync(sv *wasmSupervisor) {
	d.pending.Add(1)
	go func() {
		defer d.pending.Done()
		_ = sv.Shutdown(context.Background())
	}()
}

// spawnReplacementAsync kicks off spawnOne in a background goroutine, but only
// if the dispatcher is still open. If Shutdown has begun, no replacement is
// scheduled. Tracked via the pending WaitGroup.
func (d *Dispatcher) spawnReplacementAsync() {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return
	}
	d.pending.Add(1)
	d.mu.Unlock()
	go d.spawnOne()
}

// spawnOne initialises a fresh wasmSupervisor and adds it to the pool.
// Called in a goroutine when an unhealthy supervisor is discarded so that
// pool capacity is eventually restored. Failures are logged but not fatal.
//
// If Shutdown begins while Start is running, the freshly initialised
// supervisor is closed immediately rather than inserted into the drained pool.
func (d *Dispatcher) spawnOne() {
	defer d.pending.Done()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	d.log.Info("wasm: initialising replacement supervisor")
	sv := newWasmSupervisor(d.rt, d.compiled, d.modCfg, d.cfg.Timeout, d.log)
	if err := sv.Start(ctx); err != nil {
		d.log.Error("wasm: replacement supervisor init failed", zap.Error(err))
		return
	}

	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		d.log.Info("wasm: replacement supervisor born during shutdown — closing immediately")
		_ = sv.Shutdown(context.Background())
		return
	}
	d.pool <- sv
	d.mu.Unlock()
	d.log.Info("wasm: replacement supervisor ready")
}

// Shutdown closes all module instances and the wazero runtime. Idempotent.
func (d *Dispatcher) Shutdown(ctx context.Context) error {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil
	}
	d.closed = true
	// Close the channel under mu so the closed=true / close(closedCh) pair is
	// atomic with respect to tryBeginSend: any Send that observes !closed has
	// also pending.Add'd before Shutdown can reach pending.Wait.
	close(d.closedCh)
	d.mu.Unlock()

	d.log.Debug("shutting down wasm dispatcher")

	// Wait for in-flight Sends AND any background goroutines (replacement
	// spawns / discard shutdowns) to finish so that no late-created supervisor
	// lands in the pool after the drain below, no module is mid-Close while we
	// close the runtime, and no Send is running against the wazero runtime
	// when we tear it down.
	d.pending.Wait()

	// Non-blocking drain: after pending.Wait, no spawn or returnOrDiscard
	// goroutine will push to the pool, so we just close everything currently
	// buffered. (drainPool's blocking-for-cap-items semantics would deadlock
	// here when spawnOne took the closed-shortcut and never pushed.)
	for {
		select {
		case sv := <-d.pool:
			if err := sv.Shutdown(ctx); err != nil {
				d.log.Warn("error shutting down pooled supervisor", zap.Error(err))
			}
		default:
			goto drained
		}
	}
drained:

	if d.rt != nil {
		if err := d.rt.Close(ctx); err != nil {
			return fmt.Errorf("wasm: close runtime: %w", err)
		}
		d.rt = nil
	}

	return nil
}
