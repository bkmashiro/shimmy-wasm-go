package execution

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/lambda-feedback/shimmy/internal/execution/supervisor"
)

const defaultQEMURunner = "shimmy-qemu-runner"

var requiredQEMUArtifacts = []string{
	"FUNCTION_QEMU_BINARY",
	"FUNCTION_QEMU_ROOTFS",
	"FUNCTION_QEMU_IMAGE_MANIFEST",
}

// applyExecutionWrappers resolves the mutually exclusive native worker
// wrappers before dispatcher selection. Neither wrapper is an IO interface.
func applyExecutionWrappers(cfg supervisor.Config) (supervisor.Config, error) {
	qemuEnabled, err := optionalBoolEnvironment("qemu", "FUNCTION_QEMU_ENABLED")
	if err != nil {
		return supervisor.Config{}, err
	}
	if !qemuEnabled {
		return applyDBISecurityConfig(cfg)
	}

	dbiEnabled, err := optionalBoolEnvironment("dbi", "FUNCTION_DBI_SECURITY_ENABLED")
	if err != nil {
		return supervisor.Config{}, err
	}
	if dbiEnabled {
		return supervisor.Config{}, fmt.Errorf("qemu: FUNCTION_QEMU_ENABLED and FUNCTION_DBI_SECURITY_ENABLED are mutually exclusive")
	}
	return applyQEMUFallbackConfig(cfg)
}

// applyQEMUFallbackConfig transparently replaces the native process command
// with shimmy-qemu-runner while preserving the file/RPC application interface.
// Lambda workers default to lazy clean-on-borrow: a contaminated runner is
// terminated after the request and the next request creates a clean VM.
// Adapter-specific EVAL_* env/argv are added later as before.
func applyQEMUFallbackConfig(cfg supervisor.Config) (supervisor.Config, error) {
	enabled, err := optionalBoolEnvironment("qemu", "FUNCTION_QEMU_ENABLED")
	if err != nil {
		return supervisor.Config{}, err
	}
	if !enabled {
		return cfg, nil
	}

	switch cfg.IO.Interface {
	case supervisor.FileIO, supervisor.RpcIO:
	default:
		return supervisor.Config{}, fmt.Errorf(
			"qemu: fallback wrapper is only supported for file and rpc interfaces; got %q",
			cfg.IO.Interface,
		)
	}

	originalCommand := strings.TrimSpace(cfg.StartParams.Cmd)
	if originalCommand == "" {
		return supervisor.Config{}, fmt.Errorf("qemu: FUNCTION_COMMAND must name the native worker command")
	}
	for _, name := range requiredQEMUArtifacts {
		path := strings.TrimSpace(os.Getenv(name))
		if path == "" {
			return supervisor.Config{}, fmt.Errorf("qemu: %s must be set", name)
		}
		file, openErr := os.Open(path)
		if openErr != nil {
			return supervisor.Config{}, fmt.Errorf("qemu: %s %q is not readable: %w", name, path, openErr)
		}
		if closeErr := file.Close(); closeErr != nil {
			return supervisor.Config{}, fmt.Errorf("qemu: close %s %q: %w", name, path, closeErr)
		}
	}

	accelerator := strings.ToLower(strings.TrimSpace(os.Getenv("FUNCTION_QEMU_ACCELERATOR")))
	if accelerator != "tcg" && accelerator != "kvm" {
		return supervisor.Config{}, fmt.Errorf("qemu: FUNCTION_QEMU_ACCELERATOR must be explicitly set to tcg or kvm")
	}

	resetPolicy := strings.ToLower(strings.TrimSpace(os.Getenv("FUNCTION_QEMU_RESET_POLICY")))
	if resetPolicy == "" {
		if strings.TrimSpace(os.Getenv("AWS_LAMBDA_RUNTIME_API")) != "" {
			resetPolicy = "lazy"
		} else {
			resetPolicy = "off"
		}
	}
	switch resetPolicy {
	case "off":
	case "lazy":
		cfg.WorkerLifecycle = supervisor.WorkerLifecycleInvocation
	case "eager":
		if cfg.IO.Interface != supervisor.RpcIO {
			return supervisor.Config{}, fmt.Errorf("qemu: FUNCTION_QEMU_RESET_POLICY %q requires rpc interface", resetPolicy)
		}
		if strings.TrimSpace(os.Getenv("AWS_LAMBDA_RUNTIME_API")) != "" {
			return supervisor.Config{}, fmt.Errorf(
				"qemu: FUNCTION_QEMU_RESET_POLICY %q requires a post-response Lambda runtime loop, which is not enabled",
				resetPolicy,
			)
		}
		cfg.WorkerLifecycle = supervisor.WorkerLifecycleEager
	default:
		return supervisor.Config{}, fmt.Errorf(
			"qemu: invalid FUNCTION_QEMU_RESET_POLICY %q; supported values are off, lazy, eager",
			resetPolicy,
		)
	}

	args := make([]string, 0, len(cfg.StartParams.Args)+2)
	args = append(args, "--", originalCommand)
	args = append(args, cfg.StartParams.Args...)
	cfg.StartParams.Cmd = firstNonEmpty(os.Getenv("FUNCTION_QEMU_RUNNER"), defaultQEMURunner)
	cfg.StartParams.Args = args
	return cfg, nil
}

func optionalBoolEnvironment(component, name string) (bool, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return false, nil
	}
	enabled, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("%s: invalid %s %q: %w", component, name, value, err)
	}
	return enabled, nil
}
