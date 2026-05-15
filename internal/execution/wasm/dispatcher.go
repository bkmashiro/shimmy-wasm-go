package wasm

import (
	"context"
	"fmt"
	"os"
	"runtime"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"go.uber.org/zap"

	"github.com/lambda-feedback/shimmy/internal/execution/dispatcher"
)

// Dispatcher implements [dispatcher.Dispatcher] for the WASM execution
// backend. It compiles the .wasm module once at startup, then maintains a pool
// of pre-initialised [wasmSupervisor] instances (one compiled module, N module
// instances). Requests are dispatched by acquiring a supervisor from the pool,
// calling its Send, and returning it to the pool.
type Dispatcher struct {
	cfg     Config
	rt      wazero.Runtime
	pool    chan *wasmSupervisor
	log     *zap.Logger
}

var _ dispatcher.Dispatcher = (*Dispatcher)(nil)

// NewDispatcher creates a new WASM dispatcher. Compilation and pool
// initialisation happen in Start.
func NewDispatcher(cfg Config, log *zap.Logger) *Dispatcher {
	return &Dispatcher{
		cfg: cfg,
		log: log.Named("dispatcher_wasm"),
	}
}

// Start reads and compiles the .wasm file, sets up WASI host functions, and
// pre-warms the supervisor pool.
func (d *Dispatcher) Start(ctx context.Context) error {
	if d.cfg.ModulePath == "" {
		return fmt.Errorf("wasm: ModulePath must be set (FUNCTION_COMMAND)")
	}

	maxInstances := d.cfg.MaxInstances
	if maxInstances <= 0 {
		maxInstances = runtime.NumCPU()
	}

	d.log.Info("starting wasm dispatcher",
		zap.String("module", d.cfg.ModulePath),
		zap.Int("max_instances", maxInstances),
	)

	// Read the .wasm bytes from disk.
	wasmBytes, err := os.ReadFile(d.cfg.ModulePath)
	if err != nil {
		return fmt.Errorf("wasm: read module file %q: %w", d.cfg.ModulePath, err)
	}

	// Create a single wazero runtime shared by all instances.
	rt := wazero.NewRuntime(ctx)
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

	// Build the pool.
	d.pool = make(chan *wasmSupervisor, maxInstances)

	for i := 0; i < maxInstances; i++ {
		sv := newWasmSupervisor(rt, compiled, d.cfg.Timeout, d.log)

		if err := sv.Start(ctx); err != nil {
			// Clean up already-started supervisors.
			d.drainPool(ctx)
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
	// Acquire a supervisor, honouring the caller's context deadline.
	var sv *wasmSupervisor
	select {
	case sv = <-d.pool:
	case <-ctx.Done():
		return nil, fmt.Errorf("wasm: acquire instance: %w", ctx.Err())
	}

	result, err := sv.Send(ctx, method, data)

	// Always return the supervisor to the pool, regardless of errors.
	// Memory is restored inside supervisor.Send, so the instance is clean.
	d.pool <- sv

	if err != nil {
		return nil, fmt.Errorf("wasm: send: %w", err)
	}

	return result, nil
}

// Shutdown closes all module instances and the wazero runtime.
func (d *Dispatcher) Shutdown(ctx context.Context) error {
	d.log.Debug("shutting down wasm dispatcher")

	d.drainPool(ctx)

	if d.rt != nil {
		if err := d.rt.Close(ctx); err != nil {
			return fmt.Errorf("wasm: close runtime: %w", err)
		}
		d.rt = nil
	}

	return nil
}

// drainPool closes all supervisors currently in the pool channel.
func (d *Dispatcher) drainPool(ctx context.Context) {
	if d.pool == nil {
		return
	}

	for {
		select {
		case sv := <-d.pool:
			if err := sv.Shutdown(ctx); err != nil {
				d.log.Error("error shutting down wasm instance", zap.Error(err))
			}
		default:
			return
		}
	}
}
