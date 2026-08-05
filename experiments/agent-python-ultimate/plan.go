package main

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"math/rand"
	"os"
	"sort"
)

const (
	PlanConfigSchema          = "agent-python-ultimate-config/v1"
	DefaultUltimateConfigPath = "configs/ultimate.json"
	DefaultSmokeConfigPath    = "configs/smoke.json"
)

var canonicalCampaignOrder = []string{
	"capability",
	"direct-startup",
	"http-full-boot",
	"input-payload",
	"output-payload",
	"payload-shape",
	"dirty-sparse",
	"dirty-pattern",
	"dirty-replication",
	"lifecycle-tail",
	"cpu",
	"interaction-corners",
	"concurrency",
	"single-use-saturation",
	"density",
	"fault-recovery",
}

var campaignRowCounts = map[string]int{
	"capability":            4,
	"direct-startup":        100,
	"http-full-boot":        40,
	"input-payload":         96,
	"output-payload":        84,
	"payload-shape":         48,
	"dirty-sparse":          348,
	"dirty-pattern":         72,
	"dirty-replication":     144,
	"lifecycle-tail":        8,
	"cpu":                   108,
	"interaction-corners":   144,
	"concurrency":           84,
	"single-use-saturation": 45,
	"density":               144,
	"fault-recovery":        84,
}

var inputPayloadTargets = []int{256, 1024, 4096, 16384, 65536, 262144, 524288, 1044480}
var outputPayloadTargets = []int{128, 4096, 65536, 262144, 524288, 921600}
var payloadShapes = []string{"flat-ascii", "nested", "numeric-array", "utf8"}
var dirtyPoints = []int{0, 1, 10, 100, 1000, 5000, 10000}
var arenaMiBOptions = []int{0, 8, 32, 64, 128}
var cpuProfiles = []string{"none", "python-10k", "python-100k", "python-1m", "python-5m", "numpy-16k", "numpy-256k", "numpy-1m", "numpy-matmul-64"}
var dirtyPatterns = []string{"contiguous", "sparse", "fixed-seed-random"}

// PlanConfig defines campaigns and expansion controls.
type PlanConfig struct {
	Schema         string                    `json:"schema"`
	Seed           int64                     `json:"seed"`
	Campaigns      map[string]CampaignConfig `json:"campaigns"`
	AllowLargePlan bool                      `json:"allow_large_plan"`
	MaxRows        int                       `json:"max_rows"`
}

// CampaignConfig toggles one campaign.
type CampaignConfig struct {
	Enabled bool `json:"enabled"`
}

// PlanExpandOptions controls deterministic expansion, filtering, and sharding.
type PlanExpandOptions struct {
	Seed           int64
	CampaignFilter []string
	ShardIndex     int
	ShardTotal     int
	FullFactorial  bool
	AllowLargePlan bool
	MaxRows        int
}

func ParsePlanConfig(raw []byte) (*PlanConfig, error) {
	var cfg PlanConfig
	if err := decodeStrictJSON(raw, &cfg); err != nil {
		return nil, err
	}
	if cfg.Schema != PlanConfigSchema {
		return nil, fmt.Errorf("unsupported config schema %q", cfg.Schema)
	}
	if cfg.Campaigns == nil {
		cfg.Campaigns = map[string]CampaignConfig{}
	}
	if cfg.Seed == 0 {
		cfg.Seed = 2026
	}
	return &cfg, nil
}

func LoadPlanConfig(path string) (*PlanConfig, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParsePlanConfig(raw)
}

func ExpandPlanFromConfig(cfg *PlanConfig, opts PlanExpandOptions) (*Plan, error) {
	if cfg == nil {
		return nil, fmt.Errorf("config is required")
	}

	if err := ValidateCampaignConfig(cfg); err != nil {
		return nil, err
	}

	seed := cfg.Seed
	if opts.Seed != 0 {
		seed = opts.Seed
	}

	maxRows := cfg.MaxRows
	if opts.MaxRows != 0 {
		maxRows = opts.MaxRows
	}
	if opts.FullFactorial {
		if !opts.AllowLargePlan {
			return nil, fmt.Errorf("full-factorial requires allow_large_plan")
		}
		if maxRows <= 0 {
			return nil, fmt.Errorf("full-factorial requires max_rows")
		}
	}

	filter := map[string]bool{}
	if len(opts.CampaignFilter) > 0 {
		for _, c := range opts.CampaignFilter {
			if _, ok := campaignRowCounts[c]; !ok {
				return nil, fmt.Errorf("unknown campaign filter %q", c)
			}
			filter[c] = true
		}
	}

	enabled := map[string]bool{}
	for _, name := range canonicalCampaignOrder {
		enabled[name] = false
	}
	for name, c := range cfg.Campaigns {
		enabled[name] = c.Enabled
	}

	rows := make([]PlanRow, 0)
	for _, campaign := range canonicalCampaignOrder {
		if len(filter) > 0 && !filter[campaign] {
			continue
		}
		if !enabled[campaign] {
			continue
		}
		rows = append(rows, expandCampaign(campaign)...)
	}

	for i := range rows {
		rows[i].ID = CanonicalRowID(rows[i], i)
	}

	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Campaign == rows[j].Campaign {
			return rows[i].ID < rows[j].ID
		}
		return rows[i].Campaign < rows[j].Campaign
	})

	r := rand.New(rand.NewSource(seed))
	r.Shuffle(len(rows), func(i, j int) { rows[i], rows[j] = rows[j], rows[i] })

	if maxRows > 0 && len(rows) > maxRows {
		return nil, fmt.Errorf("expanded rows %d exceed max_rows %d", len(rows), maxRows)
	}

	if opts.ShardTotal > 0 {
		if opts.ShardIndex < 0 || opts.ShardIndex >= opts.ShardTotal {
			return nil, fmt.Errorf("invalid shard index")
		}
		filtered := make([]PlanRow, 0)
		for _, row := range rows {
			if stableHashBucket(row.ID, opts.ShardTotal) == opts.ShardIndex {
				filtered = append(filtered, row)
			}
		}
		rows = filtered
	}

	plan := &Plan{Schema: PlanSchemaVersion, Seed: seed, Rows: rows}
	if err := ValidatePlan(plan); err != nil {
		return nil, err
	}

	// Campaign filters/shards are structural views; still ensure row ids are unique.
	seen := map[string]struct{}{}
	for _, row := range plan.Rows {
		if _, ok := seen[row.ID]; ok {
			return nil, fmt.Errorf("duplicate row id after expansion: %s", row.ID)
		}
		seen[row.ID] = struct{}{}
	}

	return plan, nil
}

func ValidateCampaignConfig(cfg *PlanConfig) error {
	if cfg == nil {
		return fmt.Errorf("config is required")
	}
	if cfg.Schema == "" {
		return fmt.Errorf("schema is required")
	}
	if cfg.Schema != PlanConfigSchema {
		return fmt.Errorf("unsupported config schema %q", cfg.Schema)
	}
	for name := range cfg.Campaigns {
		if _, ok := campaignRowCounts[name]; !ok {
			return fmt.Errorf("unknown campaign %q", name)
		}
	}
	return nil
}

func stableHashBucket(input string, mod int) int {
	h := fnv.New32a()
	h.Write([]byte(input))
	return int(h.Sum32() % uint32(mod))
}

func expandCampaign(name string) []PlanRow {
	switch name {
	case "capability":
		return capabilityRows()
	case "direct-startup":
		return directStartupRows()
	case "http-full-boot":
		return httpFullBootRows()
	case "input-payload":
		return inputPayloadRows()
	case "output-payload":
		return outputPayloadRows()
	case "payload-shape":
		return payloadShapeRows()
	case "dirty-sparse":
		return dirtySparseRows()
	case "dirty-pattern":
		return dirtyPatternRows()
	case "dirty-replication":
		return dirtyReplicationRows()
	case "lifecycle-tail":
		return lifecycleTailRows()
	case "cpu":
		return cpuRows()
	case "interaction-corners":
		return interactionCornerRows()
	case "concurrency":
		return concurrencyRows()
	case "single-use-saturation":
		return singleUseSaturationRows()
	case "density":
		return densityRows()
	case "fault-recovery":
		return faultRecoveryRows()
	default:
		return nil
	}
}

func baseRow(campaign string, lifecycle Lifecycle) PlanRow {
	return PlanRow{
		Campaign:         campaign,
		Lifecycle:        lifecycle,
		Pool:             (len(campaign) % 4) + 1,
		PreparedCapacity: (len(lifecycle) % 4) + 1,
		Repeat:           3,
		CacheState:       "warm",
		Surface:          "direct",
	}
}

func capabilityRows() []PlanRow {
	rows := make([]PlanRow, 0, 4)
	lcs := []Lifecycle{LifecycleFresh, LifecycleSingleUse, LifecycleSnapshotMemcpy, LifecycleSnapshotCow}
	for i, lc := range lcs {
		row := baseRow("capability", lc)
		row.Repeat = 1
		row.Pool = (i % 4) + 1
		row.PreparedCapacity = (i % 4) + 1
		if lc == LifecycleSnapshotMemcpy {
			row.SnapshotSelected = "memcpy"
		}
		if lc == LifecycleSnapshotCow {
			row.SnapshotSelected = "cow"
		}
		rows = append(rows, row)
	}
	return rows
}

func directStartupRows() []PlanRow {
	rows := make([]PlanRow, 0, campaignRowCounts["direct-startup"])
	lcs := []Lifecycle{LifecycleFresh, LifecycleSingleUse, LifecycleSnapshotMemcpy, LifecycleSnapshotCow}
	for i := 0; i < campaignRowCounts["direct-startup"]; i++ {
		row := baseRow("direct-startup", lcs[i%4])
		row.Repeat = 1
		if i%2 == 0 {
			row.CacheState = "cold"
		}
		row.PreparedCapacity = i%4 + 1
		row.CPUProfile = cpuProfiles[i%len(cpuProfiles)]
		row.DirtyPattern = dirtyPatterns[i%len(dirtyPatterns)]
		if row.Lifecycle == LifecycleSnapshotMemcpy {
			row.SnapshotSelected = "memcpy"
		}
		if row.Lifecycle == LifecycleSnapshotCow {
			row.SnapshotSelected = "cow"
		}
		rows = append(rows, row)
	}
	return rows
}

func httpFullBootRows() []PlanRow {
	rows := make([]PlanRow, 0, campaignRowCounts["http-full-boot"])
	lcs := []Lifecycle{LifecycleFresh, LifecycleSingleUse, LifecycleSnapshotMemcpy, LifecycleSnapshotCow}
	for i := 0; i < campaignRowCounts["http-full-boot"]; i++ {
		row := baseRow("http-full-boot", lcs[i%4])
		row.Surface = "http"
		if i%2 == 0 {
			row.CacheState = "cold"
		}
		row.Repeat = 2
		if row.Lifecycle == LifecycleSnapshotMemcpy {
			row.SnapshotSelected = "memcpy"
		}
		if row.Lifecycle == LifecycleSnapshotCow {
			row.SnapshotSelected = "cow"
		}
		rows = append(rows, row)
	}
	return rows
}

func inputPayloadRows() []PlanRow {
	rows := make([]PlanRow, 0, campaignRowCounts["input-payload"])
	lcs := []Lifecycle{LifecycleFresh, LifecycleSingleUse, LifecycleSnapshotMemcpy, LifecycleSnapshotCow}
	for i := 0; i < campaignRowCounts["input-payload"]; i++ {
		row := baseRow("input-payload", lcs[i%4])
		target := inputPayloadTargets[(i/12)%len(inputPayloadTargets)]
		row.InputBytes = &target
		row.PayloadShape = payloadShapes[(i/3)%len(payloadShapes)]
		row.Repeat = 3
		if row.Lifecycle == LifecycleSnapshotMemcpy {
			row.SnapshotSelected = "memcpy"
		}
		if row.Lifecycle == LifecycleSnapshotCow {
			row.SnapshotSelected = "cow"
		}
		rows = append(rows, row)
	}
	return rows
}

func outputPayloadRows() []PlanRow {
	rows := make([]PlanRow, 0, campaignRowCounts["output-payload"])
	lcs := []Lifecycle{LifecycleFresh, LifecycleSingleUse, LifecycleSnapshotMemcpy, LifecycleSnapshotCow}
	for i := 0; i < campaignRowCounts["output-payload"]; i++ {
		row := baseRow("output-payload", lcs[i%4])
		target := outputPayloadTargets[i%len(outputPayloadTargets)]
		row.OutputBytes = &target
		row.PayloadShape = payloadShapes[(i/21)%len(payloadShapes)]
		row.Repeat = 3
		if row.Lifecycle == LifecycleSnapshotMemcpy {
			row.SnapshotSelected = "memcpy"
		}
		if row.Lifecycle == LifecycleSnapshotCow {
			row.SnapshotSelected = "cow"
		}
		rows = append(rows, row)
	}
	return rows
}

func payloadShapeRows() []PlanRow {
	rows := make([]PlanRow, 0, campaignRowCounts["payload-shape"])
	lcs := []Lifecycle{LifecycleFresh, LifecycleSingleUse, LifecycleSnapshotMemcpy, LifecycleSnapshotCow}
	for i := 0; i < campaignRowCounts["payload-shape"]; i++ {
		row := baseRow("payload-shape", lcs[i%4])
		row.PayloadShape = payloadShapes[i/12]
		if i%2 == 0 {
			in := inputPayloadTargets[(i/2)%len(inputPayloadTargets)]
			row.InputBytes = &in
		} else {
			out := outputPayloadTargets[(i/2)%len(outputPayloadTargets)]
			row.OutputBytes = &out
		}
		if row.Lifecycle == LifecycleSnapshotMemcpy {
			row.SnapshotSelected = "memcpy"
		}
		if row.Lifecycle == LifecycleSnapshotCow {
			row.SnapshotSelected = "cow"
		}
		rows = append(rows, row)
	}
	return rows
}

func dirtySparseRows() []PlanRow {
	rows := make([]PlanRow, 0, campaignRowCounts["dirty-sparse"])
	lcs := []Lifecycle{LifecycleFresh, LifecycleSingleUse, LifecycleSnapshotMemcpy, LifecycleSnapshotCow}
	for i := 0; i < campaignRowCounts["dirty-sparse"]; i++ {
		row := baseRow("dirty-sparse", lcs[i%4])
		row.PayloadShape = payloadShapes[i%len(payloadShapes)]
		row.DirtyPattern = "sparse"
		d := dirtyPoints[i%len(dirtyPoints)]
		row.DirtyBps = &d
		a := arenaMiBOptions[(i/len(dirtyPoints))%len(arenaMiBOptions)]
		row.ArenaMiB = &a
		if row.Lifecycle == LifecycleSnapshotMemcpy {
			row.SnapshotSelected = "memcpy"
		}
		if row.Lifecycle == LifecycleSnapshotCow {
			row.SnapshotSelected = "cow"
		}
		rows = append(rows, row)
	}
	return rows
}

func dirtyPatternRows() []PlanRow {
	rows := make([]PlanRow, 0, campaignRowCounts["dirty-pattern"])
	lcs := []Lifecycle{LifecycleFresh, LifecycleSingleUse, LifecycleSnapshotMemcpy, LifecycleSnapshotCow}
	for i := 0; i < campaignRowCounts["dirty-pattern"]; i++ {
		row := baseRow("dirty-pattern", lcs[i%4])
		row.DirtyPattern = dirtyPatterns[i%len(dirtyPatterns)]
		d := dirtyPoints[(i/len(dirtyPatterns))%len(dirtyPoints)]
		row.DirtyBps = &d
		a := arenaMiBOptions[i%len(arenaMiBOptions)]
		row.ArenaMiB = &a
		if row.Lifecycle == LifecycleSnapshotMemcpy {
			row.SnapshotSelected = "memcpy"
		}
		if row.Lifecycle == LifecycleSnapshotCow {
			row.SnapshotSelected = "cow"
		}
		rows = append(rows, row)
	}
	return rows
}

func dirtyReplicationRows() []PlanRow {
	lifecycles := []Lifecycle{LifecycleSingleUse, LifecycleSnapshotMemcpy, LifecycleSnapshotCow}
	arenas := []int{8, 32, 64, 128}
	dirtyRates := []int{10, 100, 1000, 5000}
	patterns := []string{"contiguous", "sparse", "fixed-seed-random"}
	rows := make([]PlanRow, 0, campaignRowCounts["dirty-replication"])
	for _, lifecycle := range lifecycles {
		for _, arena := range arenas {
			for _, dirtyRate := range dirtyRates {
				for _, pattern := range patterns {
					arenaValue, dirtyValue := arena, dirtyRate
					row := baseRow("dirty-replication", lifecycle)
					row.Pool, row.PreparedCapacity, row.Repeat = 1, 1, 3
					row.ArenaMiB, row.DirtyBps = &arenaValue, &dirtyValue
					row.DirtyPattern = pattern
					row.PayloadShape = "flat-ascii"
					if lifecycle == LifecycleSnapshotMemcpy {
						row.SnapshotSelected = "memcpy"
					}
					if lifecycle == LifecycleSnapshotCow {
						row.SnapshotSelected = "cow"
					}
					rows = append(rows, row)
				}
			}
		}
	}
	return rows
}

func lifecycleTailRows() []PlanRow {
	lifecycles := []Lifecycle{LifecycleFresh, LifecycleSingleUse, LifecycleSnapshotMemcpy, LifecycleSnapshotCow}
	concurrencyLevels := []int{1, 4}
	rows := make([]PlanRow, 0, campaignRowCounts["lifecycle-tail"])
	for _, lifecycle := range lifecycles {
		for _, concurrency := range concurrencyLevels {
			concurrencyValue := concurrency
			row := baseRow("lifecycle-tail", lifecycle)
			row.Pool, row.PreparedCapacity, row.Repeat = concurrency, concurrency, 12
			row.Concurrency = &concurrencyValue
			row.PayloadShape = "flat-ascii"
			if lifecycle == LifecycleSnapshotMemcpy {
				row.SnapshotSelected = "memcpy"
			}
			if lifecycle == LifecycleSnapshotCow {
				row.SnapshotSelected = "cow"
			}
			rows = append(rows, row)
		}
	}
	return rows
}

func stableWorkerSeed(planSeed int64, rowID string) int64 {
	h := fnv.New64a()
	_, _ = fmt.Fprintf(h, "%d\x00%s", planSeed, rowID)
	seed := int64(h.Sum64() & ((1 << 63) - 1))
	if seed == 0 {
		return 1
	}
	return seed
}

func cpuRows() []PlanRow {
	rows := make([]PlanRow, 0, campaignRowCounts["cpu"])
	lcs := []Lifecycle{LifecycleFresh, LifecycleSingleUse, LifecycleSnapshotMemcpy, LifecycleSnapshotCow}
	for i := 0; i < campaignRowCounts["cpu"]; i++ {
		row := baseRow("cpu", lcs[i%4])
		row.CPUProfile = cpuProfiles[i%len(cpuProfiles)]
		row.DirtyPattern = dirtyPatterns[(i/len(cpuProfiles))%len(dirtyPatterns)]
		if row.Lifecycle == LifecycleSnapshotMemcpy {
			row.SnapshotSelected = "memcpy"
		}
		if row.Lifecycle == LifecycleSnapshotCow {
			row.SnapshotSelected = "cow"
		}
		rows = append(rows, row)
	}
	return rows
}

func interactionCornerRows() []PlanRow {
	rows := make([]PlanRow, 0, campaignRowCounts["interaction-corners"])
	lcs := []Lifecycle{LifecycleFresh, LifecycleSingleUse, LifecycleSnapshotMemcpy, LifecycleSnapshotCow}
	for i := 0; i < campaignRowCounts["interaction-corners"]; i++ {
		row := baseRow("interaction-corners", lcs[i%4])
		row.CPUProfile = cpuProfiles[(i/4)%len(cpuProfiles)]
		row.PayloadShape = payloadShapes[(i/12)%len(payloadShapes)]
		c := (i % 16) + 1
		row.Concurrency = &c
		if row.Lifecycle == LifecycleSnapshotMemcpy {
			row.SnapshotSelected = "memcpy"
		}
		if row.Lifecycle == LifecycleSnapshotCow {
			row.SnapshotSelected = "cow"
		}
		rows = append(rows, row)
	}
	return rows
}

func concurrencyRows() []PlanRow {
	rows := make([]PlanRow, 0, campaignRowCounts["concurrency"])
	lcs := []Lifecycle{LifecycleFresh, LifecycleSingleUse, LifecycleSnapshotMemcpy, LifecycleSnapshotCow}
	conc := []int{1, 2, 4, 8, 16}
	for i := 0; i < campaignRowCounts["concurrency"]; i++ {
		row := baseRow("concurrency", lcs[i%4])
		c := conc[i%len(conc)]
		row.Concurrency = &c
		if row.Lifecycle == LifecycleSnapshotMemcpy {
			row.SnapshotSelected = "memcpy"
		}
		if row.Lifecycle == LifecycleSnapshotCow {
			row.SnapshotSelected = "cow"
		}
		rows = append(rows, row)
	}
	return rows
}

func singleUseSaturationRows() []PlanRow {
	rows := make([]PlanRow, 0, campaignRowCounts["single-use-saturation"])
	for i := 0; i < campaignRowCounts["single-use-saturation"]; i++ {
		row := baseRow("single-use-saturation", LifecycleSingleUse)
		if i%2 == 0 {
			a := arenaMiBOptions[(i/3)%len(arenaMiBOptions)]
			row.ArenaMiB = &a
		}
		d := dirtyPoints[i%len(dirtyPoints)]
		row.DirtyBps = &d
		if row.Lifecycle == LifecycleSnapshotMemcpy {
			row.SnapshotSelected = "memcpy"
		}
		if row.Lifecycle == LifecycleSnapshotCow {
			row.SnapshotSelected = "cow"
		}
		rows = append(rows, row)
	}
	return rows
}

func densityRows() []PlanRow {
	rows := make([]PlanRow, 0, campaignRowCounts["density"])
	lcs := []Lifecycle{LifecycleFresh, LifecycleSingleUse, LifecycleSnapshotMemcpy, LifecycleSnapshotCow}
	for i := 0; i < campaignRowCounts["density"]; i++ {
		row := baseRow("density", lcs[i%4])
		row.PayloadShape = payloadShapes[(i/36)%len(payloadShapes)]
		row.CPUProfile = cpuProfiles[(i/16)%len(cpuProfiles)]
		if row.Lifecycle == LifecycleSnapshotMemcpy {
			row.SnapshotSelected = "memcpy"
		}
		if row.Lifecycle == LifecycleSnapshotCow {
			row.SnapshotSelected = "cow"
		}
		rows = append(rows, row)
	}
	return rows
}

func faultRecoveryRows() []PlanRow {
	rows := make([]PlanRow, 0, campaignRowCounts["fault-recovery"])
	lcs := []Lifecycle{LifecycleFresh, LifecycleSingleUse, LifecycleSnapshotMemcpy, LifecycleSnapshotCow}
	for i := 0; i < campaignRowCounts["fault-recovery"]; i++ {
		row := baseRow("fault-recovery", lcs[i%4])
		row.Repeat = 1
		faults := []string{"exception", "timeout", "cancel", "memory-growth", "oversized-input", "oversized-output", "recovery"}
		row.Fault = faults[i%len(faults)]
		row.CPUProfile = cpuProfiles[(i+3)%len(cpuProfiles)]
		row.PayloadShape = payloadShapes[(i+1)%len(payloadShapes)]
		if row.Lifecycle == LifecycleSnapshotMemcpy {
			row.SnapshotSelected = "memcpy"
		}
		if row.Lifecycle == LifecycleSnapshotCow {
			row.SnapshotSelected = "cow"
		}
		rows = append(rows, row)
	}
	return rows
}

func CampaignCountsFromConfig(cfg *PlanConfig, filter []string) map[string]int {
	include := map[string]bool{}
	if len(filter) == 0 {
		for _, name := range canonicalCampaignOrder {
			include[name] = true
		}
	} else {
		for _, name := range filter {
			include[name] = true
		}
	}
	if cfg != nil {
		for name, c := range cfg.Campaigns {
			if !c.Enabled {
				delete(include, name)
			}
		}
	}
	counts := map[string]int{}
	for name := range include {
		if count, ok := campaignRowCounts[name]; ok {
			counts[name] = count
		}
	}
	return counts
}

func MustMarshalPlanConfig(cfg *PlanConfig) ([]byte, error) {
	return json.Marshal(cfg)
}
