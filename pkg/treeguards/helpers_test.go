package treeguards

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// moduleRoot walks up from the package directory to go.mod, so every guard
// here covers the whole module.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	require.NoError(t, err)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		require.NotEqual(t, dir, parent, "could not find module root")
		dir = parent
	}
}
