// shimmy-artifact-check validates caller-produced WebAssembly artifacts without
// starting Shimmy's production request path.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"

	"github.com/lambda-feedback/shimmy/internal/execution/wasm"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	flags := flag.NewFlagSet("shimmy-artifact-check", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	profile := flags.String("profile", "generic", "runtime ABI: generic or python-reactor")
	module := flags.String("module", "", "path to a prebuilt WebAssembly module")
	manifest := flags.String("manifest", "", "Python Reactor manifest path")
	buildCommand := flags.String("build-command", "", "explicit producer command to run before validation")
	buildDir := flags.String("build-dir", ".", "working directory for --build-command")
	jsonOutput := flags.Bool("json", false, "emit a JSON report")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "unexpected arguments: %v\n", flags.Args())
		return 2
	}

	if *buildCommand != "" {
		command := exec.Command("/bin/sh", "-c", *buildCommand)
		command.Dir = *buildDir
		command.Stdout = os.Stdout
		command.Stderr = os.Stderr
		if err := command.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "artifact build failed: %v\n", err)
			return 1
		}
	}

	report, err := wasm.CheckArtifact(context.Background(), wasm.ArtifactCheckOptions{
		Profile:      *profile,
		ModulePath:   *module,
		ManifestPath: *manifest,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if *jsonOutput {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(report); err != nil {
			fmt.Fprintf(os.Stderr, "encode report: %v\n", err)
			return 1
		}
		return 0
	}

	fmt.Printf("OK %s artifact: %s\n", report.Profile, report.Module)
	fmt.Printf("exports: %v\n", report.Exports)
	fmt.Printf("imports: %v\n", report.Imports)
	for _, warning := range report.Warnings {
		fmt.Printf("WARNING: %s\n", warning)
	}
	return 0
}
