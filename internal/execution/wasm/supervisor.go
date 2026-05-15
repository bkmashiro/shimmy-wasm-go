package wasm

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"go.uber.org/zap"
)

// wasmSupervisor manages a single instantiated WASM module. After the module
// is initialised its linear memory is snapshotted; the snapshot is restored
// after every Send so that the next request sees a clean initial state. This
// gives cheap warm-start semantics without re-compiling the module.
type wasmSupervisor struct {
	mu sync.Mutex

	runtime  wazero.Runtime
	compiled wazero.CompiledModule

	mod     api.Module
	adapter *wasmAdapter

	// snapshot is a copy of the guest's linear memory taken immediately after
	// _initialize/_start has returned.
	snapshot []byte

	timeout time.Duration
	log     *zap.Logger
}

func newWasmSupervisor(
	rt wazero.Runtime,
	compiled wazero.CompiledModule,
	timeout time.Duration,
	log *zap.Logger,
) *wasmSupervisor {
	return &wasmSupervisor{
		runtime:  rt,
		compiled: compiled,
		timeout:  timeout,
		log:      log.Named("supervisor_wasm"),
	}
}

// Start instantiates the compiled module, runs any WASI start function, then
// snapshots linear memory.
func (s *wasmSupervisor) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.mod != nil {
		return nil
	}

	s.log.Debug("instantiating wasm module")

	mod, err := s.runtime.InstantiateModule(ctx, s.compiled, wazero.NewModuleConfig().WithName(""))
	if err != nil {
		return fmt.Errorf("wasm: instantiate module: %w", err)
	}

	s.mod = mod
	s.adapter = newWasmAdapter(mod, s.log)

	// Snapshot linear memory so we can restore it before each request.
	if err := s.takeSnapshot(); err != nil {
		_ = mod.Close(ctx)
		s.mod = nil
		return fmt.Errorf("wasm: snapshot memory: %w", err)
	}

	s.log.Debug("wasm module ready", zap.Int("snapshot_bytes", len(s.snapshot)))

	return nil
}

// Send calls the guest's evaluate function, then restores linear memory from
// the snapshot so the next request starts from a clean state.
func (s *wasmSupervisor) Send(
	ctx context.Context,
	method string,
	data map[string]any,
) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.mod == nil || s.adapter == nil {
		return nil, fmt.Errorf("wasm: supervisor not started")
	}

	result, err := s.adapter.send(ctx, method, data, s.timeout)

	// Always restore memory snapshot, even on error, to keep state clean for
	// the next request.
	if restoreErr := s.restoreSnapshot(); restoreErr != nil {
		s.log.Error("failed to restore memory snapshot", zap.Error(restoreErr))
	}

	return result, err
}

// Shutdown closes the module instance and releases resources.
func (s *wasmSupervisor) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.mod == nil {
		return nil
	}

	s.log.Debug("shutting down wasm module instance")

	if err := s.mod.Close(ctx); err != nil {
		return fmt.Errorf("wasm: close module: %w", err)
	}

	s.mod = nil
	s.adapter = nil
	s.snapshot = nil

	return nil
}

// takeSnapshot copies all of the guest's linear memory into s.snapshot.
// Must be called with s.mu held.
func (s *wasmSupervisor) takeSnapshot() error {
	mem := s.mod.Memory()
	if mem == nil {
		// No linear memory — nothing to snapshot.
		s.snapshot = nil
		return nil
	}

	size := mem.Size()
	if size == 0 {
		s.snapshot = nil
		return nil
	}

	buf, ok := mem.Read(0, size)
	if !ok {
		return fmt.Errorf("wasm: could not read %d bytes of linear memory", size)
	}

	// Make an owned copy — mem.Read may return a slice backed by the wazero
	// memory buffer which could change under us.
	s.snapshot = make([]byte, len(buf))
	copy(s.snapshot, buf)

	return nil
}

// restoreSnapshot writes the snapshot back into guest linear memory.
// Must be called with s.mu held.
func (s *wasmSupervisor) restoreSnapshot() error {
	if s.snapshot == nil || s.mod == nil {
		return nil
	}

	mem := s.mod.Memory()
	if mem == nil {
		return nil
	}

	if !mem.Write(0, s.snapshot) {
		return fmt.Errorf("wasm: failed to restore %d snapshot bytes", len(s.snapshot))
	}

	return nil
}
