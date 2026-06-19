package wasm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tetratelabs/wazero"
	"go.uber.org/zap"
)

// echoModulePath returns the absolute path to the pre-compiled echo.wasm test
// fixture. The fixture is a minimal guest module that always returns
// {"ok":true} regardless of the request, which lets us test the host-side Go
// code (alloc call, memory write, evaluate call, length-prefix parsing, JSON
// unmarshal) without implementing a full language runtime in WAT.
func echoModulePath(t *testing.T) string {
	t.Helper()
	// __file__ is not available in Go, but runtime.Caller gives us the source
	// file path so we can derive testdata/ relative to the test file.
	_, filename, _, ok := runtime.Caller(0)
	require.True(t, ok, "runtime.Caller failed")
	return filepath.Join(filepath.Dir(filename), "testdata", "echo.wasm")
}

// newTestLogger returns a no-op zap logger suitable for unit tests.
func newTestLogger(t *testing.T) *zap.Logger {
	t.Helper()
	log, err := zap.NewDevelopment()
	require.NoError(t, err)
	return log
}

// newEchoDispatcher creates a Dispatcher backed by the echo fixture and starts
// it. The caller is responsible for calling Shutdown.
func newEchoDispatcher(t *testing.T, maxInstances int) *Dispatcher {
	t.Helper()
	cfg := Config{
		ModulePath:   echoModulePath(t),
		MaxInstances: maxInstances,
		Timeout:      5 * time.Second,
	}
	d := NewDispatcher(cfg, newTestLogger(t))
	require.NoError(t, d.Start(context.Background()), "dispatcher start")
	return d
}

// TestDispatcher_StartStop verifies that a Dispatcher can be started and shut
// down cleanly without any interaction in between.
func TestDispatcher_StartStop(t *testing.T) {
	d := newEchoDispatcher(t, 1)
	err := d.Shutdown(context.Background())
	assert.NoError(t, err)
}

// TestDispatcher_StartStop_MultipleInstances verifies start/stop with the
// default pool size (NumCPU).
func TestDispatcher_StartStop_MultipleInstances(t *testing.T) {
	d := newEchoDispatcher(t, runtime.NumCPU())
	err := d.Shutdown(context.Background())
	assert.NoError(t, err)
}

// TestDispatcher_Send_BasicResponse sends a single request and checks that the
// echo module returns {"ok":true}.
func TestDispatcher_Send_BasicResponse(t *testing.T) {
	d := newEchoDispatcher(t, 1)
	t.Cleanup(func() { _ = d.Shutdown(context.Background()) })

	result, err := d.Send(context.Background(), "test", map[string]any{"hello": "world"})
	require.NoError(t, err)
	require.NotNil(t, result)

	ok, exists := result["ok"]
	assert.True(t, exists, "response should contain 'ok' key")
	assert.Equal(t, true, ok, "response 'ok' should be true")
}

// TestDispatcher_Send_EmptyParams verifies that Send works with nil params.
func TestDispatcher_Send_EmptyParams(t *testing.T) {
	d := newEchoDispatcher(t, 1)
	t.Cleanup(func() { _ = d.Shutdown(context.Background()) })

	result, err := d.Send(context.Background(), "noop", nil)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, true, result["ok"])
}

// TestDispatcher_Send_Concurrent sends 10 concurrent requests using a pool of
// 3 instances and verifies that all succeed.
func TestDispatcher_Send_Concurrent(t *testing.T) {
	const (
		numWorkers  = 10
		numRequests = 20
		poolSize    = 3
	)

	d := newEchoDispatcher(t, poolSize)
	t.Cleanup(func() { _ = d.Shutdown(context.Background()) })

	type result struct {
		res map[string]any
		err error
	}

	results := make([]result, numRequests)
	var wg sync.WaitGroup
	wg.Add(numRequests)

	sem := make(chan struct{}, numWorkers)
	for i := range numRequests {
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			res, err := d.Send(context.Background(), "eval", map[string]any{"i": i})
			results[i] = result{res, err}
		}(i)
	}

	wg.Wait()

	for i, r := range results {
		require.NoError(t, r.err, "request %d failed", i)
		require.NotNil(t, r.res, "request %d returned nil result", i)
		assert.Equal(t, true, r.res["ok"], "request %d: unexpected result", i)
	}
}

// TestDispatcher_Send_AfterShutdown checks that Send after Shutdown returns
// ErrDispatcherClosed immediately, rather than blocking on the drained pool
// until the caller's context expires.
func TestDispatcher_Send_AfterShutdown(t *testing.T) {
	d := newEchoDispatcher(t, 1)
	require.NoError(t, d.Shutdown(context.Background()))

	_, err := d.Send(context.Background(), "test", nil)
	assert.ErrorIs(t, err, ErrDispatcherClosed, "Send after Shutdown must return ErrDispatcherClosed")
}

// TestDispatcher_Shutdown_Idempotent verifies that calling Shutdown twice does
// not return an error or double-close the runtime.
func TestDispatcher_Shutdown_Idempotent(t *testing.T) {
	d := newEchoDispatcher(t, 1)
	require.NoError(t, d.Shutdown(context.Background()))
	require.NoError(t, d.Shutdown(context.Background()), "second Shutdown must be a no-op")
}

// TestDispatcher_ReplacementDuringShutdown exercises the race where Send has
// just discarded an unhealthy supervisor and scheduled a replacement spawn
// while Shutdown begins. The replacement spawn must NOT insert a supervisor
// into a drained pool, and Shutdown must wait for the spawn goroutine to
// finish before closing the runtime (otherwise the late supervisor would
// reference a torn-down wazero.Runtime).
func TestDispatcher_ReplacementDuringShutdown(t *testing.T) {
	d := newEchoDispatcher(t, 1)

	// Consume the only supervisor in the pool to mimic an in-flight Send.
	sv := <-d.pool

	// Simulate Send's unhealthy-path bookkeeping: schedule the discard close
	// of the bad supervisor and the spawn of a replacement.
	d.discardAsync(sv)
	d.spawnReplacementAsync()

	// Shutdown races with the spawn. It must wait for pending background work
	// (via d.pending.Wait) before draining the pool and closing the runtime.
	require.NoError(t, d.Shutdown(context.Background()))

	// After Shutdown the pool must be empty: any replacement that finished
	// initialising during the race window was closed by spawnOne's
	// closed-guard rather than inserted.
	assert.Equal(t, 0, len(d.pool), "drained pool must be empty after Shutdown")

	// Send after Shutdown returns ErrDispatcherClosed promptly.
	_, err := d.Send(context.Background(), "test", nil)
	assert.ErrorIs(t, err, ErrDispatcherClosed)
}

// TestDispatcher_Shutdown_WaitsForInFlightSends drives the original race the
// lifecycle patch is meant to fix: many concurrent Sends are issued while
// Shutdown runs partway through. Without the in-flight tracking, Shutdown
// could close the wazero runtime out from under a live Send (use-after-close),
// or returnOrDiscard/discardAsync could call pending.Add after Shutdown's
// pending.Wait already returned. Both surfaces are caught by -race or by an
// outright panic.
//
// Acceptance: every Send either succeeds or returns ErrDispatcherClosed, never
// any other error; Shutdown returns nil; no panic.
func TestDispatcher_Shutdown_WaitsForInFlightSends(t *testing.T) {
	d := newEchoDispatcher(t, runtime.NumCPU())

	const numWorkers = 128
	var (
		wg          sync.WaitGroup
		successes   atomic.Int64
		closedExits atomic.Int64
		unexpected  atomic.Int64
	)
	wg.Add(numWorkers)

	start := make(chan struct{})
	for i := 0; i < numWorkers; i++ {
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < 5; j++ {
				_, err := d.Send(context.Background(), "eval", map[string]any{"j": j})
				switch {
				case err == nil:
					successes.Add(1)
				case errors.Is(err, ErrDispatcherClosed):
					closedExits.Add(1)
					return // dispatcher is gone; stop hammering
				default:
					unexpected.Add(1)
					t.Errorf("unexpected error: %v", err)
					return
				}
			}
		}()
	}

	close(start)
	// Give some Sends a chance to begin.
	time.Sleep(5 * time.Millisecond)

	require.NoError(t, d.Shutdown(context.Background()))
	wg.Wait()

	assert.Zero(t, unexpected.Load(), "no Send may return a non-closed error")
	// Post-shutdown Send must return ErrDispatcherClosed promptly (not block).
	postCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := d.Send(postCtx, "eval", nil)
	assert.ErrorIs(t, err, ErrDispatcherClosed)
	t.Logf("successes=%d closed_exits=%d", successes.Load(), closedExits.Load())
}

// TestDispatcher_Shutdown_UnblocksBlockedSend covers the second race called
// out in the patch: a Send that passed tryBeginSend but finds the pool empty
// (all supervisors are in-use or have been drained by a racing Shutdown).
// Without selecting on closedCh, the Send would block on the empty pool until
// the caller's context expired. With the patch it must return
// ErrDispatcherClosed as soon as Shutdown begins.
func TestDispatcher_Shutdown_UnblocksBlockedSend(t *testing.T) {
	d := newEchoDispatcher(t, 1)
	// Empty the pool so a Send is forced to block on acquire.
	sv := <-d.pool

	type sendResult struct {
		err error
	}
	res := make(chan sendResult, 1)
	go func() {
		_, err := d.Send(context.Background(), "eval", nil)
		res <- sendResult{err: err}
	}()

	// Let Send reach the empty-pool select.
	time.Sleep(50 * time.Millisecond)

	// Put sv back so the dispatcher's drain has something to clean up
	// (otherwise Shutdown sees an empty pool, which is also fine).
	d.pool <- sv

	require.NoError(t, d.Shutdown(context.Background()))

	select {
	case r := <-res:
		// Either the Send got the supervisor before Shutdown drained it
		// (succeeded), or Shutdown's closedCh fired first.
		if r.err != nil {
			assert.ErrorIs(t, r.err, ErrDispatcherClosed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Send did not return after Shutdown — closedCh select missing")
	}
}

// TestDispatcher_SpawnReplacementAsync_NoopAfterShutdown asserts that calling
// spawnReplacementAsync on a closed dispatcher is a no-op: it must not
// increment pending and must not launch a goroutine that touches the closed
// runtime.
func TestDispatcher_SpawnReplacementAsync_NoopAfterShutdown(t *testing.T) {
	d := newEchoDispatcher(t, 1)
	require.NoError(t, d.Shutdown(context.Background()))

	// Should return immediately without scheduling work.
	d.spawnReplacementAsync()

	// Wait briefly with a deadline — pending.Wait would block forever if the
	// no-op guard regressed and a goroutine were leaked with a stale runtime.
	done := make(chan struct{})
	go func() {
		d.pending.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pending.Wait did not return — spawn goroutine leaked after Shutdown")
	}
}

// TestDispatcher_MissingModule checks that Start fails when ModulePath does
// not point to a valid file.
func TestDispatcher_MissingModule(t *testing.T) {
	cfg := Config{
		ModulePath:   "/nonexistent/path/module.wasm",
		MaxInstances: 1,
	}
	d := NewDispatcher(cfg, newTestLogger(t))
	err := d.Start(context.Background())
	assert.Error(t, err, "Start with missing module should fail")
}

// TestSupervisor_MemoryRestored sends two sequential requests through the same
// supervisor and verifies that both succeed with the same response. This
// exercises the snapshot/restore cycle: after the first evaluate the bump
// allocator's heap_top is advanced, but restoreSnapshot rewinds memory so the
// second call starts from the exact same state.
func TestSupervisor_MemoryRestored(t *testing.T) {
	// Use a pool of exactly 1 so both sends use the same supervisor instance.
	d := newEchoDispatcher(t, 1)
	t.Cleanup(func() { _ = d.Shutdown(context.Background()) })

	ctx := context.Background()

	r1, err := d.Send(ctx, "first", map[string]any{"seq": 1})
	require.NoError(t, err)
	require.NotNil(t, r1)

	r2, err := d.Send(ctx, "second", map[string]any{"seq": 2})
	require.NoError(t, err)
	require.NotNil(t, r2)

	// Both responses must be identical {"ok":true}.
	assert.Equal(t, r1, r2, "responses must be equal, proving memory was restored between calls")
	assert.Equal(t, true, r1["ok"])
	assert.Equal(t, true, r2["ok"])
}

// TestSupervisor_MemoryRestored_ManyTimes exercises many sequential calls
// through a single-instance pool to ensure the snapshot/restore cycle is
// stable over repeated invocations.
func TestSupervisor_MemoryRestored_ManyTimes(t *testing.T) {
	d := newEchoDispatcher(t, 1)
	t.Cleanup(func() { _ = d.Shutdown(context.Background()) })

	ctx := context.Background()
	const iters = 50

	for i := range iters {
		res, err := d.Send(ctx, "loop", map[string]any{"i": i})
		require.NoError(t, err, "iteration %d", i)
		assert.Equal(t, true, res["ok"], "iteration %d", i)
	}
}

// buildMissingImportModule constructs a valid WASM module that imports a host
// function Shimmy does not provide. Compilation succeeds, but instantiation
// fails inside wasmSupervisor.Start.
func buildMissingImportModule() []byte {
	section := func(id byte, payload []byte) []byte {
		out := []byte{id}
		out = append(out, leb128Encode(uint32(len(payload)))...)
		out = append(out, payload...)
		return out
	}
	name := func(s string) []byte {
		out := leb128Encode(uint32(len(s)))
		out = append(out, []byte(s)...)
		return out
	}

	module := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
	// Type section: one function type () -> ().
	module = append(module, section(1, []byte{0x01, 0x60, 0x00, 0x00})...)
	// Import section: one function import env.missing with type index 0.
	importPayload := []byte{0x01}
	importPayload = append(importPayload, name("env")...)
	importPayload = append(importPayload, name("missing")...)
	importPayload = append(importPayload, 0x00, 0x00) // kind=func, typeidx=0
	module = append(module, section(2, importPayload)...)
	return module
}

// TestDispatcher_StartFailure_DoesNotBlock verifies that a startup failure while
// initialising the warm instance pool returns an error instead of blocking while
// trying to drain a not-yet-full pool.
func TestDispatcher_StartFailure_DoesNotBlock(t *testing.T) {
	modulePath := filepath.Join(t.TempDir(), "missing-import.wasm")
	require.NoError(t, os.WriteFile(modulePath, buildMissingImportModule(), 0o644))

	d := NewDispatcher(Config{
		ModulePath:   modulePath,
		MaxInstances: 2,
		Timeout:      5 * time.Second,
	}, newTestLogger(t))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := d.Start(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "start instance")
}

// TestSupervisor_Start_Idempotent verifies that calling Start twice on the
// same supervisor does not error (the second call is a no-op).
func TestSupervisor_Start_Idempotent(t *testing.T) {
	ctx := context.Background()
	log := newTestLogger(t)

	wasmBytes := echoWasmBytes(t)

	rt, compiled := compileEchoModule(t, ctx, wasmBytes)
	t.Cleanup(func() { _ = rt.Close(ctx) })

	sv := newWasmSupervisor(rt, compiled, wazero.NewModuleConfig().WithName(""), 5*time.Second, log)
	require.NoError(t, sv.Start(ctx))
	require.NoError(t, sv.Start(ctx), "second Start must be a no-op")
	require.NoError(t, sv.Shutdown(ctx))
}

// TestSupervisor_Send_NotStarted checks that Send before Start returns an
// error.
func TestSupervisor_Send_NotStarted(t *testing.T) {
	ctx := context.Background()
	log := newTestLogger(t)

	wasmBytes := echoWasmBytes(t)
	rt, compiled := compileEchoModule(t, ctx, wasmBytes)
	t.Cleanup(func() { _ = rt.Close(ctx) })

	sv := newWasmSupervisor(rt, compiled, wazero.NewModuleConfig().WithName(""), 5*time.Second, log)
	// Do NOT call sv.Start.

	_, err := sv.Send(ctx, "test", nil)
	assert.Error(t, err, "Send without Start should return an error")
}

// TestSupervisor_Send_MemoryGrowDetected is the regression test for
// memory.grow snapshot isolation: if the guest expands linear memory during a
// request, the supervisor must (a) detect the growth, (b) zero the grown tail
// so the next request cannot read leaked guest data, (c) surface
// ErrMemoryGrew, and (d) mark itself unhealthy so the dispatcher discards it
// instead of returning it to the pool.
//
// The echo fixture itself never grows memory, so we simulate a request that
// did by growing the module's memory from host code (between Start and Send)
// and writing a recognisable poison pattern into the new pages. After Send
// runs, restoreSnapshot observes mem.Size() > snapshotSize and must trip the
// defensive path.
func TestSupervisor_Send_MemoryGrowDetected(t *testing.T) {
	ctx := context.Background()
	log := newTestLogger(t)

	wasmBytes := echoWasmBytes(t)
	rt, compiled := compileEchoModule(t, ctx, wasmBytes)
	t.Cleanup(func() { _ = rt.Close(ctx) })

	sv := newWasmSupervisor(rt, compiled, wazero.NewModuleConfig().WithName(""), 5*time.Second, log)
	require.NoError(t, sv.Start(ctx))
	t.Cleanup(func() { _ = sv.Shutdown(ctx) })

	require.True(t, sv.IsHealthy(), "supervisor should be healthy after Start")

	// Capture the snapshot size, then grow memory by 1 page (64 KiB) and
	// poison the new pages. This simulates a guest that called memory.grow
	// during execution and wrote sensitive data into the new pages.
	mem := sv.mod.Memory()
	require.NotNil(t, mem)
	origSize := mem.Size()
	require.Equal(t, origSize, sv.snapshotSize, "snapshotSize must be recorded at Take time")

	prevPages, ok := mem.Grow(1)
	require.True(t, ok, "memory.Grow must succeed (echo fixture has no max)")
	require.Equal(t, origSize/(64*1024), prevPages)

	grownSize := mem.Size()
	require.Greater(t, grownSize, origSize, "memory must have grown")

	poison := make([]byte, grownSize-origSize)
	for i := range poison {
		poison[i] = 0xAB
	}
	require.True(t, mem.Write(origSize, poison), "poison tail")

	// Issue a request. The echo guest doesn't itself grow memory, but Send's
	// post-call restoreSnapshot will observe the host-injected growth and
	// trip the defensive path.
	_, err := sv.Send(ctx, "test", map[string]any{"hello": "world"})
	require.Error(t, err, "Send must return the restore error")
	assert.ErrorIs(t, err, ErrMemoryGrew, "error must wrap ErrMemoryGrew")

	assert.False(t, sv.IsHealthy(), "supervisor must be marked unhealthy after grow detected")

	// The grown tail must have been zeroed so no leftover guest data remains
	// in the (now-unhealthy but still-instantiated) module.
	tail, readOK := mem.Read(origSize, grownSize-origSize)
	require.True(t, readOK)
	expected := make([]byte, grownSize-origSize)
	assert.Equal(t, expected, []byte(tail), "tail must be zero-filled, not contain poison bytes")
}
