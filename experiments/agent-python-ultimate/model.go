package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

const (
	PlanSchemaVersion   = "agent-python-ultimate-plan/v1"
	ReportSchemaVersion = "agent-python-ultimate-report/v1"

	MinCapacity = 1
	MaxCapacity = 4

	MaxPayloadBytes = 1 << 20
)

var payloadLimitsByCampaign = map[string]int{
	"payload": MaxPayloadBytes,
}

var validFaults = map[string]struct{}{
	"exception": {}, "timeout": {}, "cancel": {}, "memory-growth": {},
	"oversized-input": {}, "oversized-output": {}, "recovery": {},
}

// Lifecycle is the execution lifecycle for a benchmark row.
type Lifecycle string

const (
	LifecycleFresh          Lifecycle = "fresh"
	LifecycleSingleUse      Lifecycle = "single-use"
	LifecycleSnapshotMemcpy Lifecycle = "snapshot/memcpy"
	LifecycleSnapshotCow    Lifecycle = "snapshot/cow"
)

var canonicalLifecycles = map[Lifecycle]struct{}{
	LifecycleFresh:          {},
	LifecycleSingleUse:      {},
	LifecycleSnapshotMemcpy: {},
	LifecycleSnapshotCow:    {},
}

// PlanRow models one row in the ultimate benchmark plan.
type PlanRow struct {
	ID               string    `json:"row_id"`
	Campaign         string    `json:"campaign"`
	Lifecycle        Lifecycle `json:"lifecycle"`
	Pool             int       `json:"pool"`
	PreparedCapacity int       `json:"prepared_capacity"`
	Repeat           int       `json:"repeat"`
	InputBytes       *int      `json:"input_bytes,omitempty"`
	OutputBytes      *int      `json:"output_bytes,omitempty"`
	PayloadShape     string    `json:"payload_shape,omitempty"`
	ArenaMiB         *int      `json:"arena_mib,omitempty"`
	DirtyBps         *int      `json:"dirty_bps,omitempty"`
	DirtyPattern     string    `json:"dirty_pattern,omitempty"`
	CPUProfile       string    `json:"cpu_profile,omitempty"`
	Concurrency      *int      `json:"concurrency,omitempty"`
	CacheState       string    `json:"cache_state,omitempty"`
	Surface          string    `json:"surface,omitempty"`
	Fault            string    `json:"fault,omitempty"`
	SnapshotSelected string    `json:"snapshot_selected,omitempty"`
	SnapshotFallback bool      `json:"snapshot_fallback,omitempty"`
}

// Plan is the full expanded plan used by benchmark generation.
type Plan struct {
	Schema string    `json:"schema"`
	Seed   int64     `json:"seed,omitempty"`
	Rows   []PlanRow `json:"rows"`
}

// ReportSummary captures the count-level summary for a row result.
type ReportSummary struct {
	RawCount int `json:"raw_count"`
}

// ReportRow models one row result in benchmark report.
type ReportRow struct {
	RowID             string            `json:"row_id"`
	Campaign          string            `json:"campaign"`
	Lifecycle         Lifecycle         `json:"lifecycle"`
	SnapshotRequested string            `json:"snapshot_requested,omitempty"`
	SnapshotSelected  string            `json:"snapshot_selected,omitempty"`
	SnapshotFallback  bool              `json:"snapshot_fallback,omitempty"`
	RawEntries        []json.RawMessage `json:"raw_entries"`
	Summary           ReportSummary     `json:"summary"`
}

// Report models benchmark execution report input for downstream validation.
type Report struct {
	Schema string      `json:"schema"`
	Rows   []ReportRow `json:"rows"`
}

func ParsePlanJSON(raw []byte) (*Plan, error) {
	var plan Plan
	if err := decodeStrictJSON(raw, &plan); err != nil {
		return nil, err
	}
	if err := ValidatePlan(&plan); err != nil {
		return nil, err
	}
	return &plan, nil
}

func ParseReportJSON(raw []byte) (*Report, error) {
	var report Report
	if err := decodeStrictJSON(raw, &report); err != nil {
		return nil, err
	}
	if err := ValidateReport(&report); err != nil {
		return nil, err
	}
	return &report, nil
}

func ValidatePlan(plan *Plan) error {
	if plan == nil {
		return fmt.Errorf("plan is nil")
	}
	if plan.Schema == "" {
		return fmt.Errorf("plan.schema is required")
	}
	if plan.Schema != PlanSchemaVersion {
		return fmt.Errorf("unsupported plan schema %q", plan.Schema)
	}
	if plan.Seed <= 0 {
		return fmt.Errorf("plan.seed must be positive")
	}
	ids := map[string]struct{}{}

	for i := range plan.Rows {
		row := &plan.Rows[i]
		if row.ID == "" {
			return fmt.Errorf("row_id is required")
		}
		if row.Campaign == "" {
			return fmt.Errorf("campaign is required for row %s", row.ID)
		}
		if _, ok := ids[row.ID]; ok {
			return fmt.Errorf("duplicate row_id %q", row.ID)
		}
		ids[row.ID] = struct{}{}

		if _, ok := canonicalLifecycles[row.Lifecycle]; !ok {
			return fmt.Errorf("invalid lifecycle %q in row %s", row.Lifecycle, row.ID)
		}
		if row.Pool < MinCapacity || row.Pool > MaxCapacity {
			return fmt.Errorf("pool %d out of range for row %s", row.Pool, row.ID)
		}
		if row.PreparedCapacity < MinCapacity || row.PreparedCapacity > MaxCapacity {
			return fmt.Errorf("prepared_capacity %d out of range for row %s", row.PreparedCapacity, row.ID)
		}
		if row.Repeat < 1 {
			return fmt.Errorf("repeat must be positive for row %s", row.ID)
		}

		if err := validatePayloadBytes(row.ID, row.InputBytes, row.OutputBytes); err != nil {
			return err
		}

		if row.DirtyBps != nil {
			if *row.DirtyBps < 0 || *row.DirtyBps > 10000 {
				return fmt.Errorf("dirty_bps %d out of range for row %s", *row.DirtyBps, row.ID)
			}
		}
		if row.Concurrency != nil && (*row.Concurrency < 1 || *row.Concurrency > 16) {
			return fmt.Errorf("concurrency %d out of range for row %s", *row.Concurrency, row.ID)
		}
		if row.ArenaMiB != nil && (*row.ArenaMiB < 0 || *row.ArenaMiB > 128) {
			return fmt.Errorf("arena_mib %d out of range for row %s", *row.ArenaMiB, row.ID)
		}
		if row.CacheState != "" && row.CacheState != "cold" && row.CacheState != "warm" {
			return fmt.Errorf("invalid cache_state %q for row %s", row.CacheState, row.ID)
		}
		if row.Surface != "" && row.Surface != "direct" && row.Surface != "http" {
			return fmt.Errorf("invalid surface %q for row %s", row.Surface, row.ID)
		}
		if row.Fault != "" {
			if _, ok := validFaults[row.Fault]; !ok {
				return fmt.Errorf("invalid fault %q for row %s", row.Fault, row.ID)
			}
		}
		if row.PayloadShape != "" && !containsString(PayloadShapeValues, row.PayloadShape) {
			return fmt.Errorf("invalid payload_shape %q for row %s", row.PayloadShape, row.ID)
		}
		if row.DirtyPattern != "" && !containsString(dirtyPagePatterns, row.DirtyPattern) {
			return fmt.Errorf("invalid dirty_pattern %q for row %s", row.DirtyPattern, row.ID)
		}
		if row.CPUProfile != "" {
			if _, ok := KnownCPUProfiles[row.CPUProfile]; !ok {
				return fmt.Errorf("invalid cpu_profile %q for row %s", row.CPUProfile, row.ID)
			}
		}

		if err := validateSnapshotConsistency(row.ID, row.Lifecycle, row.SnapshotSelected, row.SnapshotFallback); err != nil {
			return err
		}
	}
	return nil
}

func ValidateReport(report *Report) error {
	if report == nil {
		return fmt.Errorf("report is nil")
	}
	if report.Schema == "" {
		return fmt.Errorf("report.schema is required")
	}
	if report.Schema != ReportSchemaVersion {
		return fmt.Errorf("unsupported report schema %q", report.Schema)
	}
	ids := map[string]struct{}{}
	for _, row := range report.Rows {
		if row.RowID == "" {
			return fmt.Errorf("row_id is required")
		}
		if _, ok := ids[row.RowID]; ok {
			return fmt.Errorf("duplicate row_id %q", row.RowID)
		}
		ids[row.RowID] = struct{}{}

		if row.Campaign == "" {
			return fmt.Errorf("campaign is required for row %s", row.RowID)
		}
		if _, ok := canonicalLifecycles[row.Lifecycle]; !ok {
			return fmt.Errorf("invalid lifecycle %q in row %s", row.Lifecycle, row.RowID)
		}
		if row.Summary.RawCount < 0 {
			return fmt.Errorf("summary.raw_count must be non-negative for row %s", row.RowID)
		}
		if len(row.RawEntries) != row.Summary.RawCount {
			return fmt.Errorf("summary.raw_count %d does not match raw_entries length %d for row %s", row.Summary.RawCount, len(row.RawEntries), row.RowID)
		}
		if err := validateSnapshotConsistency(row.RowID, row.Lifecycle, row.SnapshotSelected, row.SnapshotFallback); err != nil {
			return err
		}
		if row.Lifecycle != LifecycleSnapshotMemcpy && row.Lifecycle != LifecycleSnapshotCow {
			if row.SnapshotFallback {
				return fmt.Errorf("snapshot_fallback set for non-snapshot row %s", row.RowID)
			}
			if row.SnapshotSelected != "" || row.SnapshotRequested != "" {
				return fmt.Errorf("snapshot fields present for non-snapshot row %s", row.RowID)
			}
		}
	}
	return nil
}

func validateSnapshotConsistency(rowID string, lifecycle Lifecycle, selected string, fallback bool) error {
	onSnapshot := lifecycle == LifecycleSnapshotMemcpy || lifecycle == LifecycleSnapshotCow
	if !onSnapshot {
		if selected != "" {
			return fmt.Errorf("snapshot_selected set on non-snapshot row %s", rowID)
		}
		if fallback {
			return fmt.Errorf("snapshot_fallback set on non-snapshot row %s", rowID)
		}
		return nil
	}

	required := ""
	switch lifecycle {
	case LifecycleSnapshotMemcpy:
		required = "memcpy"
	case LifecycleSnapshotCow:
		required = "cow"
	}

	if selected == "" {
		return fmt.Errorf("snapshot_selected is required for snapshot row %s", rowID)
	}
	if fallback {
		if selected == required {
			return fmt.Errorf("snapshot_fallback must not preserve requested snapshot for row %s", rowID)
		}
		return nil
	}
	if selected != required {
		return fmt.Errorf("snapshot_selected %q mismatches lifecycle %q for row %s", selected, lifecycle, rowID)
	}
	return nil
}

func validatePayloadBytes(rowID string, in *int, out *int) error {
	if in != nil {
		if *in < 0 {
			return fmt.Errorf("input_bytes must be non-negative for row %s", rowID)
		}
		if *in > MaxPayloadBytes {
			return fmt.Errorf("input_bytes %d exceeds max payload %d for row %s", *in, MaxPayloadBytes, rowID)
		}
		if _, ok := payloadLimitsByCampaign["payload"]; ok && *in > payloadLimitsByCampaign["payload"] {
			return fmt.Errorf("input_bytes %d exceeds configured limit for row %s", *in, rowID)
		}
	}
	if out != nil {
		if *out < 0 {
			return fmt.Errorf("output_bytes must be non-negative for row %s", rowID)
		}
		if *out > MaxPayloadBytes {
			return fmt.Errorf("output_bytes %d exceeds max payload %d for row %s", *out, MaxPayloadBytes, rowID)
		}
		if _, ok := payloadLimitsByCampaign["payload"]; ok && *out > payloadLimitsByCampaign["payload"] {
			return fmt.Errorf("output_bytes %d exceeds configured limit for row %s", *out, rowID)
		}
	}
	return nil
}

func decodeStrictJSON(raw []byte, target any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		return err
	}
	if err := expectEOF(dec); err != nil {
		return err
	}
	return nil
}

func expectEOF(dec *json.Decoder) error {
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return fmt.Errorf("trailing JSON content")
		}
		return fmt.Errorf("invalid trailing JSON: %w", err)
	}
	return nil
}

func CanonicalRowID(row PlanRow, seq int) string {
	row.ID = ""
	encoded, err := json.Marshal(struct {
		Sequence int     `json:"sequence"`
		Row      PlanRow `json:"row"`
	}{Sequence: seq, Row: row})
	if err != nil {
		panic(fmt.Sprintf("canonical row identity: %v", err))
	}
	h := sha1.Sum(encoded)
	return strings.ToLower(fmt.Sprintf("%s-%s", row.Campaign, hex.EncodeToString(h[:4])))
}

func SortPlanRows(rows []PlanRow) {
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Campaign == rows[j].Campaign {
			if rows[i].Lifecycle == rows[j].Lifecycle {
				if rows[i].Pool == rows[j].Pool {
					if rows[i].PreparedCapacity == rows[j].PreparedCapacity {
						return rows[i].ID < rows[j].ID
					}
					return rows[i].PreparedCapacity < rows[j].PreparedCapacity
				}
				return rows[i].Pool < rows[j].Pool
			}
			return rows[i].Lifecycle < rows[j].Lifecycle
		}
		return rows[i].Campaign < rows[j].Campaign
	})
}
