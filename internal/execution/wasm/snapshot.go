package wasm

import (
	"fmt"

	"github.com/tetratelabs/wazero/api"
)

// SnapshotStrategy abstracts how linear-memory snapshots are taken and
// restored. The default implementation (FullMemcpyStrategy) copies the entire
// memory region on every restore. Future strategies may track dirty pages to
// reduce restore cost for large modules.
//
// Contract (I-4 fix — document ordering and concurrency expectations):
//   - Take must be called at least once before Restore.
//   - Take may be called multiple times; each call overwrites the previous
//     snapshot.
//   - Calling Restore without a prior Take is a no-op (returns nil) but
//     logically meaningless.
//   - Implementations are NOT safe for concurrent calls to Take / Restore.
//     The caller (wasmSupervisor) must serialise access.
type SnapshotStrategy interface {
	// Take captures the current state of the WASM linear memory.
	// It is called once after module initialisation.
	Take(mem api.Memory) error

	// Restore writes the captured snapshot back into WASM linear memory.
	// It is called after every request so the next request sees a clean state.
	Restore(mem api.Memory) error

	// Close releases any resources held by the strategy. It is safe to call on a
	// zero-value or never-initialised strategy.
	Close() error
}

// ---------------------------------------------------------------------------
// FullMemcpyStrategy
// ---------------------------------------------------------------------------

// FullMemcpyStrategy is the always-available baseline: it copies the entire
// linear memory into a []byte on Take and writes it all back on Restore.
// Cost is O(total memory size) regardless of how many pages were actually
// written during the request.
type FullMemcpyStrategy struct {
	snapshot []byte
}

// NewFullMemcpyStrategy returns a ready-to-use FullMemcpyStrategy.
func NewFullMemcpyStrategy() *FullMemcpyStrategy {
	return &FullMemcpyStrategy{}
}

// Take implements SnapshotStrategy.
func (f *FullMemcpyStrategy) Take(mem api.Memory) error {
	if mem == nil {
		f.snapshot = nil
		return nil
	}

	size := mem.Size()
	if size == 0 {
		f.snapshot = nil
		return nil
	}

	buf, ok := mem.Read(0, size)
	if !ok {
		return fmt.Errorf("snapshot: could not read %d bytes of linear memory", size)
	}

	// Make an owned copy — mem.Read may return a slice backed by the wazero
	// memory buffer which could be modified by subsequent guest execution.
	f.snapshot = make([]byte, len(buf))
	copy(f.snapshot, buf)

	return nil
}

// Restore implements SnapshotStrategy.
func (f *FullMemcpyStrategy) Restore(mem api.Memory) error {
	if f.snapshot == nil || mem == nil {
		return nil
	}

	if !mem.Write(0, f.snapshot) {
		return fmt.Errorf("snapshot: failed to restore %d bytes", len(f.snapshot))
	}

	return nil
}

// Close implements SnapshotStrategy. FullMemcpyStrategy holds no OS resources.
func (f *FullMemcpyStrategy) Close() error {
	f.snapshot = nil
	return nil
}
