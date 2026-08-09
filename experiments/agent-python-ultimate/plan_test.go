package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPlanConfigStrictDecoderRejectsUnknownField(t *testing.T) {
	_, err := ParsePlanConfig([]byte(`{"schema":"agent-python-ultimate-config/v1","seed":1,"unknown":1}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown field")
}

func TestPlanExpanderReturnsExpectedUltimateRowCount(t *testing.T) {
	path := filepath.Join("configs", "ultimate.json")
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	cfg, err := ParsePlanConfig(raw)
	require.NoError(t, err)

	plan, err := ExpandPlanFromConfig(cfg, PlanExpandOptions{Seed: cfg.Seed})
	require.NoError(t, err)
	assert.Equal(t, 1401, len(plan.Rows))

	counts := map[string]int{}
	for _, row := range plan.Rows {
		counts[row.Campaign]++
	}
	for campaign, config := range cfg.Campaigns {
		if config.Enabled {
			assert.Equalf(t, campaignRowCounts[campaign], counts[campaign], "campaign %s mismatch", campaign)
		}
	}
}

func TestUltimateRowsRespectProductionRuntimeBounds(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("configs", "ultimate.json"))
	require.NoError(t, err)
	cfg, err := ParsePlanConfig(raw)
	require.NoError(t, err)
	plan, err := ExpandPlanFromConfig(cfg, PlanExpandOptions{Seed: cfg.Seed})
	require.NoError(t, err)

	for _, row := range plan.Rows {
		if row.Concurrency != nil {
			assert.LessOrEqual(t, *row.Concurrency, 16, row.ID)
		}
		switch row.Lifecycle {
		case LifecycleSnapshotMemcpy:
			assert.Equal(t, "memcpy", row.SnapshotSelected, row.ID)
		case LifecycleSnapshotCow:
			assert.Equal(t, "cow", row.SnapshotSelected, row.ID)
		}
	}
}

func TestPlanExpanderStableIDsAndShardCoverage(t *testing.T) {
	path := filepath.Join("configs", "ultimate.json")
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	cfg, err := ParsePlanConfig(raw)
	require.NoError(t, err)

	a, err := ExpandPlanFromConfig(cfg, PlanExpandOptions{Seed: 20260727})
	require.NoError(t, err)
	b, err := ExpandPlanFromConfig(cfg, PlanExpandOptions{Seed: 20260727})
	require.NoError(t, err)

	aIDs := make([]string, 0, len(a.Rows))
	bIDs := make([]string, 0, len(b.Rows))
	for _, r := range a.Rows {
		aIDs = append(aIDs, r.ID)
	}
	for _, r := range b.Rows {
		bIDs = append(bIDs, r.ID)
	}
	assert.Equal(t, aIDs, bIDs)

	union := map[string]struct{}{}
	for shard := 0; shard < 4; shard++ {
		planShard, err := ExpandPlanFromConfig(cfg, PlanExpandOptions{Seed: cfg.Seed, ShardIndex: shard, ShardTotal: 4})
		require.NoError(t, err)
		for _, row := range planShard.Rows {
			_, exists := union[row.ID]
			require.False(t, exists, "row %s appears in multiple shards", row.ID)
			union[row.ID] = struct{}{}
		}
	}
	assert.Len(t, union, len(a.Rows))
}

func TestPlanExpanderCampaignFilterAndFullFactorialGuardrails(t *testing.T) {
	path := filepath.Join("configs", "smoke.json")
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	cfg, err := ParsePlanConfig(raw)
	require.NoError(t, err)

	plan, err := ExpandPlanFromConfig(cfg, PlanExpandOptions{Seed: cfg.Seed, CampaignFilter: []string{"direct-startup"}})
	require.NoError(t, err)
	for _, r := range plan.Rows {
		assert.Equal(t, "direct-startup", r.Campaign)
	}

	_, err = ExpandPlanFromConfig(cfg, PlanExpandOptions{FullFactorial: true, AllowLargePlan: false, Seed: cfg.Seed})
	require.Error(t, err)
	_, err = ExpandPlanFromConfig(cfg, PlanExpandOptions{FullFactorial: true, AllowLargePlan: true, MaxRows: 1, Seed: cfg.Seed})
	require.Error(t, err)
}

func TestPlanConfigMaxRowsAppliedForCampaignSubset(t *testing.T) {
	cfg := &PlanConfig{
		Schema:         PlanConfigSchema,
		Seed:           2026,
		MaxRows:        10,
		AllowLargePlan: true,
		Campaigns: map[string]CampaignConfig{
			"capability": {Enabled: true},
		},
	}

	plan, err := ExpandPlanFromConfig(cfg, PlanExpandOptions{MaxRows: 4})
	require.NoError(t, err)
	assert.Equal(t, 4, len(plan.Rows))

	_, err = ExpandPlanFromConfig(cfg, PlanExpandOptions{MaxRows: 1, Seed: 2})
	require.Error(t, err)
}

func TestFocusedDirtyReplicationPlanIsExplicitAndSeeded(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("configs", "focused-dirty.json"))
	require.NoError(t, err)
	cfg, err := ParsePlanConfig(raw)
	require.NoError(t, err)

	plan, err := ExpandPlanFromConfig(cfg, PlanExpandOptions{Seed: 2026080601})
	require.NoError(t, err)
	require.Len(t, plan.Rows, 144)
	assert.Equal(t, int64(2026080601), plan.Seed)

	for _, row := range plan.Rows {
		assert.Equal(t, "dirty-replication", row.Campaign)
		assert.Contains(t, []Lifecycle{LifecycleSingleUse, LifecycleSnapshotMemcpy, LifecycleSnapshotCow}, row.Lifecycle)
		require.NotNil(t, row.ArenaMiB)
		assert.Contains(t, []int{8, 32, 64, 128}, *row.ArenaMiB)
		require.NotNil(t, row.DirtyBps)
		assert.Contains(t, []int{10, 100, 1000, 5000}, *row.DirtyBps)
		assert.Contains(t, []string{"contiguous", "sparse", "fixed-seed-random"}, row.DirtyPattern)
		assert.Equal(t, 3, row.Repeat)
		assert.NotZero(t, stableWorkerSeed(plan.Seed, row.ID))
	}
}

func TestFocusedLifecycleTailPlanSeparatesLifecycleAndConcurrency(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("configs", "focused-lifecycle.json"))
	require.NoError(t, err)
	cfg, err := ParsePlanConfig(raw)
	require.NoError(t, err)

	plan, err := ExpandPlanFromConfig(cfg, PlanExpandOptions{Seed: cfg.Seed})
	require.NoError(t, err)
	require.Len(t, plan.Rows, 8)

	seen := map[string]bool{}
	for _, row := range plan.Rows {
		require.NotNil(t, row.Concurrency)
		seen[string(row.Lifecycle)+"/c"+fmt.Sprint(*row.Concurrency)] = true
		assert.Equal(t, 12, row.Repeat)
	}
	for _, lifecycle := range []Lifecycle{LifecycleFresh, LifecycleSingleUse, LifecycleSnapshotMemcpy, LifecycleSnapshotCow} {
		for _, concurrency := range []int{1, 4} {
			assert.True(t, seen[string(lifecycle)+"/c"+fmt.Sprint(concurrency)])
		}
	}
}

func TestCurrentSourceCanaryPlanIsBoundedAndCoversLifecycleBurstDirtyAndHTTP(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("configs", "current-source-canary.json"))
	require.NoError(t, err)
	cfg, err := ParsePlanConfig(raw)
	require.NoError(t, err)

	plan, err := ExpandPlanFromConfig(cfg, PlanExpandOptions{})
	require.NoError(t, err)
	require.Len(t, plan.Rows, 10)

	lifecycles := map[Lifecycle]bool{}
	burst := 0
	httpRows := 0
	dirtyPoints := map[int]bool{}
	arenas := map[int]bool{}
	for _, row := range plan.Rows {
		assert.Equal(t, "current-canary", row.Campaign)
		lifecycles[row.Lifecycle] = true
		if row.Concurrency != nil && *row.Concurrency == 4 {
			burst++
		}
		if row.Surface == "http" {
			httpRows++
		}
		if row.DirtyBps != nil {
			dirtyPoints[*row.DirtyBps] = true
		}
		if row.ArenaMiB != nil {
			arenas[*row.ArenaMiB] = true
		}
	}
	assert.Equal(t, map[Lifecycle]bool{
		LifecycleFresh: true, LifecycleSingleUse: true,
		LifecycleSnapshotMemcpy: true, LifecycleSnapshotCow: true,
	}, lifecycles)
	assert.Equal(t, 2, burst)
	assert.Equal(t, 2, httpRows)
	assert.Equal(t, map[int]bool{100: true, 1000: true}, dirtyPoints)
	assert.Equal(t, map[int]bool{32: true, 64: true}, arenas)
}

func TestStableWorkerSeedDependsOnPlanSeedAndRowIdentity(t *testing.T) {
	a := stableWorkerSeed(100, "row-a")
	assert.Equal(t, a, stableWorkerSeed(100, "row-a"))
	assert.NotEqual(t, a, stableWorkerSeed(101, "row-a"))
	assert.NotEqual(t, a, stableWorkerSeed(100, "row-b"))
}
