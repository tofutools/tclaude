package conv

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// A resumed managed Codex profile renders the recorded network posture and
// write grants, as the fresh launch's profile does: the resume and
// `session new` build it through the same session step.
//
// No network has a platform-specific shape. On Linux it is refused, because
// Codex's restricted-network seccomp would also block the agentd Unix socket.
// On macOS it is rendered as network enabled with Codex's managed network
// proxy switched on and no domain allowlist: the Unix-socket allows need the
// network switch on, and the proxy's empty, deny-by-default domain policy is
// what keeps every other destination denied (codexAgentProfileContentForRules).
func TestResumeCodexManagedProfileKeepsNetworkPostureAndGrants(t *testing.T) {
	for _, tc := range []struct {
		name    string
		access  sandboxpolicy.NetworkAccess
		enabled string
	}{
		// On macOS; Linux refuses it (below).
		{"no network", sandboxpolicy.NetworkAccessNone, "enabled = true"},
		{"internet", sandboxpolicy.NetworkAccessInternet, "enabled = true"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupTestDB(t)
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("CODEX_HOME", t.TempDir())
			workspace := filepath.Join(home, "plain-workspace")
			require.NoError(t, os.MkdirAll(workspace, 0o755))
			writeDir := filepath.Join(home, "scratch")
			require.NoError(t, os.MkdirAll(writeDir, 0o755))
			canonicalWrite, err := filepath.EvalSymlinks(writeDir)
			require.NoError(t, err)

			effective, err := sandboxpolicy.Resolve(sandboxpolicy.Scopes{Explicit: &sandboxpolicy.Profile{
				Name: "codex-resume-network", NetworkAccess: tc.access,
				Filesystem: []sandboxpolicy.FilesystemGrant{{Path: canonicalWrite, Access: sandboxpolicy.AccessWrite}},
			}})
			require.NoError(t, err)
			snapshot := sandboxpolicy.NewSnapshot(effective, nil)
			agentID, _, err := db.EnsureAgentForConv(resumeConvCodex, "test")
			require.NoError(t, err)
			require.NoError(t, db.SetAgentEffectiveSandboxConfig(agentID, &snapshot))
			require.NoError(t, db.SaveSession(&db.SessionRow{
				ID: "source-session", ConvID: resumeConvCodex, Harness: harness.CodexName,
				Cwd: workspace, HarnessBuiltinMode: harness.SandboxManagedProfile,
			}))

			cmd, path, _, err := resumeLaunchCmd(harness.CodexName, resumeConvCodex[:8], resumeConvCodex, nil)
			if tc.access == sandboxpolicy.NetworkAccessNone && runtime.GOOS == "linux" {
				// Codex's restricted-network seccomp would also block agentd.
				require.ErrorContains(t, err, "unsupported_sandbox_profile_network")
				return
			}
			require.NoError(t, err)
			require.NotEmpty(t, path, "a managed Codex resume writes a launch-unique profile")
			assert.Contains(t, cmd, " -p tclaude-agent-")
			assert.Contains(t, cmd, path, "the pane removes the profile it was launched with")
			raw, err := os.ReadFile(path)
			require.NoError(t, err)
			content := string(raw)
			assert.Regexp(t, `\.network\]\n`+tc.enabled+`\n`, content)
			if tc.access == sandboxpolicy.NetworkAccessNone {
				require.Equal(t, "darwin", runtime.GOOS, "only macOS renders an offline Codex profile")
				assert.Contains(t, content, "network_proxy = true",
					"offline on macOS routes through Codex's managed proxy")
				assert.NotContains(t, content, ".network.domains]",
					"the proxy keeps its empty, deny-by-default domain policy: no destination is allowed")
				assert.Regexp(t, `\.network\.unix_sockets\]\n"[^"]+" = "allow"`, content,
					"the network switch is on for the agentd Unix socket")
			}
			assert.Contains(t, content, `"`+canonicalWrite+`" = "write"`)
		})
	}
}
