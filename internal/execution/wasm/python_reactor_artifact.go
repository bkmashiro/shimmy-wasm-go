package wasm

import (
	"fmt"
	"slices"
	"sort"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

type pythonReactorImport struct {
	Module string `json:"module"`
	Name   string `json:"name"`
}

type pythonReactorFunctionSignature struct {
	Params  []api.ValueType
	Results []api.ValueType
}

type pythonReactorModuleShape struct {
	Exports          map[string]pythonReactorFunctionSignature
	ExportedMemories map[string]struct{}
	Imports          map[pythonReactorImport]struct{}
}

func inspectPythonReactorCompiledModule(compiled wazero.CompiledModule) (pythonReactorModuleShape, error) {
	if compiled == nil {
		return pythonReactorModuleShape{}, fmt.Errorf("python-reactor: compiled module is nil")
	}
	shape := pythonReactorModuleShape{
		Exports:          make(map[string]pythonReactorFunctionSignature),
		ExportedMemories: make(map[string]struct{}),
		Imports:          make(map[pythonReactorImport]struct{}),
	}
	for name, definition := range compiled.ExportedFunctions() {
		shape.Exports[name] = pythonReactorFunctionSignature{
			Params:  append([]api.ValueType(nil), definition.ParamTypes()...),
			Results: append([]api.ValueType(nil), definition.ResultTypes()...),
		}
	}
	for name := range compiled.ExportedMemories() {
		shape.ExportedMemories[name] = struct{}{}
	}
	for _, definition := range compiled.ImportedFunctions() {
		module, name, imported := definition.Import()
		if !imported {
			return pythonReactorModuleShape{}, fmt.Errorf("python-reactor: imported function has no import identity")
		}
		shape.Imports[pythonReactorImport{Module: module, Name: name}] = struct{}{}
	}
	for _, definition := range compiled.ImportedMemories() {
		module, name, imported := definition.Import()
		if !imported {
			return pythonReactorModuleShape{}, fmt.Errorf("python-reactor: imported memory has no import identity")
		}
		shape.Imports[pythonReactorImport{Module: module, Name: name}] = struct{}{}
	}
	return shape, nil
}

func verifyCompiledPythonReactorArtifact(compiled wazero.CompiledModule, artifact *PythonReactorArtifact) error {
	shape, err := inspectPythonReactorCompiledModule(compiled)
	if err != nil {
		return err
	}
	return verifyPythonReactorModuleShape(shape, artifact)
}

func verifyPythonReactorModuleShape(shape pythonReactorModuleShape, artifact *PythonReactorArtifact) error {
	if artifact == nil {
		return fmt.Errorf("python-reactor: artifact contract is nil")
	}

	initExport := artifact.InitExport
	prepareExport := artifact.PrepareExport
	executeExport := artifact.ExecuteExport
	i32 := api.ValueTypeI32
	required := map[string]pythonReactorFunctionSignature{
		"_initialize":                    {},
		initExport:                       {Results: []api.ValueType{i32}},
		prepareExport:                    {Params: []api.ValueType{i32, i32}, Results: []api.ValueType{i32}},
		"alloc":                          {Params: []api.ValueType{i32}, Results: []api.ValueType{i32}},
		"dealloc":                        {Params: []api.ValueType{i32}},
		executeExport:                    {Params: []api.ValueType{i32, i32}, Results: []api.ValueType{i32}},
		"shimmy_python_runtime_identity": {Results: []api.ValueType{i32}},
	}
	for name, expected := range required {
		actual, ok := shape.Exports[name]
		if !ok {
			return fmt.Errorf("python-reactor: actual module is missing required export %q", name)
		}
		if !samePythonReactorSignature(actual, expected) {
			return fmt.Errorf("python-reactor: export %q has ABI params=%s results=%s; want params=%s results=%s", name, formatWasmValueTypes(actual.Params), formatWasmValueTypes(actual.Results), formatWasmValueTypes(expected.Params), formatWasmValueTypes(expected.Results))
		}
	}
	if _, ok := shape.ExportedMemories["memory"]; !ok {
		return fmt.Errorf("python-reactor: actual module is missing required exported memory %q", "memory")
	}

	for _, name := range artifact.DeclaredExports {
		if _, ok := shape.Exports[name]; ok {
			continue
		}
		if _, ok := shape.ExportedMemories[name]; ok {
			continue
		}
		return fmt.Errorf("python-reactor: manifest export %q is absent from actual module", name)
	}

	declaredImports := make(map[pythonReactorImport]struct{}, len(artifact.DeclaredImports))
	for _, imported := range artifact.DeclaredImports {
		declaredImports[imported] = struct{}{}
	}
	var undeclared []pythonReactorImport
	for imported := range shape.Imports {
		if _, ok := declaredImports[imported]; !ok {
			undeclared = append(undeclared, imported)
		}
	}
	sort.Slice(undeclared, func(i, j int) bool {
		if undeclared[i].Module == undeclared[j].Module {
			return undeclared[i].Name < undeclared[j].Name
		}
		return undeclared[i].Module < undeclared[j].Module
	})
	if len(undeclared) > 0 {
		return fmt.Errorf("python-reactor: actual import %q.%q is not declared by manifest", undeclared[0].Module, undeclared[0].Name)
	}
	var absent []pythonReactorImport
	for imported := range declaredImports {
		if _, ok := shape.Imports[imported]; !ok {
			absent = append(absent, imported)
		}
	}
	sort.Slice(absent, func(i, j int) bool {
		if absent[i].Module == absent[j].Module {
			return absent[i].Name < absent[j].Name
		}
		return absent[i].Module < absent[j].Module
	})
	if len(absent) > 0 {
		return fmt.Errorf("python-reactor: manifest import %q.%q is absent from actual module", absent[0].Module, absent[0].Name)
	}
	return nil
}

func samePythonReactorSignature(actual, expected pythonReactorFunctionSignature) bool {
	return slices.Equal(actual.Params, expected.Params) && slices.Equal(actual.Results, expected.Results)
}

func formatPythonReactorSignature(signature pythonReactorFunctionSignature) string {
	return fmt.Sprintf("params=%s results=%s", formatWasmValueTypes(signature.Params), formatWasmValueTypes(signature.Results))
}

func formatWasmValueTypes(types []api.ValueType) string {
	if len(types) == 0 {
		return "[]"
	}
	names := make([]string, len(types))
	for i, valueType := range types {
		switch valueType {
		case api.ValueTypeI32:
			names[i] = "i32"
		case api.ValueTypeI64:
			names[i] = "i64"
		case api.ValueTypeF32:
			names[i] = "f32"
		case api.ValueTypeF64:
			names[i] = "f64"
		case api.ValueTypeExternref:
			names[i] = "externref"
		default:
			names[i] = fmt.Sprintf("0x%x", valueType)
		}
	}
	return fmt.Sprintf("%v", names)
}
