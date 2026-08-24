package wasm

// This file carries Shimmy's Python Reactor Host/Guest contract.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
)

// Keep Host-to-Guest protocol frames bounded independently of evaluator-level
// limits. Student code and output use tighter limits in the evaluator.
const pythonReactorPayloadMaxBytes = 1 * 1024 * 1024

type PythonReactorArtifact struct {
	WasmBytes       []byte
	ABI             string
	Profile         string
	PythonModules   []string
	ProducerCommit  string
	SHA256          string
	ManifestPath    string
	InitExport      string
	PrepareExport   string
	ExecuteExport   string
	DeclaredExports []string
	DeclaredImports []pythonReactorImport
}

type shimmyPythonManifestEntry struct {
	Name   string `json:"name"`
	Module string `json:"module"`
	Kind   string `json:"kind"`
}

type shimmyPythonManifest struct {
	Schema           string   `json:"schema"`
	ArtifactContract string   `json:"artifact_contract"`
	Profile          string   `json:"profile"`
	Target           string   `json:"target"`
	ExecutionModel   string   `json:"execution_model"`
	PythonModules    []string `json:"python_modules"`
	IdentityU32      uint32   `json:"identity_u32"`
	Producer         struct {
		Project    string `json:"project"`
		Repository string `json:"repository"`
		Commit     string `json:"commit"`
		Dirty      bool   `json:"dirty"`
	} `json:"producer"`
	SourceDateEpoch int64 `json:"source_date_epoch"`
	Artifact        struct {
		Name   string `json:"name"`
		Size   int64  `json:"size"`
		SHA256 string `json:"sha256"`
	} `json:"artifact"`
	Wasm struct {
		Exports []shimmyPythonManifestEntry `json:"exports"`
		Imports []shimmyPythonManifestEntry `json:"imports"`
	} `json:"wasm"`
}

var pythonReactorCommitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

func verifyPythonReactorArtifact(modulePath, manifestPath string) (*PythonReactorArtifact, error) {
	if modulePath == "" {
		return nil, errors.New("python-reactor: ModulePath must be set (FUNCTION_WASM_MODULE)")
	}
	if manifestPath == "" {
		manifestPath = filepath.Join(filepath.Dir(modulePath), "manifest.json")
	}
	manifestBytes, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("python-reactor: read manifest %q: %w", manifestPath, err)
	}
	var format struct {
		Schema string `json:"schema"`
	}
	if err := json.Unmarshal(manifestBytes, &format); err != nil {
		return nil, fmt.Errorf("python-reactor: parse manifest: %w", err)
	}
	if format.Schema != "shimmy-python-runtime-artifact/v1" {
		return nil, errors.New("python-reactor: manifest must use shimmy-python-runtime/v1")
	}
	return verifyShimmyPythonArtifact(modulePath, manifestPath, manifestBytes)
}

func verifyShimmyPythonArtifact(modulePath, manifestPath string, manifestBytes []byte) (*PythonReactorArtifact, error) {
	var manifest shimmyPythonManifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return nil, fmt.Errorf("python-reactor: parse Shimmy producer manifest: %w", err)
	}
	if manifest.Schema != "shimmy-python-runtime-artifact/v1" || manifest.ArtifactContract != "shimmy-python-runtime/v1" {
		return nil, errors.New("python-reactor: unsupported Shimmy producer artifact contract")
	}
	if manifest.Target != "wasm32-wasip1" || manifest.ExecutionModel != "reactor" || manifest.IdentityU32 != 0x53505231 {
		return nil, errors.New("python-reactor: Shimmy producer target, execution model, or identity mismatch")
	}
	if manifest.Producer.Project != "shimmy" || manifest.Producer.Dirty || !pythonReactorCommitPattern.MatchString(manifest.Producer.Commit) {
		return nil, errors.New("python-reactor: Shimmy producer identity is invalid or dirty")
	}
	if manifest.SourceDateEpoch <= 0 {
		return nil, errors.New("python-reactor: Shimmy producer SOURCE_DATE_EPOCH is invalid")
	}
	expectedModules := map[string][]string{
		"base": {}, "numpy-core": {"numpy"}, "sympy": {"mpmath", "sympy"},
	}
	modules, ok := expectedModules[manifest.Profile]
	if !ok || !equalPythonReactorStrings(manifest.PythonModules, modules) {
		return nil, fmt.Errorf("python-reactor: manifest python_modules do not match profile %q", manifest.Profile)
	}
	if filepath.Base(manifest.Artifact.Name) != manifest.Artifact.Name || manifest.Artifact.Name != filepath.Base(modulePath) {
		return nil, fmt.Errorf("python-reactor: manifest artifact name %q does not bind module %q", manifest.Artifact.Name, filepath.Base(modulePath))
	}
	wasmBytes, err := os.ReadFile(modulePath)
	if err != nil {
		return nil, fmt.Errorf("python-reactor: read artifact %q: %w", modulePath, err)
	}
	if len(wasmBytes) < 8 || !bytes.Equal(wasmBytes[:8], []byte("\x00asm\x01\x00\x00\x00")) {
		return nil, errors.New("python-reactor: artifact is not a WebAssembly v1 module")
	}
	if manifest.Artifact.Size != int64(len(wasmBytes)) {
		return nil, fmt.Errorf("python-reactor: artifact size mismatch: manifest=%d actual=%d", manifest.Artifact.Size, len(wasmBytes))
	}
	digest := sha256.Sum256(wasmBytes)
	digestHex := hex.EncodeToString(digest[:])
	if manifest.Artifact.SHA256 != digestHex {
		return nil, errors.New("python-reactor: artifact SHA-256 does not match manifest")
	}

	exports := make([]string, 0, len(manifest.Wasm.Exports))
	seenExports := make(map[string]struct{}, len(manifest.Wasm.Exports))
	for _, entry := range manifest.Wasm.Exports {
		if entry.Name == "" || entry.Kind == "" {
			return nil, errors.New("python-reactor: malformed Shimmy producer export declaration")
		}
		if _, duplicate := seenExports[entry.Name]; duplicate {
			return nil, fmt.Errorf("python-reactor: duplicate manifest export %q", entry.Name)
		}
		seenExports[entry.Name] = struct{}{}
		exports = append(exports, entry.Name)
	}
	imports := make([]pythonReactorImport, 0, len(manifest.Wasm.Imports))
	seenImports := make(map[pythonReactorImport]struct{}, len(manifest.Wasm.Imports))
	for _, entry := range manifest.Wasm.Imports {
		declared := pythonReactorImport{Module: entry.Module, Name: entry.Name}
		if declared.Module != "wasi_snapshot_preview1" || declared.Name == "" || entry.Kind == "" {
			return nil, fmt.Errorf("python-reactor: unexpected Shimmy producer import %q.%q", declared.Module, declared.Name)
		}
		if _, duplicate := seenImports[declared]; duplicate {
			return nil, fmt.Errorf("python-reactor: duplicate manifest import %q.%q", declared.Module, declared.Name)
		}
		seenImports[declared] = struct{}{}
		imports = append(imports, declared)
	}
	return &PythonReactorArtifact{
		WasmBytes: wasmBytes, ABI: "shimmy-python-runtime/v1", Profile: manifest.Profile,
		PythonModules:  append([]string(nil), manifest.PythonModules...),
		ProducerCommit: manifest.Producer.Commit, SHA256: digestHex, ManifestPath: manifestPath,
		InitExport: "shimmy_python_init", PrepareExport: "shimmy_python_prepare", ExecuteExport: "evaluate",
		DeclaredExports: exports, DeclaredImports: imports,
	}, nil
}

func equalPythonReactorStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func buildShimmyPythonRunRequest(method string, params map[string]any) ([]byte, error) {
	if method == "" {
		method = "eval"
	}
	if params == nil {
		params = map[string]any{}
	}
	payload, err := json.Marshal(map[string]any{"method": method, "params": params})
	if err != nil {
		return nil, fmt.Errorf("python-reactor: encode Shimmy producer request: %w", err)
	}
	if len(payload) > pythonReactorPayloadMaxBytes {
		return nil, fmt.Errorf("python-reactor: run request exceeds %d-byte guest bound", pythonReactorPayloadMaxBytes)
	}
	return payload, nil
}

type shimmyPythonRunResponse struct {
	Status string          `json:"status"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

func decodeShimmyPythonResponse(payload []byte) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var response shimmyPythonRunResponse
	if err := decoder.Decode(&response); err != nil {
		return nil, fmt.Errorf("python-reactor: decode Shimmy producer response: %w", err)
	}
	if err := ensurePythonReactorJSONEOF(decoder); err != nil {
		return nil, err
	}
	switch response.Status {
	case "ok":
		if response.Error != nil || len(response.Result) == 0 || bytes.Equal(response.Result, []byte("null")) {
			return nil, errors.New("python-reactor: successful Shimmy producer response is malformed")
		}
		var result map[string]any
		if err := json.Unmarshal(response.Result, &result); err != nil || result == nil {
			return nil, errors.New("python-reactor: evaluator result must be a JSON object")
		}
		return result, nil
	case "error":
		if response.Error == nil || response.Error.Type == "" || response.Error.Message == "" || len(response.Result) != 0 {
			return nil, errors.New("python-reactor: failed Shimmy producer response is malformed")
		}
		return nil, &PythonReactorExecutionError{
			Code: "guest_error", Message: response.Error.Message, ErrorType: response.Error.Type,
		}
	default:
		return nil, fmt.Errorf("python-reactor: unsupported response status %q", response.Status)
	}
}

// PythonReactorExecutionError preserves a structured error returned by the
// evaluator-owned dispatcher. The sandbox does not reinterpret it as a normal
// result or map it to a different business method.
type PythonReactorExecutionError struct {
	Code      string
	Message   string
	ErrorType string
	Traceback string
}

func (e *PythonReactorExecutionError) Error() string {
	if e == nil {
		return "python-reactor: execution failed"
	}
	if e.Code == "" {
		return "python-reactor: " + e.Message
	}
	return fmt.Sprintf("python-reactor: %s: %s", e.Code, e.Message)
}

func ensurePythonReactorJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); errors.Is(err, io.EOF) {
		return nil
	} else if err != nil {
		return fmt.Errorf("python-reactor: decode trailing response JSON: %w", err)
	}
	return errors.New("python-reactor: response contains trailing JSON")
}
