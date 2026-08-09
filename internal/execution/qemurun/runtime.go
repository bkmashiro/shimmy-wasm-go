package qemurun

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/lambda-feedback/shimmy/internal/protocol"
)

const (
	minFrameBytes = 1024
	maxFrameBytes = 64 << 20
)

var (
	ErrInvalidRuntimeConfig = errors.New("qemu runner: invalid runtime config")
	ErrKVMUnavailable       = errors.New("qemu runner: kvm requested but unavailable")
)

type RuntimeConfig struct {
	VM              VMConfig
	MaxFrameBytes   int
	BootTimeout     time.Duration
	ShutdownTimeout time.Duration
	WorkRoot        string
}

func LoadRuntimeConfig(getenv func(string) string, kvmAvailable bool) (RuntimeConfig, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	image, err := LoadImageManifest(getenv("FUNCTION_QEMU_IMAGE_MANIFEST"), getenv("FUNCTION_QEMU_ROOTFS"))
	if err != nil {
		return RuntimeConfig{}, err
	}

	memory, err := intSetting(getenv, "FUNCTION_QEMU_MEMORY_MB", 512, 1, MaxMemoryMB)
	if err != nil {
		return RuntimeConfig{}, err
	}
	vcpus, err := intSetting(getenv, "FUNCTION_QEMU_VCPUS", 1, 1, MaxVCPUs)
	if err != nil {
		return RuntimeConfig{}, err
	}
	frameBytes, err := intSetting(
		getenv,
		"FUNCTION_QEMU_MAX_FRAME_BYTES",
		protocol.DefaultMaxMessageBytes,
		minFrameBytes,
		maxFrameBytes,
	)
	if err != nil {
		return RuntimeConfig{}, err
	}
	bootTimeout, err := durationSetting(getenv, "FUNCTION_QEMU_BOOT_TIMEOUT", 60*time.Second, 10*time.Millisecond, 10*time.Minute)
	if err != nil {
		return RuntimeConfig{}, err
	}
	shutdownTimeout, err := durationSetting(getenv, "FUNCTION_QEMU_SHUTDOWN_TIMEOUT", 10*time.Second, 10*time.Millisecond, 2*time.Minute)
	if err != nil {
		return RuntimeConfig{}, err
	}

	accelerator, err := resolveAccelerator(getenv("FUNCTION_QEMU_ACCELERATOR"), kvmAvailable)
	if err != nil {
		return RuntimeConfig{}, err
	}
	network, err := resolveNetwork(getenv("FUNCTION_QEMU_NETWORK_PROFILE"))
	if err != nil {
		return RuntimeConfig{}, err
	}
	binary := strings.TrimSpace(getenv("FUNCTION_QEMU_BINARY"))
	if binary == "" {
		return RuntimeConfig{}, fmt.Errorf("%w: FUNCTION_QEMU_BINARY must be set", ErrInvalidRuntimeConfig)
	}
	workRoot := strings.TrimSpace(getenv("FUNCTION_QEMU_WORK_ROOT"))
	if workRoot == "" {
		workRoot = "/tmp/shimmy-qemu"
	}

	return RuntimeConfig{
		VM: VMConfig{
			Binary:       binary,
			Kernel:       image.Kernel,
			Initrd:       image.Initrd,
			RootFS:       image.RootFS,
			RootFSFormat: image.RootFSFormat,
			Accelerator:  accelerator,
			MemoryMB:     memory,
			VCPUs:        vcpus,
			Network:      network,
		},
		MaxFrameBytes:   frameBytes,
		BootTimeout:     bootTimeout,
		ShutdownTimeout: shutdownTimeout,
		WorkRoot:        workRoot,
	}, nil
}

func intSetting(getenv func(string) string, name string, fallback, minimum, maximum int) (int, error) {
	value := strings.TrimSpace(getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < minimum || parsed > maximum {
		return 0, fmt.Errorf("%w: %s must be an integer in [%d,%d]", ErrInvalidRuntimeConfig, name, minimum, maximum)
	}
	return parsed, nil
}

func durationSetting(getenv func(string) string, name string, fallback, minimum, maximum time.Duration) (time.Duration, error) {
	value := strings.TrimSpace(getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed < minimum || parsed > maximum {
		return 0, fmt.Errorf("%w: %s must be a duration in [%s,%s]", ErrInvalidRuntimeConfig, name, minimum, maximum)
	}
	return parsed, nil
}

func resolveAccelerator(value string, kvmAvailable bool) (Accelerator, error) {
	switch Accelerator(strings.ToLower(strings.TrimSpace(value))) {
	case "", AcceleratorAuto:
		return "", fmt.Errorf("%w: FUNCTION_QEMU_ACCELERATOR must be explicitly set to tcg or kvm", ErrInvalidRuntimeConfig)
	case AcceleratorTCG:
		return AcceleratorTCG, nil
	case AcceleratorKVM:
		if !kvmAvailable {
			return "", ErrKVMUnavailable
		}
		return AcceleratorKVM, nil
	default:
		return "", fmt.Errorf("%w: unsupported FUNCTION_QEMU_ACCELERATOR %q", ErrInvalidRuntimeConfig, value)
	}
}

func resolveNetwork(value string) (NetworkProfile, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "none":
		return NetworkNone, nil
	case "inherit", "user":
		return NetworkUser, nil
	default:
		return "", fmt.Errorf("%w: unsupported FUNCTION_QEMU_NETWORK_PROFILE %q", ErrInvalidRuntimeConfig, value)
	}
}

func KVMAvailable() bool {
	device, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		return false
	}
	return device.Close() == nil
}
