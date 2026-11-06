package agentd

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A spawnFailure is written through writeSpawnFailure, not field by field: the
// helper is what keeps a failure whose response is already written
// (errSpawnResponseWritten) from being answered twice (awb tcl-x3gmao).
func TestSpawnFailuresAreWrittenThroughTheHelper(t *testing.T) {
	inline := regexp.MustCompile(`writeError\(\w+, (\w+)\.Status, (\w+)\.Kind, (\w+)\.Msg\)`)
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		src, err := os.ReadFile(file)
		require.NoError(t, err)
		matches := inline.FindAllStringSubmatch(string(src), -1)
		if file == "lifecycle_spawn_profile.go" && len(matches) > 0 && matches[0][1] == "fail" {
			matches = matches[1:] // writeSpawnFailure's own body; any other is a finding
		}
		for _, m := range matches {
			assert.Failf(t, "inline spawnFailure write", "%s: %s; use writeSpawnFailure(w, %s)", file, m[0], m[1])
		}
	}
}

func TestWriteSpawnFailure(t *testing.T) {
	rec := httptest.NewRecorder()
	writeSpawnFailure(rec, &spawnFailure{http.StatusForbidden, "permission", "no"})
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), `"permission"`)

	rec = httptest.NewRecorder()
	writeSpawnFailure(rec, errSpawnResponseWritten)
	assert.Empty(t, rec.Body.String(), "a failure already answered writes nothing more")
	assert.False(t, rec.Flushed)
}
