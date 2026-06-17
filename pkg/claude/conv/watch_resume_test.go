package conv

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// JOH-217 — the watch-mode resume (createSessionForConv) used to hardcode
// `claude --resume <id>`, so selecting a Codex conv in `conv ls -w` spawned
// `claude --resume <codex-id>`, which Claude Code can't resume → broken. These
// pin resumeLaunchCmd, the seam createSessionForConv now routes through: it must
// resolve the conv's recorded harness and build the launch command via that
// harness's Spawner, the same way the web-dashboard / agentd resume path does.

const (
	resumeConvClaude = "abcd1234-0000-0000-0000-000000000001"
	resumeConvCodex  = "019ec004-4250-79b1-9ade-ebaea4135453"
)

// A Codex conv must resume with Codex's `resume <id>` SUBCOMMAND form, never
// the Claude `--resume` flag — the exact regression JOH-217 fixes.
func TestResumeLaunchCmd_Codex(t *testing.T) {
	cmd, h, err := resumeLaunchCmd("codex", resumeConvCodex[:8], resumeConvCodex, nil)
	require.NoError(t, err)
	require.NotNil(t, h)

	assert.Equal(t, "codex", h.Name, "resolved harness drives the saved SessionState.Harness")
	assert.Contains(t, cmd, "codex resume "+resumeConvCodex, "Codex resume uses the `codex resume <id>` subcommand")
	assert.NotContains(t, cmd, "claude --resume", "the old hardcoded Claude form must be gone for a Codex conv")
	assert.Contains(t, cmd, "TCLAUDE_SESSION_ID="+resumeConvCodex[:8], "identity env still carried, like the spawn path")
}

// A Claude conv keeps its existing `claude --resume <id>` launch unchanged.
func TestResumeLaunchCmd_Claude(t *testing.T) {
	cmd, h, err := resumeLaunchCmd("claude", resumeConvClaude[:8], resumeConvClaude, nil)
	require.NoError(t, err)
	require.NotNil(t, h)

	assert.Equal(t, "claude", h.Name)
	assert.Contains(t, cmd, "claude --resume "+resumeConvClaude)
	assert.NotContains(t, cmd, "codex resume")
	assert.Contains(t, cmd, "TCLAUDE_SESSION_ID="+resumeConvClaude[:8])
}

// An empty harness tag (a fresh-parse conv the DB layer would coalesce to
// "claude") resolves to the default harness, so legacy untagged convs keep
// working exactly as before.
func TestResumeLaunchCmd_EmptyHarnessDefaultsToClaude(t *testing.T) {
	cmd, h, err := resumeLaunchCmd("", resumeConvClaude[:8], resumeConvClaude, nil)
	require.NoError(t, err)
	require.NotNil(t, h)

	assert.Equal(t, "claude", h.Name)
	assert.Contains(t, cmd, "claude --resume "+resumeConvClaude)
}

// An unknown / unspawnable harness tag fails with a clear error instead of
// silently spawning a broken `claude --resume` against a foreign conv id.
func TestResumeLaunchCmd_UnknownHarnessErrors(t *testing.T) {
	cmd, h, err := resumeLaunchCmd("nope", resumeConvCodex[:8], resumeConvCodex, nil)
	require.Error(t, err)
	assert.Nil(t, h)
	assert.Empty(t, cmd)
	assert.Contains(t, err.Error(), resumeConvCodex, "the error names the conv that couldn't be resumed")
}

// Passthrough args (everything after `--`) ride SpawnSpec.ExtraArgs so the
// harness Spawner shell-quotes them — including for Codex, where they land
// after the `resume <id>` subcommand.
func TestResumeLaunchCmd_ExtraArgsPassthrough(t *testing.T) {
	cmd, _, err := resumeLaunchCmd("codex", resumeConvCodex[:8], resumeConvCodex, []string{"--foo", "a b"})
	require.NoError(t, err)

	assert.Contains(t, cmd, "codex resume "+resumeConvCodex)
	assert.Contains(t, cmd, "--foo")
	assert.Contains(t, cmd, "'a b'", "an arg with a space is shell-quoted by the Spawner")
}
