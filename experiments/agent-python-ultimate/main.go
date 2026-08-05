package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	runReportSchema = "agent-python-ultimate-run-report/v1"
	// workerProtocolExitCode is reserved for worker CLI/input/output protocol failures.
	// Row-level evaluator/runtime crashes must never intentionally use this code.
	workerProtocolExitCode = 90
)

// sourceCommit is injected from a verified clean worktree with
// -ldflags "-X main.sourceCommit=<commit>".
var sourceCommit string

type RunMetadata struct {
	StartedUTC     string `json:"started_utc"`
	FinishedUTC    string `json:"finished_utc,omitempty"`
	Hostname       string `json:"hostname"`
	GOOS           string `json:"goos"`
	GOARCH         string `json:"goarch"`
	GoVersion      string `json:"go_version"`
	ExecutableSHA  string `json:"executable_sha256"`
	ArtifactSHA    string `json:"artifact_sha256"`
	ManifestSHA    string `json:"manifest_sha256"`
	ConfigSHA      string `json:"config_sha256"`
	PlanSHA        string `json:"plan_sha256"`
	SourceCommit   string `json:"source_commit"`
	SourceModified bool   `json:"source_modified"`
	BuildInfo      string `json:"build_info,omitempty"`
	SlurmJobID     string `json:"slurm_job_id,omitempty"`
	SlurmNode      string `json:"slurm_node,omitempty"`
	RowsPlanned    int    `json:"rows_planned"`
	RowsCompleted  int    `json:"rows_completed"`
	RowsResumed    int    `json:"rows_resumed"`
	Complete       bool   `json:"complete"`
	StopReason     string `json:"stop_reason,omitempty"`
	OutlierPolicy  string `json:"outlier_policy"`
	TimingClock    string `json:"timing_clock"`
	ObserverPolicy string `json:"observer_policy"`
}

type inputFileIdentity struct {
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

type inputManifest struct {
	Schema       string                       `json:"schema"`
	SourceCommit string                       `json:"source_commit"`
	ConfigPath   string                       `json:"config_path"`
	PlanSeed     int64                        `json:"plan_seed"`
	Files        map[string]inputFileIdentity `json:"files"`
}

type Aggregate struct {
	Key             string  `json:"key"`
	Rows            int     `json:"rows"`
	OK              int     `json:"ok"`
	Unavailable     int     `json:"unavailable"`
	Failed          int     `json:"failed"`
	StartupMedianNS int64   `json:"startup_median_ns,omitempty"`
	StartupP95NS    int64   `json:"startup_p95_ns,omitempty"`
	RequestMedianNS int64   `json:"request_median_ns,omitempty"`
	RequestP95NS    int64   `json:"request_p95_ns,omitempty"`
	MeanRSSBytes    float64 `json:"mean_rss_bytes,omitempty"`
	MeanPSSBytes    float64 `json:"mean_pss_bytes,omitempty"`
}

type RunReport struct {
	Schema     string         `json:"schema"`
	Metadata   RunMetadata    `json:"metadata"`
	Plan       Plan           `json:"plan"`
	Rows       []WorkerResult `json:"rows"`
	ByCampaign []Aggregate    `json:"by_campaign"`
	ByLane     []Aggregate    `json:"by_lane"`
}

func main() {
	os.Exit(runMain(os.Args, os.Stderr))
}

func runMain(args []string, stderr io.Writer) int {
	if len(args) < 2 {
		fmt.Fprintf(stderr, "usage: %s plan|worker|run|validate ...\n", args[0])
		return 1
	}
	command := args[1]
	var err error
	switch command {
	case "plan":
		err = commandPlan(args[2:])
	case "worker":
		err = commandWorker(args[2:])
	case "run":
		err = commandRun(args[2:])
	case "validate":
		err = commandValidate(args[2:])
	default:
		err = fmt.Errorf("unknown command %q", command)
	}
	if err == nil {
		return 0
	}
	fmt.Fprintln(stderr, strings.TrimSpace(err.Error()))
	if command == "worker" {
		return workerProtocolExitCode
	}
	return 1
}

func commandPlan(args []string) error {
	flags := flag.NewFlagSet("plan", flag.ContinueOnError)
	configPath := flags.String("config", "configs/ultimate.json", "strict benchmark config")
	outputPath := flags.String("output", "-", "plan output or -")
	limit := flags.Int("limit", 0, "explicit local/smoke row limit")
	seed := flags.Int64("seed", 0, "optional deterministic plan seed override")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if err := validateExplicitSeed(flags, *seed); err != nil {
		return err
	}
	plan, _, err := loadExpandedPlan(*configPath, *limit, *seed)
	if err != nil {
		return err
	}
	raw, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		return err
	}
	if *outputPath == "-" {
		_, err = os.Stdout.Write(append(raw, '\n'))
		return err
	}
	return atomicWrite(*outputPath, append(raw, '\n'), 0o600)
}

func commandWorker(args []string) error {
	flags := flag.NewFlagSet("worker", flag.ContinueOnError)
	inputPath := flags.String("input", "", "worker input JSON")
	outputPath := flags.String("output", "", "worker result JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *inputPath == "" || *outputPath == "" {
		return errors.New("worker requires --input and --output")
	}
	raw, err := os.ReadFile(*inputPath)
	if err != nil {
		return err
	}
	input, err := ParseWorkerInput(raw)
	if err != nil {
		return err
	}
	result := RunWorker(context.Background(), input)
	return WriteWorkerResult(*outputPath, result)
}

func commandRun(args []string) error {
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	configPath := flags.String("config", "", "strict benchmark config")
	artifactPath := flags.String("artifact", "", "exact Agent Python wasm")
	manifestPath := flags.String("manifest", "", "exact artifact manifest")
	outputDir := flags.String("output", "", "private result directory")
	limit := flags.Int("limit", 0, "explicit smoke row limit")
	seed := flags.Int64("seed", 0, "optional deterministic plan seed override")
	maxDuration := flags.Duration("max-duration", 0, "optional parent wall-time guard")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if err := validateExplicitSeed(flags, *seed); err != nil {
		return err
	}
	if *configPath == "" || *artifactPath == "" || *manifestPath == "" || *outputDir == "" {
		return errors.New("run requires --config, --artifact, --manifest, and --output")
	}
	return runParent(*configPath, *artifactPath, *manifestPath, *outputDir, *limit, *seed, *maxDuration)
}

func validateExplicitSeed(flags *flag.FlagSet, seed int64) error {
	set := false
	flags.Visit(func(current *flag.Flag) {
		if current.Name == "seed" {
			set = true
		}
	})
	if set && seed <= 0 {
		return errors.New("--seed must be a positive integer")
	}
	return nil
}

func commandValidate(args []string) error {
	flags := flag.NewFlagSet("validate", flag.ContinueOnError)
	dir := flags.String("output", "", "run result directory")
	requireProvenance := flags.Bool("require-provenance", false, "require complete bundle provenance metadata")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *dir == "" {
		return errors.New("validate requires --output")
	}
	report, err := rebuildReport(*dir)
	if err != nil {
		return err
	}
	if *requireProvenance {
		if err := validateRunProvenance(*dir, report.Metadata, &report.Plan); err != nil {
			return err
		}
	}
	return writeJSON(filepath.Join(*dir, "report.recomputed.json"), report)
}

func runParent(configPath, artifactPath, manifestPath, outputDir string, limit int, seed int64, maxDuration time.Duration) error {
	configPath, _ = filepath.Abs(configPath)
	artifactPath, _ = filepath.Abs(artifactPath)
	manifestPath, _ = filepath.Abs(manifestPath)
	outputDir, _ = filepath.Abs(outputDir)
	for _, path := range []string{configPath, artifactPath, manifestPath} {
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Size() <= 0 {
			return fmt.Errorf("required input is not a non-empty regular file: %s", path)
		}
	}
	if err := os.MkdirAll(filepath.Join(outputDir, "rows"), 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(outputDir, "logs"), 0o700); err != nil {
		return err
	}
	if free, err := freeBytes(outputDir); err != nil || free < 4<<30 {
		return fmt.Errorf("output filesystem requires at least 4 GiB free (free=%d, err=%v)", free, err)
	}

	plan, configRaw, err := loadExpandedPlan(configPath, limit, seed)
	if err != nil {
		return err
	}
	planRaw, err := json.Marshal(plan)
	if err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	executableSHA, err := fileSHA256(executable)
	if err != nil {
		return fmt.Errorf("hash executable: %w", err)
	}
	artifactSHA, err := fileSHA256(artifactPath)
	if err != nil {
		return fmt.Errorf("hash artifact: %w", err)
	}
	manifestSHA, err := fileSHA256(manifestPath)
	if err != nil {
		return fmt.Errorf("hash manifest: %w", err)
	}
	hostname, _ := os.Hostname()
	metadata := RunMetadata{
		StartedUTC: time.Now().UTC().Format(time.RFC3339Nano), Hostname: hostname,
		GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, GoVersion: runtime.Version(),
		ExecutableSHA: executableSHA, ArtifactSHA: artifactSHA,
		ManifestSHA: manifestSHA, ConfigSHA: bytesSHA256(configRaw), PlanSHA: bytesSHA256(planRaw),
		SourceCommit: sourceCommit,
		SlurmJobID:   os.Getenv("SLURM_JOB_ID"), SlurmNode: os.Getenv("SLURMD_NODENAME"),
		RowsPlanned: len(plan.Rows), OutlierPolicy: "no deletion; warmups are separately labelled",
		TimingClock: "Go monotonic time embedded in time.Time", ObserverPolicy: "observer callback time excluded from each phase duration",
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		metadata.BuildInfo = info.String()
		buildRevision := ""
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				buildRevision = setting.Value
			case "vcs.modified":
				metadata.SourceModified = setting.Value == "true"
			}
		}
		if metadata.SourceCommit == "" {
			metadata.SourceCommit = buildRevision
		} else if buildRevision != "" && buildRevision != metadata.SourceCommit {
			return fmt.Errorf("linker source commit %s disagrees with build VCS revision %s", metadata.SourceCommit, buildRevision)
		}
	}
	if len(metadata.SourceCommit) != 40 {
		return errors.New("benchmark executable is not bound to a source commit")
	}
	if _, err := hex.DecodeString(metadata.SourceCommit); err != nil {
		return errors.New("benchmark executable source commit is not hexadecimal")
	}
	if metadata.SourceModified {
		return errors.New("benchmark executable was built from a modified source tree")
	}
	if err := validateResumeIdentity(outputDir, metadata); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(outputDir, "plan.json"), plan); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(outputDir, "metadata.json"), metadata); err != nil {
		return err
	}

	deadline := time.Time{}
	deadlineReserve := 30 * time.Minute
	if maxDuration > 0 {
		deadline = time.Now().Add(maxDuration)
		if maxDuration < time.Hour {
			deadlineReserve = time.Minute
		}
	}
	if raw := os.Getenv("SLURM_JOB_END_TIME"); raw != "" {
		if seconds, parseErr := strconv.ParseInt(raw, 10, 64); parseErr == nil {
			slurmDeadline := time.Unix(seconds, 0)
			if deadline.IsZero() || slurmDeadline.Before(deadline) {
				deadline = slurmDeadline
			}
		}
	}

	cacheDir := filepath.Join(outputDir, "compile-cache")
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		return err
	}
	if err := prewarmCompileCache(executable, artifactPath, manifestPath, cacheDir, outputDir, plan); err != nil {
		return fmt.Errorf("prewarm compile cache: %w", err)
	}

	for _, row := range plan.Rows {
		resultPath := filepath.Join(outputDir, "rows", row.ID+".json")
		if validWorkerResult(resultPath, row, "ok", "unavailable", "unsupported") {
			metadata.RowsResumed++
			metadata.RowsCompleted++
			continue
		}
		if !deadline.IsZero() && time.Until(deadline) < deadlineReserve {
			metadata.StopReason = fmt.Sprintf("deadline guard: less than %s remain", deadlineReserve)
			break
		}
		if free, err := freeBytes(outputDir); err != nil || free < 2<<30 {
			metadata.StopReason = fmt.Sprintf("disk guard: free=%d err=%v", free, err)
			break
		}
		input := WorkerInput{
			Schema: "agent-python-ultimate-worker-input/v1", Row: row,
			ArtifactPath: artifactPath, ManifestPath: manifestPath,
			CompileCacheDir: cacheDir, Seed: stableWorkerSeed(plan.Seed, row.ID),
		}
		inputPath := filepath.Join(outputDir, "rows", row.ID+".input.json")
		if err := writeJSON(inputPath, input); err != nil {
			return err
		}
		stdoutPath := filepath.Join(outputDir, "logs", row.ID+".stdout")
		stderrPath := filepath.Join(outputDir, "logs", row.ID+".stderr")
		if err := runPlanWorkerProcess(executable, inputPath, resultPath, stdoutPath, stderrPath, row); err != nil {
			return fmt.Errorf("worker %s: %w", row.ID, err)
		}
		info, err := os.Stat(resultPath)
		if err != nil || info.Size() > 16<<20 {
			return fmt.Errorf("worker %s result size invalid: size=%d err=%v", row.ID, sizeOrZero(info), err)
		}
		if !validWorkerResult(resultPath, row, "ok", "unavailable", "unsupported", "failed") {
			return fmt.Errorf("worker %s did not produce a structurally valid exact-row result", row.ID)
		}
		metadata.RowsCompleted++
		if err := appendCheckpoint(filepath.Join(outputDir, "checkpoint.jsonl"), row.ID); err != nil {
			return err
		}
		metadata.FinishedUTC = time.Now().UTC().Format(time.RFC3339Nano)
		if err := writeJSON(filepath.Join(outputDir, "metadata.json"), metadata); err != nil {
			return err
		}
	}
	metadata.Complete = metadata.RowsCompleted == metadata.RowsPlanned
	metadata.FinishedUTC = time.Now().UTC().Format(time.RFC3339Nano)
	if !metadata.Complete && metadata.StopReason == "" {
		metadata.StopReason = "incomplete row set"
	}
	if err := writeJSON(filepath.Join(outputDir, "metadata.json"), metadata); err != nil {
		return err
	}
	report, err := rebuildReport(outputDir)
	if err != nil {
		return err
	}
	report.Metadata = metadata
	return writeJSON(filepath.Join(outputDir, "report.json"), report)
}

func prewarmCompileCache(executable, artifact, manifest, cacheDir, outputDir string, plan *Plan) error {
	marker := filepath.Join(cacheDir, ".prewarmed")
	if _, err := os.Stat(marker); err == nil {
		return nil
	}
	if len(plan.Rows) == 0 {
		return errors.New("cannot prewarm empty plan")
	}
	row := plan.Rows[0]
	row.ID = "internal-prewarm"
	row.Campaign = "capability"
	row.Lifecycle = LifecycleFresh
	row.Pool = 1
	row.PreparedCapacity = 1
	row.Repeat = 1
	row.InputBytes, row.OutputBytes, row.ArenaMiB, row.DirtyBps, row.Concurrency = nil, nil, nil, nil, nil
	row.CPUProfile, row.DirtyPattern, row.Fault = "none", "", ""
	row.SnapshotSelected, row.SnapshotFallback, row.Surface, row.CacheState = "", false, "direct", "warm"
	input := WorkerInput{Schema: "agent-python-ultimate-worker-input/v1", Row: row, ArtifactPath: artifact, ManifestPath: manifest, CompileCacheDir: cacheDir, Seed: 1}
	inputPath := filepath.Join(outputDir, "prewarm.input.json")
	resultPath := filepath.Join(outputDir, "prewarm.result.json")
	if err := writeJSON(inputPath, input); err != nil {
		return err
	}
	if err := runWorkerProcess(executable, inputPath, resultPath, filepath.Join(outputDir, "logs", "prewarm.stdout"), filepath.Join(outputDir, "logs", "prewarm.stderr")); err != nil {
		return err
	}
	if !validWorkerResult(resultPath, row, "ok") {
		return errors.New("prewarm worker did not produce a valid result")
	}
	return atomicWrite(marker, []byte(time.Now().UTC().Format(time.RFC3339Nano)+"\n"), 0o600)
}

func runPlanWorkerProcess(executable, inputPath, resultPath, stdoutPath, stderrPath string, row PlanRow) error {
	startedUTC := time.Now().UTC().Format(time.RFC3339Nano)
	if err := runWorkerProcess(executable, inputPath, resultPath, stdoutPath, stderrPath); err != nil {
		if !workerProcessCrashed(err) {
			return err
		}
		return writeJSON(resultPath, WorkerResult{
			Schema:      workerResultSchema,
			Row:         row,
			Status:      "failed",
			StartedUTC:  startedUTC,
			FinishedUTC: time.Now().UTC().Format(time.RFC3339Nano),
			Error:       fmt.Sprintf("worker process crashed: %v", err),
		})
	}
	return nil
}

func workerProcessCrashed(err error) bool {
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) || exitError.ProcessState == nil {
		return false
	}
	return exitError.ExitCode() != workerProtocolExitCode
}

func runWorkerProcess(executable, inputPath, resultPath, stdoutPath, stderrPath string) error {
	stdout, err := os.OpenFile(stdoutPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer stdout.Close()
	stderr, err := os.OpenFile(stderrPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer stderr.Close()
	command := exec.Command(executable, "worker", "--input", inputPath, "--output", resultPath)
	command.Stdout, command.Stderr = stdout, stderr
	return command.Run()
}

func loadExpandedPlan(configPath string, limit int, seed int64) (*Plan, []byte, error) {
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return nil, nil, err
	}
	cfg, err := ParsePlanConfig(raw)
	if err != nil {
		return nil, nil, err
	}
	if seed == 0 {
		seed = cfg.Seed
	}
	plan, err := ExpandPlanFromConfig(cfg, PlanExpandOptions{Seed: seed})
	if err != nil {
		return nil, nil, err
	}
	if limit < 0 {
		return nil, nil, errors.New("limit must be non-negative")
	}
	if limit > 0 && limit < len(plan.Rows) {
		plan.Rows = append([]PlanRow(nil), plan.Rows[:limit]...)
	}
	return plan, raw, nil
}

func rebuildReport(outputDir string) (*RunReport, error) {
	planRaw, err := os.ReadFile(filepath.Join(outputDir, "plan.json"))
	if err != nil {
		return nil, err
	}
	plan, err := ParsePlanJSON(planRaw)
	if err != nil {
		return nil, err
	}
	metadataRaw, err := os.ReadFile(filepath.Join(outputDir, "metadata.json"))
	if err != nil {
		return nil, fmt.Errorf("read metadata.json: %w", err)
	}
	if len(metadataRaw) > 1<<20 {
		return nil, errors.New("metadata.json exceeds size limit")
	}
	metadata := RunMetadata{}
	if err := decodeStrictJSON(metadataRaw, &metadata); err != nil {
		return nil, fmt.Errorf("decode metadata.json: %w", err)
	}
	rows := make([]WorkerResult, 0, len(plan.Rows))
	for _, row := range plan.Rows {
		path := filepath.Join(outputDir, "rows", row.ID+".json")
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil, fmt.Errorf("read worker result %s: %w", path, readErr)
		}
		if len(raw) > 16<<20 {
			return nil, fmt.Errorf("worker result exceeds size limit: %s", path)
		}
		var result WorkerResult
		if err := decodeStrictJSON(raw, &result); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if err := validateTerminalWorkerResult(result, row, "ok", "unavailable", "unsupported", "failed"); err != nil {
			return nil, fmt.Errorf("worker result does not match exact plan row %s: %w", path, err)
		}
		rows = append(rows, result)
	}
	return &RunReport{
		Schema: runReportSchema, Metadata: metadata, Plan: *plan, Rows: rows,
		ByCampaign: aggregateRows(rows, func(row WorkerResult) string { return row.Row.Campaign }),
		ByLane:     aggregateRows(rows, func(row WorkerResult) string { return string(row.Row.Lifecycle) }),
	}, nil
}

func aggregateRows(rows []WorkerResult, keyFn func(WorkerResult) string) []Aggregate {
	groups := map[string][]WorkerResult{}
	for _, row := range rows {
		groups[keyFn(row)] = append(groups[keyFn(row)], row)
	}
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]Aggregate, 0, len(keys))
	for _, key := range keys {
		group := groups[key]
		var starts, requests []int64
		var rss, pss float64
		agg := Aggregate{Key: key, Rows: len(group)}
		for _, row := range group {
			switch row.Status {
			case "ok":
				agg.OK++
			case "unavailable", "unsupported":
				agg.Unavailable++
			default:
				agg.Failed++
			}
			starts = append(starts, int64(row.StartupDuration))
			for _, request := range row.Requests {
				requests = append(requests, int64(request.Duration))
			}
			rss += float64(row.BeforeShutdown.RSSBytes)
			pss += float64(row.BeforeShutdown.PSSBytes)
		}
		agg.StartupMedianNS, agg.StartupP95NS = percentile(starts, 0.5), percentile(starts, 0.95)
		agg.RequestMedianNS, agg.RequestP95NS = percentile(requests, 0.5), percentile(requests, 0.95)
		agg.MeanRSSBytes, agg.MeanPSSBytes = rss/float64(len(group)), pss/float64(len(group))
		out = append(out, agg)
	}
	return out
}

func percentile(values []int64, probability float64) int64 {
	if len(values) == 0 {
		return 0
	}
	copyValues := append([]int64(nil), values...)
	sort.Slice(copyValues, func(i, j int) bool { return copyValues[i] < copyValues[j] })
	index := int(math.Ceil(probability*float64(len(copyValues)))) - 1
	if index < 0 {
		index = 0
	}
	if index >= len(copyValues) {
		index = len(copyValues) - 1
	}
	return copyValues[index]
}

func validWorkerResult(path string, expectedRow PlanRow, allowedStatuses ...string) bool {
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) > 16<<20 {
		return false
	}
	var result WorkerResult
	if decodeStrictJSON(raw, &result) != nil {
		return false
	}
	return validateTerminalWorkerResult(result, expectedRow, allowedStatuses...) == nil
}

func validateTerminalWorkerResult(result WorkerResult, expectedRow PlanRow, allowedStatuses ...string) error {
	if result.Schema != workerResultSchema {
		return fmt.Errorf("unsupported worker result schema %q", result.Schema)
	}
	if !reflect.DeepEqual(result.Row, expectedRow) {
		return errors.New("result row differs from exact plan row")
	}
	allowed := false
	for _, status := range allowedStatuses {
		if result.Status == status {
			allowed = true
			break
		}
	}
	if !allowed {
		return fmt.Errorf("status %q is not allowed", result.Status)
	}
	started, err := time.Parse(time.RFC3339Nano, result.StartedUTC)
	if err != nil {
		return fmt.Errorf("invalid started_utc: %w", err)
	}
	finished, err := time.Parse(time.RFC3339Nano, result.FinishedUTC)
	if err != nil {
		return fmt.Errorf("invalid finished_utc: %w", err)
	}
	if finished.Before(started) {
		return errors.New("finished_utc precedes started_utc")
	}
	if result.StartupDuration < 0 || result.HTTPReadyDuration < 0 || result.ShutdownDuration < 0 {
		return errors.New("negative lifecycle duration")
	}
	seenRequestIndices := make(map[int]struct{}, len(result.Requests))
	for _, sample := range result.Requests {
		if sample.Index < 0 || sample.Index >= len(result.Requests) {
			return fmt.Errorf("request index out of range: %d", sample.Index)
		}
		if _, duplicate := seenRequestIndices[sample.Index]; duplicate {
			return fmt.Errorf("duplicate request index: %d", sample.Index)
		}
		seenRequestIndices[sample.Index] = struct{}{}
		if _, err := time.Parse(time.RFC3339Nano, sample.StartedUTC); err != nil {
			return fmt.Errorf("request %d has invalid started_utc: %w", sample.Index, err)
		}
		if sample.Duration <= 0 {
			return fmt.Errorf("request %d has non-positive duration", sample.Index)
		}
		if sample.Outcome != "ok" && sample.Outcome != "failed" {
			return fmt.Errorf("request %d has invalid outcome %q", sample.Index, sample.Outcome)
		}
		if (sample.Outcome == "ok") != (sample.Error == "") {
			return fmt.Errorf("request %d outcome/error mismatch", sample.Index)
		}
	}
	for index, event := range result.Phases {
		if event.Phase == "" || event.Duration < 0 {
			return fmt.Errorf("phase %d is incomplete", index)
		}
		if event.Outcome != "ok" && event.Outcome != "error" {
			return fmt.Errorf("phase %d has invalid outcome %q", index, event.Outcome)
		}
		if (event.Outcome == "ok") != (event.Error == "") {
			return fmt.Errorf("phase %d outcome/error mismatch", index)
		}
	}
	if result.Status == "ok" {
		if result.Error != "" {
			return errors.New("ok result contains error")
		}
		expectedRequests := expectedRow.Repeat
		if expectedRow.Concurrency != nil && *expectedRow.Concurrency > expectedRequests {
			expectedRequests = *expectedRow.Concurrency
		}
		if len(result.Requests) != expectedRequests {
			return fmt.Errorf("ok result has %d requests, expected %d", len(result.Requests), expectedRequests)
		}
		if result.StartupDuration <= 0 || result.ShutdownDuration <= 0 {
			return errors.New("ok result lacks positive startup/shutdown timing")
		}
		if len(result.Phases) == 0 {
			return errors.New("ok result lacks phase evidence")
		}
		for _, sample := range result.Requests {
			if sample.Outcome != "ok" {
				return errors.New("ok result contains failed request")
			}
		}
	} else if result.Error == "" {
		return fmt.Errorf("%s result lacks terminal error", result.Status)
	}
	return nil
}

func validateRunProvenance(outputDir string, metadata RunMetadata, plan *Plan) error {
	if plan == nil || plan.Seed <= 0 {
		return errors.New("provenance requires a positive plan seed")
	}
	if metadata.SourceModified {
		return errors.New("provenance rejects modified source")
	}
	if !isLowerHex(metadata.SourceCommit, 40) {
		return errors.New("invalid metadata source_commit")
	}
	for name, value := range map[string]string{
		"executable_sha256": metadata.ExecutableSHA,
		"artifact_sha256":   metadata.ArtifactSHA,
		"manifest_sha256":   metadata.ManifestSHA,
		"config_sha256":     metadata.ConfigSHA,
		"plan_sha256":       metadata.PlanSHA,
	} {
		if !isLowerHex(value, 64) {
			return fmt.Errorf("invalid metadata %s", name)
		}
	}
	if metadata.SlurmJobID == "" {
		return errors.New("provenance requires slurm_job_id")
	}
	if _, err := strconv.ParseUint(metadata.SlurmJobID, 10, 64); err != nil {
		return fmt.Errorf("invalid slurm_job_id: %w", err)
	}
	started, err := time.Parse(time.RFC3339Nano, metadata.StartedUTC)
	if err != nil {
		return fmt.Errorf("invalid metadata started_utc: %w", err)
	}
	finished, err := time.Parse(time.RFC3339Nano, metadata.FinishedUTC)
	if err != nil || finished.Before(started) {
		return errors.New("invalid metadata finished_utc")
	}
	if !metadata.Complete || metadata.StopReason != "" || metadata.RowsPlanned != len(plan.Rows) || metadata.RowsCompleted != len(plan.Rows) {
		return errors.New("metadata does not describe a complete exact plan row set")
	}
	if metadata.RowsResumed < 0 || metadata.RowsResumed > metadata.RowsCompleted {
		return errors.New("invalid rows_resumed")
	}
	planRaw, err := json.Marshal(plan)
	if err != nil {
		return err
	}
	if bytesSHA256(planRaw) != metadata.PlanSHA {
		return errors.New("metadata plan_sha256 mismatch")
	}
	provenanceDir := filepath.Join(outputDir, "provenance")
	manifestRaw, err := os.ReadFile(filepath.Join(provenanceDir, "input-manifest.json"))
	if err != nil || len(manifestRaw) > 1<<20 {
		return fmt.Errorf("read provenance input-manifest.json: size=%d err=%v", len(manifestRaw), err)
	}
	var manifest inputManifest
	if err := decodeStrictJSON(manifestRaw, &manifest); err != nil {
		return fmt.Errorf("decode provenance input-manifest.json: %w", err)
	}
	if manifest.Schema != "shimmy-agent-python-doc-input/v1" || manifest.SourceCommit != metadata.SourceCommit || manifest.PlanSeed != plan.Seed {
		return errors.New("input manifest source/seed identity mismatch")
	}
	if manifest.ConfigPath == "" || filepath.IsAbs(manifest.ConfigPath) || filepath.Clean(manifest.ConfigPath) != manifest.ConfigPath || strings.HasPrefix(manifest.ConfigPath, ".."+string(filepath.Separator)) {
		return errors.New("input manifest config_path is not canonical")
	}
	for filename, expected := range map[string]string{
		"agent-python-ultimate":                metadata.ExecutableSHA,
		"agent-python-runtime-numpy-core.wasm": metadata.ArtifactSHA,
		"manifest.json":                        metadata.ManifestSHA,
		"ultimate.json":                        metadata.ConfigSHA,
	} {
		identity, ok := manifest.Files[filename]
		if !ok || identity.Bytes <= 0 || identity.SHA256 != expected {
			return fmt.Errorf("input manifest identity mismatch for %s", filename)
		}
	}
	previewRaw, err := os.ReadFile(filepath.Join(provenanceDir, "plan.preview.json"))
	if err != nil || len(previewRaw) > 64<<20 {
		return fmt.Errorf("read provenance plan.preview.json: size=%d err=%v", len(previewRaw), err)
	}
	preview, err := ParsePlanJSON(previewRaw)
	if err != nil {
		return fmt.Errorf("decode provenance plan.preview.json: %w", err)
	}
	if !reflect.DeepEqual(*preview, *plan) {
		return errors.New("executed plan differs from provenance preview")
	}
	return nil
}

func isLowerHex(value string, length int) bool {
	if len(value) != length {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func validateResumeIdentity(outputDir string, expected RunMetadata) error {
	metadataPath := filepath.Join(outputDir, "metadata.json")
	raw, err := os.ReadFile(metadataPath)
	if errors.Is(err, os.ErrNotExist) {
		hasState := false
		walkErr := filepath.Walk(outputDir, func(path string, info os.FileInfo, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if path != outputDir && !info.IsDir() {
				hasState = true
				return filepath.SkipAll
			}
			return nil
		})
		if walkErr != nil && !errors.Is(walkErr, os.ErrNotExist) {
			return walkErr
		}
		if hasState {
			return errors.New("refusing to resume result files without metadata.json")
		}
		return nil
	}
	if err != nil {
		return err
	}
	var existing RunMetadata
	if err := json.Unmarshal(raw, &existing); err != nil {
		return fmt.Errorf("parse existing metadata.json: %w", err)
	}
	checks := []struct {
		name               string
		existing, expected string
	}{
		{"executable_sha256", existing.ExecutableSHA, expected.ExecutableSHA},
		{"artifact_sha256", existing.ArtifactSHA, expected.ArtifactSHA},
		{"manifest_sha256", existing.ManifestSHA, expected.ManifestSHA},
		{"config_sha256", existing.ConfigSHA, expected.ConfigSHA},
		{"plan_sha256", existing.PlanSHA, expected.PlanSHA},
		{"source_commit", existing.SourceCommit, expected.SourceCommit},
	}
	for _, check := range checks {
		if check.existing != check.expected {
			return fmt.Errorf("refusing to resume: %s differs (existing=%q expected=%q)", check.name, check.existing, check.expected)
		}
	}
	return nil
}

func appendCheckpoint(path, rowID string) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	entry, _ := json.Marshal(map[string]any{"row_id": rowID, "completed_utc": time.Now().UTC().Format(time.RFC3339Nano)})
	if _, err := file.Write(append(entry, '\n')); err != nil {
		return err
	}
	return file.Sync()
}

func writeJSON(path string, value any) error {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(path, append(raw, '\n'), 0o600)
}

func atomicWrite(path string, raw []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".tmp-")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if err := temp.Chmod(mode); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(raw); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempName, path)
}

func freeBytes(path string) (uint64, error) {
	var stats syscall.Statfs_t
	if err := syscall.Statfs(path, &stats); err != nil {
		return 0, err
	}
	return stats.Bavail * uint64(stats.Bsize), nil
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func bytesSHA256(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func sizeOrZero(info os.FileInfo) int64 {
	if info == nil {
		return 0
	}
	return info.Size()
}
