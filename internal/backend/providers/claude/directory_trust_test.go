package claude

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDirectoryTrustPreservesNativeStateAndRejectsLossyInput(t *testing.T) {
	original := []byte(`{"counter":9007199254740993,"other":"literal <>&","projects":{"/other":{"hasTrustDialogAccepted":false}}}`)
	changed, out, err := planClaudeDirTrust(original, "/work")
	require.NoError(t, err)
	require.True(t, changed)
	require.Contains(t, string(out), "9007199254740993")
	require.Contains(t, string(out), "literal <>&")
	require.Contains(t, string(out), `"hasTrustDialogAccepted": true`)
	changed, again, err := planClaudeDirTrust(out, "/work")
	require.NoError(t, err)
	require.False(t, changed)
	require.Equal(t, out, again)
	for _, input := range []string{`{"projects":[]}`, `{"projects":{"/work":1}}`, `{"kept":"\ud800"}`} {
		changed, _, err = planClaudeDirTrust([]byte(input), "/work")
		require.Error(t, err)
		require.False(t, changed)
	}
}

func TestDirectoryTrustEditsOnlyExplicitNativeRoot(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()
	path := filepath.Join(root, ".claude.json")
	original := []byte("{\"counter\":9007199254740993}")
	require.NoError(t, os.WriteFile(path, original, 0600))
	require.NoError(t, ensureDirectoryTrusted(root, cwd))
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NotEqual(t, original, after)
	require.NoError(t, ensureDirectoryTrusted(root, cwd))
	again, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, after, again)
	malformed := []byte("{\"projects\":[]}")
	require.NoError(t, os.WriteFile(path, malformed, 0600))
	require.Error(t, ensureDirectoryTrusted(root, cwd))
	again, err = os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, malformed, again)
	require.Error(t, ensureDirectoryTrusted("relative", cwd))
	require.Error(t, ensureDirectoryTrusted(root, "relative"))
}
