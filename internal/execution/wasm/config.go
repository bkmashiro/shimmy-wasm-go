package wasm

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds the configuration for the WASM execution backend.
//
// Configuration is read from environment variables via koanf (the same
// mechanism used by the rest of shimmy). The "conf" struct tags map to
// the koanf key names derived from the FUNCTION_* env-var prefix.
type Config struct {
	// ModulePath is the path to the .wasm file to load.
	// Populated from FUNCTION_COMMAND (the command field re-used as the
	// .wasm file path when FUNCTION_INTERFACE=wasm).
	ModulePath string `conf:"cmd"`

	// AgentPythonManifestPath binds the clean Python reactor artifact to its
	// producer manifest. When empty, the agent-python dispatcher reads
	// manifest.json next to ModulePath. FUNCTION_WASM_MANIFEST overrides it.
	AgentPythonManifestPath string `conf:"wasm_manifest"`

	// MaxInstances is the maximum number of concurrently active module
	// instances. When the pool is exhausted requests block until a slot is
	// available. Defaults to runtime.NumCPU() when <= 0.
	// Populated from FUNCTION_MAX_PROCS / max_workers.
	MaxInstances int `conf:"max_workers"`

	// Timeout is the per-request deadline passed to the WASM call.
	// Populated from FUNCTION_WORKER_SEND_TIMEOUT / send.timeout.
	Timeout time.Duration `conf:"timeout"`

	// --- Sandbox limits ---

	// MaxMemoryPages limits WASM linear memory (1 page = 64KB).
	// Default: 256 pages = 16MB. 0 means use module's own max.
	MaxMemoryPages uint32 `conf:"wasm_max_memory_pages"`

	// AllowedPaths is a list of host paths the module may read (read-only).
	// Empty means no filesystem access at all.
	AllowedPaths []string `conf:"wasm_allowed_paths"`

	// AllowedEnv is a list of env var names the module may read.
	// Empty means no env vars exposed.
	AllowedEnv []string `conf:"wasm_allowed_env"`

	// UseUffd enables userfaultfd write-protect dirty-page tracking for memory
	// snapshot/restore. When true, only pages written by the guest since the
	// last snapshot are restored, reducing restore cost for large modules.
	// Requires Linux kernel with UFFD_FEATURE_PAGEFAULT_FLAG_WP support and a
	// permissive seccomp profile (not available under Docker's default profile).
	// Falls back to FullMemcpyStrategy automatically if uffd is unavailable.
	// Populated from FUNCTION_WASM_USE_UFFD=true.
	UseUffd bool `conf:"use_uffd"`

	// SnapshotMode selects the strategy used to snapshot and restore WASM linear
	// memory between requests. Valid values:
	//   "memcpy"     — copy all pages on every restore (default, always available)
	//   "soft-dirty" — use /proc/self/pagemap soft-dirty bits (Linux >= 3.18)
	//   "mprotect"   — use mprotect(PROT_READ) + SIGSEGV dirty tracking
	//                  (Linux + CGO, EXPERIMENTAL — installs a process-wide
	//                  SIGSEGV handler that intercepts every segfault in the Go
	//                  process. Requires the operator to also set
	//                  FUNCTION_WASM_ALLOW_EXPERIMENTAL_MPROTECT=true; otherwise
	//                  it silently falls back to "memcpy" with a warning log.
	//                  Not recommended for production.)
	//   "uffd"       — use userfaultfd write-protect (Linux, requires privilege)
	//   "cow"        — Linux-only sealed prepared image + MAP_PRIVATE per instance
	//                  via wazero's experimental memory allocator; fixed size
	//                  after Take and full-copy fallback on mismatch/unavailability
	//
	// Falls back to "memcpy" if the requested strategy is unavailable.
	// FUNCTION_WASM_SNAPSHOT_MODE env var.
	SnapshotMode string `conf:"wasm_snapshot_mode"`

	// PythonScriptPath is the host path to the trusted Python evaluation script.
	// Used by Python Reactor and the independent resident Python compatibility path.
	// Python Reactor scripts must define dispatch(method, payload).
	PythonScriptPath string `conf:"wasm_python_script"`

	// PythonPreloadMode controls whether Agent Python passes the trusted evaluator
	// through runtime_prepare. "evaluator" is the default; "off" executes the
	// trusted script in each fresh request namespace.
	PythonPreloadMode string `conf:"wasm_python_preload"`

	// PythonLifecycle selects whether Agent Python modules are initialized for
	// every request, consumed once from a prepared pool, or restored to their
	// prepared linear-memory snapshot and reused.
	PythonLifecycle string `conf:"wasm_python_lifecycle"`

	// PythonPreparedCapacity bounds never-served candidates retained by the
	// single-use lifecycle. The current numpy-core artifact retains 128 MiB of
	// Guest linear memory per candidate, so this surface is deliberately small.
	PythonPreparedCapacity int `conf:"wasm_python_prepared_capacity"`

	// PythonSnapshotHeadroomBytes reserves allocator capacity before Take so
	// normal requests do not immediately grow memory beyond a restorable baseline.
	PythonSnapshotHeadroomBytes uint64 `conf:"wasm_python_snapshot_headroom_bytes"`

	// CompileCacheDir, if non-empty, enables wazero's on-disk compilation cache.
	// Set via FUNCTION_WASM_COMPILE_CACHE env var. Shared across all runners and
	// processes that point at the same directory, making cold starts much faster
	// after the first compile.
	CompileCacheDir string `conf:"wasm_compile_cache"`

	// AgentPythonObserver receives optional phase evidence. Callbacks may be
	// concurrent during refill and must return promptly. It is never populated
	// from operator configuration.
	AgentPythonObserver func(AgentPythonPhaseEvent) `conf:"-"`
}

// applyDefaults fills in zero-value fields with sensible defaults.
// It also resolves the deprecated UseUffd bool into SnapshotMode so that
// downstream code only needs to check SnapshotMode.
func (c *Config) applyDefaults() {
	if c.Timeout == 0 {
		c.Timeout = 30 * time.Second
	}
	if c.MaxMemoryPages == 0 {
		c.MaxMemoryPages = 256 // 16 MB
	}
	if c.PythonPreloadMode == "" {
		c.PythonPreloadMode = "evaluator"
	}

	// Resolve deprecated UseUffd → SnapshotMode so downstream code never
	// needs to check both fields.
	if c.SnapshotMode == "" && c.UseUffd {
		c.SnapshotMode = "uffd"
	}
}

func (c *Config) validatePythonPreloadMode() error {
	switch c.PythonPreloadMode {
	case "evaluator", "off":
		return nil
	default:
		return fmt.Errorf("python preload mode %q is invalid; use \"evaluator\" or \"off\"", c.PythonPreloadMode)
	}
}

func (c *Config) applyAgentPythonDefaults() {
	if c.PythonLifecycle == "" {
		c.PythonLifecycle = "snapshot"
	}
	if c.PythonPreparedCapacity == 0 {
		c.PythonPreparedCapacity = 1
	}
	if c.PythonSnapshotHeadroomBytes == 0 {
		c.PythonSnapshotHeadroomBytes = 8 * 1024 * 1024
	}
	if c.PythonLifecycle == "snapshot" && c.SnapshotMode == "" {
		if c.UseUffd {
			c.SnapshotMode = "uffd"
		} else {
			c.SnapshotMode = "memcpy"
		}
	}
}

func (c *Config) validateAgentPythonLifecycle() error {
	switch c.PythonLifecycle {
	case "fresh", "single-use", "snapshot":
	default:
		return fmt.Errorf("agent Python lifecycle %q is invalid; use \"fresh\", \"single-use\", or \"snapshot\"", c.PythonLifecycle)
	}
	if c.PythonPreparedCapacity < 1 || c.PythonPreparedCapacity > 4 {
		return fmt.Errorf("agent Python prepared capacity %d is outside the supported range 1..4", c.PythonPreparedCapacity)
	}
	if c.MaxInstances > 4 {
		return fmt.Errorf("agent Python max instances %d exceeds the supported limit 4", c.MaxInstances)
	}
	if c.PythonLifecycle != "snapshot" && (c.SnapshotMode != "" || c.UseUffd) {
		return fmt.Errorf("snapshot strategy is only valid with lifecycle snapshot")
	}
	return nil
}

// validateSnapshotMode checks that the configured snapshot mode is known and
// compatible with the pool size. Modes that use process-wide state
// (soft-dirty, mprotect) are only safe with a single instance.
func (c *Config) validateSnapshotMode(poolSize int) error {
	switch c.SnapshotMode {
	case "", "memcpy", "soft-dirty", "mprotect", "uffd", "cow":
		// Known mode. Empty retains the full-memcpy baseline.
	default:
		return fmt.Errorf(
			"snapshot mode %q is invalid; use \"memcpy\", \"soft-dirty\", \"mprotect\", \"uffd\", or \"cow\"",
			c.SnapshotMode,
		)
	}

	if poolSize <= 1 {
		return nil
	}
	switch c.SnapshotMode {
	case "soft-dirty":
		return fmt.Errorf("snapshot mode %q is not safe with pool_size=%d > 1 (process-wide dirty bits cannot be attributed to individual instances); use \"memcpy\", \"uffd\", or \"cow\" instead", c.SnapshotMode, poolSize)
	case "mprotect":
		return fmt.Errorf("snapshot mode %q is not safe with pool_size=%d > 1 (global SIGSEGV handler cannot distinguish instances); use \"memcpy\", \"uffd\", or \"cow\" instead", c.SnapshotMode, poolSize)
	}
	return nil
}

// applyEnv reads sandbox fields from FUNCTION_WASM_* environment variables.
// This allows operators to configure sandbox limits without threading them
// through the full koanf config chain.
func (c *Config) applyEnv() {
	// FUNCTION_WASM_MODULE overrides FUNCTION_COMMAND as the .wasm file path.
	if v := os.Getenv("FUNCTION_WASM_MODULE"); v != "" {
		c.ModulePath = v
	}
	if v := os.Getenv("FUNCTION_WASM_MANIFEST"); v != "" {
		c.AgentPythonManifestPath = v
	}
	if v := os.Getenv("FUNCTION_WASM_MAX_MEMORY_PAGES"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 32); err == nil {
			c.MaxMemoryPages = uint32(n)
		}
	}
	if v := os.Getenv("FUNCTION_WASM_ALLOWED_PATHS"); v != "" {
		c.AllowedPaths = splitNonEmpty(v, ",")
	}
	if v := os.Getenv("FUNCTION_WASM_ALLOWED_ENV"); v != "" {
		c.AllowedEnv = splitNonEmpty(v, ",")
	}
	if v := os.Getenv("FUNCTION_WASM_USE_UFFD"); v == "true" || v == "1" {
		c.UseUffd = true
	}
	if v := os.Getenv("FUNCTION_WASM_SNAPSHOT_MODE"); v != "" {
		c.SnapshotMode = v
	}
	if v := os.Getenv("FUNCTION_WASM_PYTHON_SCRIPT"); v != "" {
		c.PythonScriptPath = v
	}
	if v := os.Getenv("FUNCTION_WASM_PYTHON_PRELOAD"); v != "" {
		c.PythonPreloadMode = v
	}
	if v := os.Getenv("FUNCTION_WASM_PYTHON_LIFECYCLE"); v != "" {
		c.PythonLifecycle = strings.TrimSpace(v)
	}
	if v := os.Getenv("FUNCTION_WASM_PYTHON_PREPARED_CAPACITY"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.PythonPreparedCapacity = n
		}
	}
	if v := os.Getenv("FUNCTION_WASM_PYTHON_SNAPSHOT_HEADROOM_BYTES"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			c.PythonSnapshotHeadroomBytes = n
		}
	}

	if v := os.Getenv("FUNCTION_WASM_COMPILE_CACHE"); v != "" {
		c.CompileCacheDir = v
	}
}

func splitNonEmpty(s, sep string) []string {
	var out []string
	for _, p := range strings.Split(s, sep) {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}
