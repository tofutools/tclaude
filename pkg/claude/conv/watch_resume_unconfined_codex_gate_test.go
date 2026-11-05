package conv

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// A managed Codex resume recorded with an unconfined implementation (off,
// resource-only) still writes a managed profile, and runs the same
// reopen-under-deny gate `session new` runs on it (awb tcl-jyd8zy). The
// resume's own gate in planResumeDirs is skipped for an unconfined
// implementation, so before the gate moved into the shared profile step this
// launch rendered a reopen beneath a deny that Codex cannot honour on macOS.
func TestResumeUnconfinedCodexManagedProfileRunsTheReopenGate(t *testing.T) {
	// A PATH with git but no Codex, so the split-policy probe cannot verify:
	// the refusal is then the gate's own, whatever the host has installed.
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is required")
	}
	bin := t.TempDir()
	require.NoError(t, os.Symlink(git, filepath.Join(bin, "git")))
	t.Setenv("PATH", bin)
	for _, implementation := range []sandboxpolicy.Implementation{
		sandboxpolicy.ImplementationOff, sandboxpolicy.ImplementationResourceOnly,
	} {
		t.Run(string(implementation), func(t *testing.T) {
			setupTestDB(t)
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("CODEX_HOME", t.TempDir())
			canonicalHome, err := filepath.EvalSymlinks(home)
			require.NoError(t, err)
			workspace := filepath.Join(home, "plain-workspace")
			require.NoError(t, os.MkdirAll(workspace, 0o755))

			effective, err := sandboxpolicy.Resolve(sandboxpolicy.Scopes{Explicit: &sandboxpolicy.Profile{
				Name: "deny-home", Filesystem: []sandboxpolicy.FilesystemGrant{
					{Path: canonicalHome, Access: sandboxpolicy.AccessDeny},
				},
			}})
			require.NoError(t, err)
			snapshot := sandboxpolicy.NewSnapshot(effective, nil)
			agentID, _, err := db.EnsureAgentForConv(resumeConvCodex, "test")
			require.NoError(t, err)
			require.NoError(t, db.SetAgentEffectiveSandboxConfig(agentID, &snapshot))
			require.NoError(t, db.SaveSession(&db.SessionRow{
				ID: "source-session", ConvID: resumeConvCodex, Harness: harness.CodexName,
				Cwd: workspace, HarnessBuiltinMode: harness.SandboxManagedProfile,
				SandboxImplementation: string(implementation),
			}))

			_, _, _, err = resumeLaunchCmd(harness.CodexName, resumeConvCodex[:8], resumeConvCodex, nil)
			require.Error(t, err)
			var capabilityErr *harness.SandboxCapabilityError
			require.ErrorAs(t, err, &capabilityErr, "the reopen gate refuses, as session new does")
			assert.Equal(t, harness.SandboxCapabilityReopenUnderDeny, capabilityErr.Kind)
		})
	}
}
