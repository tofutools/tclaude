package agentd_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// autonomyProbeDir returns a launch directory INSIDE the flow harness's
// isolated HOME, optionally with a project settings.json.
//
// The directory matters: the effective-sandbox check walks up from the launch
// dir looking for project settings and stops at $HOME. A cwd outside the
// isolated home would keep walking into the developer's real directories and
// make the result depend on whose machine ran the test.
func autonomyProbeDir(t *testing.T, projectSettings string) string {
	t.Helper()
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	dir := filepath.Join(home, "probe-repo")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	if projectSettings != "" {
		require.NoError(t, os.MkdirAll(filepath.Join(dir, ".claude"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, ".claude", "settings.json"),
			[]byte(projectSettings), 0o600))
	}
	return dir
}

// The TCL-586 case end to end: an un-chosen Claude spawn resolves to
// `--permission-mode auto`, nothing configures a sandbox, and the operator is
// told — on the wire and in the CLI's own output.
func TestSpawnUnsandboxedAutonomy_WarnsOnDefaultClaudeSpawn(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	resp, out := runSpawnCLI(t, f, &agent.SpawnParams{
		Group: "alpha", Name: "worker", Cwd: autonomyProbeDir(t, ""),
	})

	require.NotEmpty(t, resp.Resolved.Warnings, "a default Claude spawn with no sandbox must warn")
	assert.Contains(t, resp.Resolved.Warnings[0], `permission mode "auto"`)
	assert.Contains(t, resp.Resolved.Warnings[0], "no Claude Code settings file tclaude can see enables")
	assert.Contains(t, out, "Warning: ")
	// The warning must not be filed as a provenance note — the CLI labels the
	// two differently and an operator triaging risk should not have to read
	// past "which tier set the model".
	for _, note := range resp.Resolved.Notes {
		assert.NotContains(t, note, "run commands unattended")
	}
}

// The same spawn is silent once a settings file actually enables the sandbox —
// i.e. the check reads the real files rather than assuming the `inherit`
// default means "unconfined". Using the PROJECT tier also proves the walk from
// the launch directory is wired, not just the user-level file.
func TestSpawnUnsandboxedAutonomy_SilentWhenProjectSandboxEnabled(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	resp, out := runSpawnCLI(t, f, &agent.SpawnParams{
		Group: "alpha", Name: "worker",
		Cwd: autonomyProbeDir(t, `{"sandbox":{"enabled":true}}`),
	})

	assert.Empty(t, resp.Resolved.Warnings)
	assert.NotContains(t, out, "Warning: ")
}

// Forcing the sandbox on for one agent is the per-spawn fix the warning
// recommends, so it must actually silence it.
func TestSpawnUnsandboxedAutonomy_SilentWhenSandboxForcedOn(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	resp, _ := runSpawnCLI(t, f, &agent.SpawnParams{
		Group: "alpha", Name: "worker", Sandbox: harness.ClaudeSandboxOn,
		Cwd: autonomyProbeDir(t, ""),
	})

	assert.Empty(t, resp.Resolved.Warnings)
}

// A posture that keeps a human in the loop for commands is not the pairing this
// warns about, however unconfined the machine is.
func TestSpawnUnsandboxedAutonomy_SilentForNonAutonomousMode(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	resp, _ := runSpawnCLI(t, f, &agent.SpawnParams{
		Group: "alpha", Name: "worker", Approval: "acceptEdits",
		Cwd: autonomyProbeDir(t, ""),
	})

	assert.Empty(t, resp.Resolved.Warnings)
}
