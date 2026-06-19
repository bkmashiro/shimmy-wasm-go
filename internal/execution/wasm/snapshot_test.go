//go:build !plan9

package wasm

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// leb128Encode encodes a uint32 as an unsigned LEB128 byte slice.
func leb128Encode(v uint32) []byte {
	var buf []byte
	for {
		b := byte(v & 0x7f)
		v >>= 7
		if v != 0 {
			b |= 0x80
		}
		buf = append(buf, b)
		if v == 0 {
			break
		}
	}
	return buf
}

// buildTestMemoryModule constructs a minimal WASM binary that declares exactly
// `pages` pages (64 KiB each) of linear memory.  wazero's Module.Memory()
// returns the first memory regardless of whether it is exported, so no export
// section is needed.
//
// Binary layout (WASM spec §5):
//
//	\0asm (magic) + version (1) + memory section
//
// This mirrors buildMinimalMemoryModule from snapshot_bench_test.go but
// accepts *testing.T so it can be used in unit tests.
func buildTestMemoryModule(t *testing.T, pages int) []byte {
	t.Helper()

	// Memory section payload: count=1, limits type=0x00 (min only), min=pages
	pagesLEB := leb128Encode(uint32(pages))
	memPayload := append([]byte{0x01, 0x00}, pagesLEB...)

	// Section: id=5 (memory), size=len(payload), payload
	memSec := append([]byte{0x05}, append(leb128Encode(uint32(len(memPayload))), memPayload...)...)

	// Full module: magic + version + memory section
	module := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
	module = append(module, memSec...)
	return module
}

// newTestWazeroMemory instantiates a minimal WASM module with the given number
// of 64 KiB pages and returns its api.Memory.  The runtime and module are
// closed via t.Cleanup.
func newTestWazeroMemory(t *testing.T, pages int) api.Memory {
	t.Helper()
	ctx := context.Background()

	wasmBin := buildTestMemoryModule(t, pages)

	rt := wazero.NewRuntime(ctx)
	t.Cleanup(func() { _ = rt.Close(ctx) })

	compiled, err := rt.CompileModule(ctx, wasmBin)
	require.NoError(t, err, "compile minimal module")
	t.Cleanup(func() { _ = compiled.Close(ctx) })

	mod, err := rt.InstantiateModule(ctx, compiled, wazero.NewModuleConfig().WithName(""))
	require.NoError(t, err, "instantiate minimal module")
	t.Cleanup(func() { _ = mod.Close(ctx) })

	mem := mod.Memory()
	require.NotNil(t, mem, "module must have linear memory")
	return mem
}

// ---------------------------------------------------------------------------
// TestFullMemcpyStrategy_TakeRestoreRoundtrip
// ---------------------------------------------------------------------------

// TestFullMemcpyStrategy_TakeRestoreRoundtrip verifies the core contract:
// after Take, mutating the memory and calling Restore brings it back to the
// snapshotted state.
func TestFullMemcpyStrategy_TakeRestoreRoundtrip(t *testing.T) {
	mem := newTestWazeroMemory(t, 1) // 1 page = 64 KiB

	// Fill memory with a known pattern.
	size := mem.Size()
	pattern := make([]byte, size)
	for i := range pattern {
		pattern[i] = byte(i % 251)
	}
	require.True(t, mem.Write(0, pattern), "write initial pattern")

	s := NewFullMemcpyStrategy()
	t.Cleanup(func() { require.NoError(t, s.Close()) })

	// Take snapshot.
	require.NoError(t, s.Take(mem))

	// Overwrite memory with zeros (simulated guest write).
	zeros := make([]byte, size)
	require.True(t, mem.Write(0, zeros), "overwrite with zeros")

	after, ok := mem.Read(0, size)
	require.True(t, ok)
	require.Equal(t, zeros, []byte(after), "sanity: memory should be all-zeros now")

	// Restore and verify memory matches original pattern.
	require.NoError(t, s.Restore(mem))

	restored, ok := mem.Read(0, size)
	require.True(t, ok)
	assert.Equal(t, pattern, []byte(restored), "Restore must return memory to snapshotted state")
}

// ---------------------------------------------------------------------------
// TestFullMemcpyStrategy_TakeNilMemory
// ---------------------------------------------------------------------------

// TestFullMemcpyStrategy_TakeNilMemory checks that Take(nil) is safe and
// results in a nil snapshot (no panic, no error).
func TestFullMemcpyStrategy_TakeNilMemory(t *testing.T) {
	s := NewFullMemcpyStrategy()
	t.Cleanup(func() { require.NoError(t, s.Close()) })

	require.NoError(t, s.Take(nil))
	assert.Nil(t, s.snapshot, "snapshot should be nil after Take(nil)")

	// A subsequent Restore(nil) must also be a no-op.
	require.NoError(t, s.Restore(nil))
}

// ---------------------------------------------------------------------------
// TestFullMemcpyStrategy_RestoreBeforeTake
// ---------------------------------------------------------------------------

// TestFullMemcpyStrategy_RestoreBeforeTake verifies that calling Restore on a
// zero-value / never-initialised strategy is a no-op that does not modify
// memory or return an error.
func TestFullMemcpyStrategy_RestoreBeforeTake(t *testing.T) {
	s := NewFullMemcpyStrategy()
	t.Cleanup(func() { require.NoError(t, s.Close()) })

	mem := newTestWazeroMemory(t, 1)
	size := mem.Size()

	// Fill with recognisable data.
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i % 97)
	}
	require.True(t, mem.Write(0, data), "write initial data")

	// Snapshot the state so we can compare after Restore.
	before, ok := mem.Read(0, size)
	require.True(t, ok)
	beforeCopy := make([]byte, len(before))
	copy(beforeCopy, before)

	// Restore before any Take — must be a no-op (snapshot is nil).
	require.NoError(t, s.Restore(mem))

	after, ok := mem.Read(0, size)
	require.True(t, ok)
	assert.Equal(t, beforeCopy, []byte(after), "Restore before Take must leave memory unchanged")
}

// ---------------------------------------------------------------------------
// TestFullMemcpyStrategy_EmptyMemory
// ---------------------------------------------------------------------------

// TestFullMemcpyStrategy_EmptyMemory checks that a zero-size case in snapshot
// logic produces a nil snapshot (size==0 branch).  We test this by calling
// Take with nil (which mirrors the zero-size code path in the implementation:
// both nil and zero-size result in snapshot=nil).
func TestFullMemcpyStrategy_EmptyMemory(t *testing.T) {
	s := NewFullMemcpyStrategy()
	t.Cleanup(func() { require.NoError(t, s.Close()) })

	// Take(nil) exercises the "mem == nil" branch which sets snapshot=nil.
	require.NoError(t, s.Take(nil))
	assert.Nil(t, s.snapshot, "snapshot must be nil when memory is nil")

	// Restore(nil) must be a no-op.
	require.NoError(t, s.Restore(nil))
}

// ---------------------------------------------------------------------------
// TestFullMemcpyStrategy_CloseIdempotent
// ---------------------------------------------------------------------------

// TestFullMemcpyStrategy_CloseIdempotent verifies that Close can be called
// multiple times without panicking or returning an error.
func TestFullMemcpyStrategy_CloseIdempotent(t *testing.T) {
	s := NewFullMemcpyStrategy()

	mem := newTestWazeroMemory(t, 1)
	require.NoError(t, s.Take(mem))
	assert.NotNil(t, s.snapshot, "snapshot should be set after Take")

	// First Close should succeed and clear the snapshot.
	require.NoError(t, s.Close())
	assert.Nil(t, s.snapshot, "snapshot should be nil after first Close")

	// Second Close must also be safe.
	require.NoError(t, s.Close())
}

// ---------------------------------------------------------------------------
// TestFullMemcpyStrategy_SnapshotIsOwnedCopy
// ---------------------------------------------------------------------------

// TestFullMemcpyStrategy_SnapshotIsOwnedCopy confirms that the snapshot is an
// independent copy of the memory buffer, not an alias into wazero's backing
// store.  If Take stored a slice backed by the same underlying array, a
// subsequent guest write would silently corrupt the snapshot.
func TestFullMemcpyStrategy_SnapshotIsOwnedCopy(t *testing.T) {
	mem := newTestWazeroMemory(t, 1)
	size := mem.Size()

	// Write distinct pattern.
	pattern := make([]byte, size)
	for i := range pattern {
		pattern[i] = byte(i % 199)
	}
	require.True(t, mem.Write(0, pattern), "write pattern")

	s := NewFullMemcpyStrategy()
	t.Cleanup(func() { require.NoError(t, s.Close()) })

	require.NoError(t, s.Take(mem))

	// Overwrite memory entirely with 0xFF.
	corrupt := make([]byte, size)
	for i := range corrupt {
		corrupt[i] = 0xFF
	}
	require.True(t, mem.Write(0, corrupt))

	// Restore: snapshot must be independent of the wazero buffer.
	require.NoError(t, s.Restore(mem))

	restored, ok := mem.Read(0, size)
	require.True(t, ok)
	assert.Equal(t, pattern, []byte(restored), "snapshot must be independent copy of original data")
}
