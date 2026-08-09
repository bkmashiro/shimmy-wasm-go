package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSyntheticPayloadHasExactTargetLength(t *testing.T) {
	for _, target := range InputPayloadTargets {
		for _, shape := range PayloadShapeValues {
			b, err := BuildInputPayloadByShape(shape, target)
			require.NoError(t, err)
			require.Equal(t, target, len(b))
		}
	}

	for _, target := range OutputPayloadTargets {
		for _, shape := range PayloadShapeValues {
			b, err := BuildOutputPayloadByShape(shape, target)
			require.NoError(t, err)
			require.Equal(t, target, len(b))
		}
	}
}

func TestSyntheticPayloadsAreValidJSON(t *testing.T) {
	for _, targets := range [][]int{InputPayloadTargets, OutputPayloadTargets} {
		for _, target := range targets {
			for _, shape := range PayloadShapeValues {
				payload, err := BuildSyntheticPayload(target, shape, 0)
				require.NoError(t, err)
				var decoded any
				require.NoErrorf(t, json.Unmarshal(payload, &decoded), "shape=%s target=%d", shape, target)
			}
		}
	}
}

func TestSyntheticPayloadBoundaryLimits(t *testing.T) {
	_, err := BuildInputPayloadByShape(PayloadShapeFlatASCII, maxInputPayloadBytes+1)
	require.Error(t, err)

	_, err = BuildOutputPayloadByShape(PayloadShapeFlatASCII, maxOutputPayloadBytes+1)
	require.Error(t, err)

	near, err := BuildInputPayloadByShape(PayloadShapeNested, 1044480)
	require.NoError(t, err)
	assert.Equal(t, 1044480, len(near))

	over, err := BuildOutputPayloadByShape(PayloadShapeUTF8, 921601)
	require.Error(t, err)
	assert.Nil(t, over)
}

func TestDirtyPagePatternsAreDeterministic(t *testing.T) {
	pages, err := DirtyPages(8, 10000, "contiguous", 42)
	require.NoError(t, err)
	assert.Len(t, pages, 2048)
	assert.Equal(t, 0, pages[0])
	assert.Equal(t, 2047, pages[len(pages)-1])

	pagesSparse, err := DirtyPages(8, 10000, "sparse", 42)
	require.NoError(t, err)
	assert.Len(t, pagesSparse, 2048)

	p1, err := DirtyPages(64, 5000, "fixed-seed-random", 123)
	require.NoError(t, err)
	p2, err := DirtyPages(64, 5000, "fixed-seed-random", 123)
	require.NoError(t, err)
	assert.Equal(t, p1, p2)

	_, err = DirtyPages(64, 10001, "contiguous", 123)
	require.Error(t, err)
}

func TestCPUProfileChecksumDeterministic(t *testing.T) {
	checksum1, iterations1, err := CPUChecksum("python-100k", 123)
	require.NoError(t, err)
	checksum2, iterations2, err := CPUChecksum("python-100k", 123)
	require.NoError(t, err)
	assert.Equal(t, checksum1, checksum2)
	assert.Equal(t, 100000, iterations1)
	assert.Equal(t, iterations1, iterations2)

	_, _, err = CPUChecksum("invalid", 123)
	require.Error(t, err)
}

func TestEvaluatorTemplateEmbeddedOutsideSourceTree(t *testing.T) {
	t.Chdir(t.TempDir())
	fixture, err := RenderEvaluatorTemplate(EvaluatorTemplateData{ArenaMiB: 1, Seed: 7, CPUProfile: "none"})
	require.NoError(t, err)
	assert.Contains(t, fixture, "def dispatch(method, payload):")
	assert.NotContains(t, fixture, "def evaluation_function")
}

func TestEvaluatorTemplateContainsFixtureContractMarkers(t *testing.T) {
	fixture, err := RenderEvaluatorTemplate(EvaluatorTemplateData{
		ArenaMiB:   8,
		Sentinel:   0xd3,
		Seed:       12345,
		CPUProfile: "python-1m",
	})
	require.NoError(t, err)
	assert.Contains(t, fixture, "guest_invocation_count")
	assert.Contains(t, fixture, "def dispatch(method, payload):")
	assert.Contains(t, fixture, `if method != "eval":`)
	assert.NotContains(t, fixture, "def runtime_prepare")
	assert.Contains(t, fixture, "arena_digest")
	assert.Contains(t, fixture, "output_digest")
	assert.Contains(t, fixture, "dirty_pages")
	assert.Contains(t, fixture, "fixed-seed-random")
	assert.True(t, strings.Contains(fixture, "PREPARED_SENTINEL"))
}
