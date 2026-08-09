package wasm

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCheckArtifactAcceptsGenericDispatchABI(t *testing.T) {
	report, err := CheckArtifact(context.Background(), ArtifactCheckOptions{
		Profile:    "generic",
		ModulePath: echoModulePath(t),
	})

	require.NoError(t, err)
	assert.Equal(t, "generic", report.Profile)
	assert.Contains(t, report.Exports, "dispatch")
	assert.NotContains(t, report.Exports, "evaluate")
}

func TestCheckArtifactRejectsLegacyBusinessNamedExport(t *testing.T) {
	data, err := os.ReadFile(echoModulePath(t))
	require.NoError(t, err)
	require.Contains(t, string(data), "dispatch")
	data = []byte(replaceEqualLength(string(data), "dispatch", "evaluate"))
	modulePath := filepath.Join(t.TempDir(), "legacy.wasm")
	require.NoError(t, os.WriteFile(modulePath, data, 0o644))

	_, err = CheckArtifact(context.Background(), ArtifactCheckOptions{
		Profile:    "generic",
		ModulePath: modulePath,
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), `required export "dispatch" is missing`)
}

func replaceEqualLength(value, old, replacement string) string {
	if len(old) != len(replacement) {
		panic("replacement must preserve binary length")
	}
	for index := 0; index+len(old) <= len(value); index++ {
		if value[index:index+len(old)] == old {
			return value[:index] + replacement + value[index+len(old):]
		}
	}
	return value
}
