package server

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAPIEndpointRefusesUnrelatedFilesWithoutRemovingThem(t *testing.T) {
	for _, kind := range []string{"legacy-file", "legacy-link", "directory-link", "directory-entry"} {
		t.Run(kind, func(t *testing.T) {
			state := t.TempDir()
			retained := filepath.Join(state, "retained")
			require.NoError(t, os.WriteFile(retained, []byte("retained state"), 0600))
			switch kind {
			case "legacy-file":
				require.NoError(t, os.WriteFile(filepath.Join(state, "api.sock"), []byte("unrelated"), 0600))
			case "legacy-link":
				require.NoError(t, os.Symlink(retained, filepath.Join(state, "api.sock")))
			case "directory-link":
				require.NoError(t, os.Symlink(state, AgentSocketDirectory(state)))
			case "directory-entry":
				require.NoError(t, os.Mkdir(AgentSocketDirectory(state), 0700))
				require.NoError(t, os.WriteFile(filepath.Join(AgentSocketDirectory(state), "other"), []byte("unrelated"), 0600))
			}
			_, err := prepareAPIEndpoint(state)
			require.Error(t, err)
			data, err := os.ReadFile(retained)
			require.NoError(t, err)
			require.Equal(t, "retained state", string(data))
			if kind == "legacy-file" {
				data, err = os.ReadFile(filepath.Join(state, "api.sock"))
				require.NoError(t, err)
				require.Equal(t, "unrelated", string(data))
			}
			if kind == "directory-entry" {
				require.FileExists(t, filepath.Join(AgentSocketDirectory(state), "other"))
			}
		})
	}
}
