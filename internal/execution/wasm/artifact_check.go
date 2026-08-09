package wasm

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// ArtifactCheckOptions selects an explicitly declared runtime ABI. The checker
// never infers a source language or evaluator framework.
type ArtifactCheckOptions struct {
	Profile      string
	ModulePath   string
	ManifestPath string
}

// ArtifactCheckReport describes objective module facts plus advisory warnings.
// Warnings do not make an artifact invalid.
type ArtifactCheckReport struct {
	Profile  string   `json:"profile"`
	Module   string   `json:"module"`
	Exports  []string `json:"exports"`
	Imports  []string `json:"imports"`
	Warnings []string `json:"warnings,omitempty"`
}

// CheckArtifact compiles and validates a caller-produced module against the
// explicitly selected Shimmy runtime ABI.
func CheckArtifact(ctx context.Context, options ArtifactCheckOptions) (*ArtifactCheckReport, error) {
	profile := strings.ToLower(strings.TrimSpace(options.Profile))
	if profile != "generic" && profile != "python-reactor" {
		return nil, fmt.Errorf("artifact checker: unsupported profile %q; use generic or python-reactor", options.Profile)
	}
	if options.ModulePath == "" {
		return nil, fmt.Errorf("artifact checker: module path is required")
	}

	var (
		moduleBytes []byte
		artifact    *AgentPythonArtifact
		err         error
	)
	if profile == "python-reactor" {
		artifact, err = verifyAgentPythonArtifact(options.ModulePath, options.ManifestPath)
		if err != nil {
			return nil, err
		}
		moduleBytes = artifact.WasmBytes
	} else {
		moduleBytes, err = os.ReadFile(options.ModulePath)
		if err != nil {
			return nil, fmt.Errorf("artifact checker: read module %q: %w", options.ModulePath, err)
		}
	}

	runtime := wazero.NewRuntime(ctx)
	defer runtime.Close(ctx) //nolint:errcheck -- validation result has precedence
	compiled, err := runtime.CompileModule(ctx, moduleBytes)
	if err != nil {
		return nil, fmt.Errorf("artifact checker: compile module: %w", err)
	}

	if profile == "python-reactor" {
		if err := verifyCompiledPythonReactorArtifact(compiled, artifact); err != nil {
			return nil, err
		}
	} else if err := verifyGenericWasmArtifact(compiled); err != nil {
		return nil, err
	}

	report := reportCompiledArtifact(profile, options.ModulePath, compiled)
	if profile == "generic" {
		report.Warnings = genericWasmWarnings(compiled)
	}
	return report, nil
}

func verifyGenericWasmArtifact(compiled wazero.CompiledModule) error {
	if compiled == nil {
		return fmt.Errorf("generic wasm: compiled module is nil")
	}

	exports := compiled.ExportedFunctions()
	required := map[string]pythonReactorFunctionSignature{
		"alloc": {
			Params:  []api.ValueType{api.ValueTypeI32},
			Results: []api.ValueType{api.ValueTypeI32},
		},
		"dispatch": {
			Params:  []api.ValueType{api.ValueTypeI32, api.ValueTypeI32},
			Results: []api.ValueType{api.ValueTypeI32},
		},
	}
	for name, expected := range required {
		definition, ok := exports[name]
		if !ok {
			return fmt.Errorf("generic wasm: required export %q is missing", name)
		}
		actual := pythonReactorFunctionSignature{Params: definition.ParamTypes(), Results: definition.ResultTypes()}
		if !samePythonReactorSignature(actual, expected) {
			return fmt.Errorf("generic wasm: export %q has ABI %s; expected %s", name, formatPythonReactorSignature(actual), formatPythonReactorSignature(expected))
		}
	}
	if _, ok := compiled.ExportedMemories()["memory"]; !ok {
		return fmt.Errorf("generic wasm: required exported memory %q is missing", "memory")
	}

	for _, definition := range compiled.ImportedFunctions() {
		module, name, imported := definition.Import()
		if imported && module != "wasi_snapshot_preview1" {
			return fmt.Errorf("generic wasm: unsupported custom import %q.%q", module, name)
		}
	}
	for _, definition := range compiled.ImportedMemories() {
		module, name, imported := definition.Import()
		if imported && module != "wasi_snapshot_preview1" {
			return fmt.Errorf("generic wasm: unsupported custom memory import %q.%q", module, name)
		}
	}
	return nil
}

func reportCompiledArtifact(profile, modulePath string, compiled wazero.CompiledModule) *ArtifactCheckReport {
	exports := make([]string, 0, len(compiled.ExportedFunctions())+len(compiled.ExportedMemories()))
	for name := range compiled.ExportedFunctions() {
		exports = append(exports, name)
	}
	for name := range compiled.ExportedMemories() {
		exports = append(exports, name)
	}
	imports := make([]string, 0, len(compiled.ImportedFunctions())+len(compiled.ImportedMemories()))
	for _, definition := range compiled.ImportedFunctions() {
		module, name, imported := definition.Import()
		if imported {
			imports = append(imports, module+"."+name)
		}
	}
	for _, definition := range compiled.ImportedMemories() {
		module, name, imported := definition.Import()
		if imported {
			imports = append(imports, module+"."+name)
		}
	}
	sort.Strings(exports)
	sort.Strings(imports)
	return &ArtifactCheckReport{Profile: profile, Module: modulePath, Exports: exports, Imports: imports}
}

func genericWasmWarnings(compiled wazero.CompiledModule) []string {
	var hasFilesystem, hasNetwork bool
	for _, definition := range compiled.ImportedFunctions() {
		module, name, imported := definition.Import()
		if !imported || module != "wasi_snapshot_preview1" {
			continue
		}
		if strings.HasPrefix(name, "sock_") {
			hasNetwork = true
		}
		if strings.HasPrefix(name, "path_") || strings.HasPrefix(name, "fd_") {
			hasFilesystem = true
		}
	}
	warnings := make([]string, 0, 2)
	if hasFilesystem {
		warnings = append(warnings, "module imports WASI filesystem operations; behavior depends on explicitly allowed sandbox paths and may differ from native execution")
	}
	if hasNetwork {
		warnings = append(warnings, "module imports WASI socket operations; network behavior may be unavailable or differ from native execution")
	}
	return warnings
}
