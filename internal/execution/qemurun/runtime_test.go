package qemurun

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/lambda-feedback/shimmy/internal/protocol"
)

func TestLoadRuntimeConfigUsesBoundedDefaultsWithExplicitTCG(t *testing.T) {
	env := runtimeFixtureEnvironment(t)
	got, err := LoadRuntimeConfig(mapEnvironment(env), false)
	if err != nil {
		t.Fatalf("LoadRuntimeConfig: %v", err)
	}
	if got.VM.Accelerator != AcceleratorTCG || got.VM.MemoryMB != 512 || got.VM.VCPUs != 1 || got.VM.RootFSFormat != "qcow2" {
		t.Fatalf("vm defaults = %#v", got.VM)
	}
	if got.VM.Network != NetworkNone || got.MaxFrameBytes != protocol.DefaultMaxMessageBytes {
		t.Fatalf("runtime defaults = %#v", got)
	}
	if got.BootTimeout != 60*time.Second || got.ShutdownTimeout != 10*time.Second {
		t.Fatalf("timeouts = %s %s", got.BootTimeout, got.ShutdownTimeout)
	}
	if got.VM.Kernel == "" || got.VM.Initrd == "" || got.VM.RootFS == "" {
		t.Fatalf("manifest artifacts not resolved: %#v", got.VM)
	}
}

func TestLoadRuntimeConfigAcceptsExplicitAvailableKVM(t *testing.T) {
	env := runtimeFixtureEnvironment(t)
	env["FUNCTION_QEMU_ACCELERATOR"] = "kvm"
	got, err := LoadRuntimeConfig(mapEnvironment(env), true)
	if err != nil {
		t.Fatalf("LoadRuntimeConfig: %v", err)
	}
	if got.VM.Accelerator != AcceleratorKVM {
		t.Fatalf("accelerator = %q, want kvm", got.VM.Accelerator)
	}
}

func TestLoadRuntimeConfigRejectsMissingOrAutoAccelerator(t *testing.T) {
	for _, value := range []string{"", "auto"} {
		t.Run(value, func(t *testing.T) {
			env := runtimeFixtureEnvironment(t)
			env["FUNCTION_QEMU_ACCELERATOR"] = value
			_, err := LoadRuntimeConfig(mapEnvironment(env), true)
			if !errors.Is(err, ErrInvalidRuntimeConfig) {
				t.Fatalf("error = %v, want ErrInvalidRuntimeConfig", err)
			}
		})
	}
}

func TestLoadRuntimeConfigRejectsExplicitUnavailableKVM(t *testing.T) {
	env := runtimeFixtureEnvironment(t)
	env["FUNCTION_QEMU_ACCELERATOR"] = "kvm"
	_, err := LoadRuntimeConfig(mapEnvironment(env), false)
	if !errors.Is(err, ErrKVMUnavailable) {
		t.Fatalf("error = %v, want ErrKVMUnavailable", err)
	}
}

func TestLoadRuntimeConfigParsesExplicitResourceAndNetworkSettings(t *testing.T) {
	env := runtimeFixtureEnvironment(t)
	env["FUNCTION_QEMU_ACCELERATOR"] = "tcg"
	env["FUNCTION_QEMU_MEMORY_MB"] = "768"
	env["FUNCTION_QEMU_VCPUS"] = "2"
	env["FUNCTION_QEMU_NETWORK_PROFILE"] = "inherit"
	env["FUNCTION_QEMU_MAX_FRAME_BYTES"] = "8388608"
	env["FUNCTION_QEMU_BOOT_TIMEOUT"] = "45s"
	env["FUNCTION_QEMU_SHUTDOWN_TIMEOUT"] = "7s"
	env["FUNCTION_QEMU_WORK_ROOT"] = filepath.Join(t.TempDir(), "work")
	got, err := LoadRuntimeConfig(mapEnvironment(env), false)
	if err != nil {
		t.Fatalf("LoadRuntimeConfig: %v", err)
	}
	if got.VM.MemoryMB != 768 || got.VM.VCPUs != 2 || got.VM.Network != NetworkUser {
		t.Fatalf("vm = %#v", got.VM)
	}
	if got.MaxFrameBytes != 8388608 || got.BootTimeout != 45*time.Second || got.ShutdownTimeout != 7*time.Second {
		t.Fatalf("runtime = %#v", got)
	}
}

func TestLoadRuntimeConfigRejectsInvalidBoundsAndProfiles(t *testing.T) {
	for name, keyValue := range map[string][2]string{
		"memory":        {"FUNCTION_QEMU_MEMORY_MB", "0"},
		"vcpus":         {"FUNCTION_QEMU_VCPUS", "65"},
		"frame bytes":   {"FUNCTION_QEMU_MAX_FRAME_BYTES", "128"},
		"accelerator":   {"FUNCTION_QEMU_ACCELERATOR", "magic"},
		"network":       {"FUNCTION_QEMU_NETWORK_PROFILE", "tap"},
		"boot timeout":  {"FUNCTION_QEMU_BOOT_TIMEOUT", "never"},
		"shutdown time": {"FUNCTION_QEMU_SHUTDOWN_TIMEOUT", "0s"},
	} {
		t.Run(name, func(t *testing.T) {
			env := runtimeFixtureEnvironment(t)
			env[keyValue[0]] = keyValue[1]
			_, err := LoadRuntimeConfig(mapEnvironment(env), false)
			if !errors.Is(err, ErrInvalidRuntimeConfig) {
				t.Fatalf("error = %v, want ErrInvalidRuntimeConfig", err)
			}
		})
	}
}

func runtimeFixtureEnvironment(t *testing.T) map[string]string {
	t.Helper()
	root := t.TempDir()
	kernel := writeArtifact(t, root, "vmlinuz", []byte("kernel"))
	initrd := writeArtifact(t, root, "initramfs.gz", []byte("initrd"))
	rootfs := writeArtifact(t, root, "root.qcow2", []byte("rootfs"))
	manifestPath := writeManifest(t, root, imageManifestFixture{
		SchemaVersion: 1,
		Architecture:  "x86_64",
		Kernel:        artifactFixture{Path: filepath.Base(kernel), SHA256: digestFixture([]byte("kernel"))},
		Initrd:        artifactFixture{Path: filepath.Base(initrd), SHA256: digestFixture([]byte("initrd"))},
		RootFS:        artifactFixture{Path: filepath.Base(rootfs), SHA256: digestFixture([]byte("rootfs")), Format: "qcow2"},
	})
	return map[string]string{
		"FUNCTION_QEMU_BINARY":         "/opt/qemu-system-x86_64",
		"FUNCTION_QEMU_ROOTFS":         rootfs,
		"FUNCTION_QEMU_IMAGE_MANIFEST": manifestPath,
		"FUNCTION_QEMU_ACCELERATOR":    "tcg",
	}
}

func mapEnvironment(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}
