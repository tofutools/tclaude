package agent

import (
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc/agentipctest"
	tcommon "github.com/tofutools/tclaude/pkg/common"
)

func TestAttachHumanTokenPrefersEnvironment(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv(HumanTokenEnvVar, "  tclo_explicit  ")
	t.Setenv(agentipc.AgentHintEnvVar, "")
	writePersistentHumanToken(t, "tclo_persisted")

	req, err := http.NewRequest(http.MethodGet, "http://_/v1/whoami", nil)
	require.NoError(t, err)
	attachCallerIdentity(req)

	assert.Equal(t, "tclo_explicit", req.Header.Get(HumanTokenHeader))
}

func TestAttachHumanTokenFallsBackToPersistentFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv(HumanTokenEnvVar, "")
	t.Setenv(agentipc.AgentHintEnvVar, "")
	writePersistentHumanToken(t, "  tclo_persisted\n")

	req, err := http.NewRequest(http.MethodGet, "http://_/v1/whoami", nil)
	require.NoError(t, err)
	attachCallerIdentity(req)

	assert.Equal(t, "tclo_persisted", req.Header.Get(HumanTokenHeader))
}

func TestAttachHumanTokenUsesLegacyStatePathWithPreSplitDaemon(t *testing.T) {
	home := agentipctest.ShortSocketDir(t)
	t.Setenv("HOME", home)
	t.Setenv(HumanTokenEnvVar, "")
	t.Setenv(agentipc.AgentHintEnvVar, "")

	legacySocket := agentipc.LegacyHomeSocketPath()
	listener, err := net.Listen("unix", legacySocket)
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })

	legacyTokenPath := filepath.Join(tcommon.TclaudeDir(), "operator_token")
	require.NoError(t, os.MkdirAll(filepath.Dir(legacyTokenPath), 0o700))
	require.NoError(t, os.WriteFile(legacyTokenPath, []byte("tclo_legacy\n"), 0o600))

	req, err := http.NewRequest(http.MethodGet, "http://_/v1/whoami", nil)
	require.NoError(t, err)
	attachCallerIdentity(req)

	assert.Equal(t, "tclo_legacy", req.Header.Get(HumanTokenHeader))
}

func TestAttachCallerIdentityAgentHintSkipsPersistentToken(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv(HumanTokenEnvVar, "")
	t.Setenv(agentipc.AgentHintEnvVar, "1")
	writePersistentHumanToken(t, "tclo_must_not_be_read")

	req, err := http.NewRequest(http.MethodGet, "http://_/v1/whoami", nil)
	require.NoError(t, err)
	attachCallerIdentity(req)

	assert.Equal(t, "1", req.Header.Get(agentipc.AgentHintHeader))
	assert.Empty(t, req.Header.Get(HumanTokenHeader))
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
			t.Setenv(agentipc.AgentHintEnvVar, "")
			if tt.setup != nil {
				tt.setup(t)
			}

			req, err := http.NewRequest(http.MethodGet, "http://_/v1/whoami", nil)
			require.NoError(t, err)
			attachCallerIdentity(req)

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
