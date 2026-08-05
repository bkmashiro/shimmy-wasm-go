package main

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPlanStrictDecoderRejectsUnknownField(t *testing.T) {
	raw := []byte(`{
		"schema": "agent-python-ultimate-plan/v1",
		"rows": [{
			"row_id": "r-001",
			"campaign": "capability",
			"lifecycle": "fresh",
			"pool": 1,
			"prepared_capacity": 1,
			"repeat": 1,
			"unexpected": true
		}]
	}`)
	_, err := ParsePlanJSON(raw)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown field")
}

func TestPlanStrictDecoderRejectsTrailingJSON(t *testing.T) {
	raw := []byte(`{"schema":"agent-python-ultimate-plan/v1","rows":[]}{"extra":1}`)
	_, err := ParsePlanJSON(raw)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "trailing")
}

func TestPlanValidationRejectsInvalidLifecycle(t *testing.T) {
	raw := []byte(`{
		"schema":"agent-python-ultimate-plan/v1",
		"rows":[{
			"row_id":"r-001",
			"campaign":"capability",
			"lifecycle":"invalid",
			"pool":1,
			"prepared_capacity":1,
			"repeat":1
		}]
	}`)
	_, err := ParsePlanJSON(raw)
	require.Error(t, err)
}

func TestPlanValidationRejectsCapacityOutOfRange(t *testing.T) {
	raw := []byte(`{
		"schema":"agent-python-ultimate-plan/v1",
		"rows":[{
			"row_id":"r-001",
			"campaign":"capability",
			"lifecycle":"fresh",
			"pool":0,
			"prepared_capacity":1,
			"repeat":1
		}]
	}`)
	_, err := ParsePlanJSON(raw)
	require.Error(t, err)

	raw2 := []byte(`{
		"schema":"agent-python-ultimate-plan/v1",
		"rows":[{
			"row_id":"r-001",
			"campaign":"capability",
			"lifecycle":"fresh",
			"pool":1,
			"prepared_capacity":5,
			"repeat":1
		}]
	}`)
	_, err = ParsePlanJSON(raw2)
	require.Error(t, err)
}

func TestPlanValidationRejectsDuplicateRowID(t *testing.T) {
	raw := []byte(`{
		"schema":"agent-python-ultimate-plan/v1",
		"rows":[
			{"row_id":"dup", "campaign":"capability", "lifecycle":"fresh", "pool":1, "prepared_capacity":1, "repeat":1},
			{"row_id":"dup", "campaign":"capability", "lifecycle":"single-use", "pool":1, "prepared_capacity":1, "repeat":1}
		]
	}`)
	_, err := ParsePlanJSON(raw)
	require.Error(t, err)
}

func TestPlanValidationRejectsPayloadOverflow(t *testing.T) {
	raw := []byte(`{
		"schema":"agent-python-ultimate-plan/v1",
		"rows":[{
			"row_id":"r-001",
			"campaign":"input-payload",
			"lifecycle":"fresh",
			"pool":1,
			"prepared_capacity":1,
			"repeat":1,
			"input_bytes":1048577
		}]
	}`)
	_, err := ParsePlanJSON(raw)
	require.Error(t, err)
}

func TestPlanValidationRejectsInvalidDirtyBps(t *testing.T) {
	raw := []byte(`{
		"schema":"agent-python-ultimate-plan/v1",
		"rows":[{
			"row_id":"r-001",
			"campaign":"dirty-pattern",
			"lifecycle":"fresh",
			"pool":1,
			"prepared_capacity":1,
			"repeat":1,
			"dirty_bps":10001
		}]
	}`)
	_, err := ParsePlanJSON(raw)
	require.Error(t, err)
}

func TestPlanValidationRejectsFallbackImpersonatingSelected(t *testing.T) {
	raw := []byte(`{
		"schema":"agent-python-ultimate-plan/v1",
		"rows":[{
			"row_id":"r-001",
			"campaign":"capability",
			"lifecycle":"snapshot/cow",
			"pool":1,
			"prepared_capacity":1,
			"repeat":1,
			"snapshot_selected":"cow",
			"snapshot_fallback":true
		}]
	}`)
	_, err := ParsePlanJSON(raw)
	require.Error(t, err)
}

func TestPlanValidationAcceptsCanonicalRows(t *testing.T) {
	in := 256
	out := 128
	dirty := 10
	row := PlanRow{
		ID:               "r-001",
		Campaign:         "capability",
		Lifecycle:        LifecycleFresh,
		Pool:             1,
		PreparedCapacity: 1,
		Repeat:           1,
		InputBytes:       &in,
		OutputBytes:      &out,
		DirtyBps:         &dirty,
		PayloadShape:     "flat-ascii",
	}
	plan := Plan{Schema: PlanSchemaVersion, Seed: 1, Rows: []PlanRow{row}}
	b, err := json.Marshal(plan)
	require.NoError(t, err)
	parsed, err := ParsePlanJSON(b)
	require.NoError(t, err)
	assert.Equal(t, PlanSchemaVersion, parsed.Schema)
	assert.Len(t, parsed.Rows, 1)
	assert.Equal(t, "r-001", parsed.Rows[0].ID)
}

func TestReportValidationRejectsSummaryMismatch(t *testing.T) {
	raw := []byte(`{
		"schema":"agent-python-ultimate-report/v1",
		"rows":[{
			"row_id":"r-001",
			"campaign":"capability",
			"lifecycle":"fresh",
			"raw_entries":[{"a":1}],
			"summary":{"raw_count":0}
		}]
	}`)
	_, err := ParseReportJSON(raw)
	require.Error(t, err)
}

func TestReportValidationRejectsFallbackImpersonatingSelected(t *testing.T) {
	raw := []byte(`{
		"schema":"agent-python-ultimate-report/v1",
		"rows":[{
			"row_id":"r-001",
			"campaign":"capability",
			"lifecycle":"snapshot/cow",
			"snapshot_requested":"snapshot/cow",
			"snapshot_selected":"cow",
			"snapshot_fallback":true,
			"raw_entries":[],
			"summary":{"raw_count":0}
		}]
	}`)
	_, err := ParseReportJSON(raw)
	require.Error(t, err)
}

func TestCanonicalRowIDIsStable(t *testing.T) {
	in := 256
	raw := PlanRow{
		Campaign:         "capability",
		Lifecycle:        LifecycleFresh,
		Pool:             1,
		PreparedCapacity: 1,
		Repeat:           1,
		InputBytes:       &in,
	}
	id1 := CanonicalRowID(raw, 0)
	id2 := CanonicalRowID(raw, 0)
	assert.Equal(t, id1, id2)
	id3 := CanonicalRowID(raw, 1)
	assert.NotEqual(t, id1, id3)
}

func TestCanonicalRowIDIncludesBehaviorFields(t *testing.T) {
	base := PlanRow{Campaign: "fault-recovery", Lifecycle: LifecycleFresh, Pool: 1, PreparedCapacity: 1, Repeat: 1, Surface: "direct", CacheState: "warm", Fault: "timeout"}
	baseID := CanonicalRowID(base, 0)
	for name, mutate := range map[string]func(*PlanRow){
		"surface": func(row *PlanRow) { row.Surface = "http" },
		"cache":   func(row *PlanRow) { row.CacheState = "cold" },
		"fault":   func(row *PlanRow) { row.Fault = "cancel" },
	} {
		t.Run(name, func(t *testing.T) {
			changed := base
			mutate(&changed)
			assert.NotEqual(t, baseID, CanonicalRowID(changed, 0))
		})
	}
}
