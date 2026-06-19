package wasm

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"go.uber.org/zap"
)

// ErrMemoryGrew indicates that the guest expanded linear memory during a
// request beyond the size captured at snapshot time. wazero (and the WASM
// spec) does not allow shrinking linear memory, so the original snapshotted
// state cannot be fully reproduced and the supervisor must be discarded.
var ErrMemoryGrew = errors.New("wasm: linear memory grew beyond snapshotted size")

// wasmSupervisor manages a single instantiated WASM module. After the module
// is initialised its linear memory is snapshotted; the snapshot is restored
// after every Send so that the next request sees a clean initial state. This
// gives cheap warm-start semantics without re-compiling the module.
type wasmSupervisor struct {
	mu sync.Mutex

	runtime  wazero.Runtime
	compiled wazero.CompiledModule
	modCfg   wazero.ModuleConfig

	mod     api.Module
	adapter *wasmAdapter

	// strategy implements snapshot/restore. The generic backend intentionally
	// uses the portable full-memory copy strategy; dirty-page optimisation is a
	// separate future concern.
	strategy SnapshotStrategy

	// healthy is true when the supervisor is in a known-good state and can be
	// safely returned to the pool. It is set to false when restoreSnapshot fails,
	// indicating the WASM module's memory state is undefined.
	healthy bool

	// snapshotSize is the linear-memory size (in bytes) captured at Take time.
	// restoreSnapshot compares this against the post-request memory size to
	// detect memory.grow during execution — wazero cannot shrink memory, so
	// any growth invalidates the snapshot and must mark the supervisor unhealthy.
	snapshotSize uint32

	timeout time.Duration
	log     *zap.Logger
}

func newWasmSupervisor(
	rt wazero.Runtime,
	compiled wazero.CompiledModule,
	modCfg wazero.ModuleConfig,
	timeout time.Duration,
	log *zap.Logger,
) *wasmSupervisor {
	return &wasmSupervisor{
		runtime:  rt,
		compiled: compiled,
		modCfg:   modCfg,
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

	// Apply start functions on top of the provided (sandboxed) module config.
	instCfg := s.modCfg.WithStartFunctions("_initialize", "_start")

	mod, err := s.runtime.InstantiateModule(ctx, s.compiled, instCfg)
	if err != nil {
		return fmt.Errorf("wasm: instantiate module: %w", err)
	}

	s.mod = mod
	s.adapter = newWasmAdapter(mod, s.log)
	s.healthy = true

	// Snapshot linear memory so we can restore it before each request.
	s.strategy = NewFullMemcpyStrategy()
	if err := s.takeSnapshot(); err != nil {
		_ = s.strategy.Close()
		_ = mod.Close(ctx)
		s.mod = nil
		return fmt.Errorf("wasm: snapshot memory: %w", err)
	}

	memSize := uint32(0)
	if m := s.mod.Memory(); m != nil {
		memSize = m.Size()
	}
	s.log.Debug("wasm module ready",
		zap.Uint32("snapshot_bytes", memSize),
		zap.String("strategy", fmt.Sprintf("%T", s.strategy)),
	)

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

	// Restore memory snapshot to keep state clean for the next request.
	// If restore fails, mark the supervisor unhealthy so the dispatcher
	// discards it rather than returning it to the pool with undefined state.
	if restoreErr := s.restoreSnapshot(); restoreErr != nil {
		s.log.Error("failed to restore memory snapshot — marking supervisor unhealthy", zap.Error(restoreErr))
		s.healthy = false
		if err == nil {
			err = fmt.Errorf("wasm: restore snapshot: %w", restoreErr)
		}
	}

	return result, err
}

// IsHealthy reports whether the supervisor is in a known-good state.
// Safe to call without holding s.mu (acquires the lock internally). (I-3 fix)
func (s *wasmSupervisor) IsHealthy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.healthy
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

	if err := s.strategy.Close(); err != nil {
		s.log.Warn("failed to close snapshot strategy", zap.Error(err))
	}

	return nil
}

// takeSnapshot captures the guest's linear memory via the active strategy and
// records the memory size so restoreSnapshot can detect post-snapshot growth.
// Must be called with s.mu held.
func (s *wasmSupervisor) takeSnapshot() error {
	mem := s.mod.Memory()
	if mem == nil {
		s.snapshotSize = 0
		return nil
	}
	if err := s.strategy.Take(mem); err != nil {
		return err
	}
	s.snapshotSize = mem.Size()
	return nil
}

// restoreSnapshot restores the guest's linear memory from the last snapshot
// via the active strategy. If the guest grew memory during the request
// (memory.grow), it zero-fills the tail beyond the snapshotted size to prevent
// leaking guest data into the next request and returns ErrMemoryGrew so the
// caller (Send) marks the supervisor unhealthy and discards it. Must be called
// with s.mu held.
func (s *wasmSupervisor) restoreSnapshot() error {
	if s.mod == nil {
		return nil
	}
	mem := s.mod.Memory()
	if mem == nil {
		return nil
	}
	if err := s.strategy.Restore(mem); err != nil {
		return err
	}
	if cur := mem.Size(); cur > s.snapshotSize {
		tail := cur - s.snapshotSize
		zeros := make([]byte, tail)
		if !mem.Write(s.snapshotSize, zeros) {
			return fmt.Errorf("wasm: memory grew by %d bytes; zero-fill failed: %w", tail, ErrMemoryGrew)
		}
		return fmt.Errorf("wasm: memory grew by %d bytes (tail zero-filled): %w", tail, ErrMemoryGrew)
	}
	return nil
}
