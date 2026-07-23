package agent

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tcommon "github.com/tofutools/tclaude/pkg/common"
)

func TestAttachHumanTokenPrefersEnvironment(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv(HumanTokenEnvVar, "  tclo_explicit  ")
	writePersistentHumanToken(t, "tclo_persisted")

	req, err := http.NewRequest(http.MethodGet, "http://_/v1/whoami", nil)
	require.NoError(t, err)
	attachHumanToken(req)

	assert.Equal(t, "tclo_explicit", req.Header.Get(HumanTokenHeader))
}

func TestAttachHumanTokenFallsBackToPersistentFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv(HumanTokenEnvVar, "")
	writePersistentHumanToken(t, "  tclo_persisted\n")

	req, err := http.NewRequest(http.MethodGet, "http://_/v1/whoami", nil)
	require.NoError(t, err)
	attachHumanToken(req)

	assert.Equal(t, "tclo_persisted", req.Header.Get(HumanTokenHeader))
}

func TestAttachHumanTokenIgnoresUnavailableOrEmptyPersistentFile(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T)
	}{
		{name: "missing"},
		{
			name: "unreadable",
			setup: func(t *testing.T) {
				path := filepath.Join(tcommon.TclaudeDataDir(), "operator_token")
				require.NoError(t, os.MkdirAll(path, 0o700))
			},
		},
		{
			name: "empty",
			setup: func(t *testing.T) {
				writePersistentHumanToken(t, " \n\t")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv(HumanTokenEnvVar, "")
			if tt.setup != nil {
				tt.setup(t)
			}

			req, err := http.NewRequest(http.MethodGet, "http://_/v1/whoami", nil)
			require.NoError(t, err)
			attachHumanToken(req)

			assert.Empty(t, req.Header.Get(HumanTokenHeader))
		})
	}
}

func writePersistentHumanToken(t *testing.T, token string) {
	t.Helper()
	path := filepath.Join(tcommon.TclaudeDataDir(), "operator_token")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(token), 0o600))
}
