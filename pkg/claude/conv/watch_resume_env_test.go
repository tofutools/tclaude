package conv

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// resumeLaunchCmd injects the configured CLAUDE_CODE_RESUME_* overrides so the
// watch-mode resume doesn't trip Claude Code's "Resume from summary" chooser —
// the prompt that hangs a scripted/detached resume. The override lives in
// tclaude's own config.json (never ~/.claude/settings.json) and is
// Claude-Code-specific, so it must ride a Claude resume command and be absent
// from a Codex one.

// withResumeConfig points HOME at a temp dir and writes a config whose
// claude_resume.threshold_minutes is the suppress sentinel, so config.Load
// (HOME-relative) inside resumeLaunchCmd sees a deterministic override.
func withResumeConfig(t *testing.T) {
	t.Helper()
	clearAmbientResumeOverride(t)
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir) // os.UserHomeDir reads this on Windows
	db.ResetForTest()
	cfg := config.DefaultConfig()
	cfg.ClaudeResume = &config.ClaudeResumeConfig{ThresholdMinutes: new(config.ResumeThresholdMinutesSuppress)}
	require.NoError(t, config.Save(cfg))
}

// clearAmbientResumeOverride blanks the resume-threshold variable the test
// process may have inherited: Claude Code exports it in its own environment,
// and resumeLaunchCmd snapshots the inherited environment into the command it
// builds. The Contains/NotContains assertions below must observe only what
// tclaude itself injects, not what the test runner happened to leak in.
func clearAmbientResumeOverride(t *testing.T) {
	t.Helper()
	t.Setenv("CLAUDE_CODE_RESUME_THRESHOLD_MINUTES", "")
}

func TestResumeLaunchCmd_AppliesActorSnapshotAndStripsOperatorToken(t *testing.T) {
	setupTestDB(t)
	t.Setenv("TCLAUDE_HUMAN_TOKEN", "must-not-reach-pane")
	readDir := t.TempDir()
	writeDir := t.TempDir()
	denyDir := t.TempDir()
	effective, err := sandboxpolicy.Resolve(sandboxpolicy.Scopes{Global: &sandboxpolicy.Profile{
		Name: "resume-policy",
		Filesystem: []sandboxpolicy.FilesystemGrant{
			{Path: readDir, Access: sandboxpolicy.AccessRead},
			{Path: writeDir, Access: sandboxpolicy.AccessWrite},
			{Path: denyDir, Access: sandboxpolicy.AccessDeny},
		},
		Environment: []sandboxpolicy.EnvironmentEntry{{Name: "LITERAL", Value: "spaces $(touch nope); `echo nope`"}},
	}})
	require.NoError(t, err)
	snapshot := sandboxpolicy.NewSnapshot(effective, nil)
	agentID, _, err := db.EnsureAgentForConv(resumeConvClaude, "test")
	require.NoError(t, err)
	require.NoError(t, db.SetAgentEffectiveSandboxConfig(agentID, &snapshot))
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "source-session", ConvID: resumeConvClaude, Harness: harness.DefaultName,
		SandboxMode: harness.ClaudeSandboxOn,
	}))

	cmd, _, _, err := resumeLaunchCmd(harness.DefaultName, resumeConvClaude[:8], resumeConvClaude, nil)
	require.NoError(t, err)
	assert.Contains(t, cmd, "LITERAL=")
	assert.Contains(t, cmd, "$(touch nope)")
	assert.Contains(t, cmd, readDir)
	assert.Contains(t, cmd, writeDir)
	assert.Contains(t, cmd, denyDir)
	assert.Contains(t, cmd, "allowRead")
	assert.Contains(t, cmd, "allowWrite")
	assert.Contains(t, cmd, "denyRead")
	assert.Contains(t, cmd, "denyWrite")
	assert.NotContains(t, cmd, "TCLAUDE_HUMAN_TOKEN")
	assert.NotContains(t, cmd, "must-not-reach-pane")

	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "source-session", ConvID: resumeConvClaude, Harness: harness.DefaultName,
		SandboxMode: harness.ClaudeSandboxInherit,
	}))
	_, _, _, err = resumeLaunchCmd(harness.DefaultName, resumeConvClaude[:8], resumeConvClaude, nil)
	require.ErrorContains(t, err, "deny rules require sandbox on")
}

func TestResumeLaunchCmd_CodexFilesystemRequiresManagedProfile(t *testing.T) {
	setupTestDB(t)
	writeDir := t.TempDir()
	effective, err := sandboxpolicy.Resolve(sandboxpolicy.Scopes{Global: &sandboxpolicy.Profile{
		Name: "resume-policy", Filesystem: []sandboxpolicy.FilesystemGrant{{Path: writeDir, Access: sandboxpolicy.AccessWrite}},
	}})
	require.NoError(t, err)
	snapshot := sandboxpolicy.NewSnapshot(effective, nil)
	agentID, _, err := db.EnsureAgentForConv(resumeConvCodex, "test")
	require.NoError(t, err)
	require.NoError(t, db.SetAgentEffectiveSandboxConfig(agentID, &snapshot))
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "source-session", ConvID: resumeConvCodex, Harness: harness.CodexName,
		SandboxMode: harness.SandboxReadOnly,
	}))

	_, _, _, err = resumeLaunchCmd(harness.CodexName, resumeConvCodex[:8], resumeConvCodex, nil)
	require.ErrorContains(t, err, "unsupported_sandbox_profile_filesystem")

	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "source-session", ConvID: resumeConvCodex, Harness: harness.CodexName,
		SandboxMode: harness.SandboxManagedProfile,
	}))
	cmd, _, _, err := resumeLaunchCmd(harness.CodexName, resumeConvCodex[:8], resumeConvCodex, nil)
	require.NoError(t, err)
	assert.Contains(t, cmd, " -p tclaude-agent-")
	assert.Contains(t, cmd, " session codex-profile-cleanup --path")
	assert.Contains(t, cmd, "|| rm -f -- ")
	assert.True(t, strings.Contains(cmd, "codex resume"), cmd)
}

func TestResumeLaunchCmd_CodexPinsActorEnvironmentForToolCommands(t *testing.T) {
	setupTestDB(t)
	privateGOBIN := filepath.Join(t.TempDir(), "GOBIN")
	effective, err := sandboxpolicy.Resolve(sandboxpolicy.Scopes{Global: &sandboxpolicy.Profile{
		Name:        "resume-environment",
		Environment: []sandboxpolicy.EnvironmentEntry{{Name: "GOBIN", Value: privateGOBIN}},
	}})
	require.NoError(t, err)
	snapshot := sandboxpolicy.NewSnapshot(effective, nil)
	agentID, _, err := db.EnsureAgentForConv(resumeConvCodex, "test")
	require.NoError(t, err)
	require.NoError(t, db.SetAgentEffectiveSandboxConfig(agentID, &snapshot))

	cmd, _, _, err := resumeLaunchCmd(harness.CodexName, resumeConvCodex[:8], resumeConvCodex, nil)
	require.NoError(t, err)
	assert.Contains(t, cmd, "GOBIN="+privateGOBIN, "Codex itself must receive the actor environment")
	assert.Contains(t, cmd, `shell_environment_policy.set.GOBIN="`+privateGOBIN+`"`,
		"Codex tool commands must retain the actor environment after shell initialization")
}

func TestResumeLaunchCmd_CodexManagedProfileIncludesGitWorktreeGrants(t *testing.T) {
	setupTestDB(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git unavailable: %v", err)
	}
	home := t.TempDir()
	codexHome := filepath.Join(home, ".codex")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CODEX_HOME", codexHome)
	repo := filepath.Join(t.TempDir(), "repo")
	cmd := exec.Command("git", "init", "-q", repo)
	require.NoError(t, cmd.Run())

	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "source-session", ConvID: resumeConvCodex, Harness: harness.CodexName,
		Cwd: repo, SandboxMode: harness.SandboxManagedProfile,
	}))
	launch, _, _, err := resumeLaunchCmd(harness.CodexName, resumeConvCodex[:8], resumeConvCodex, nil)
	require.NoError(t, err)
	assert.Contains(t, launch, " -p tclaude-agent-")

	profiles, err := filepath.Glob(filepath.Join(codexHome, "tclaude-agent-*.config.toml"))
	require.NoError(t, err)
	require.Len(t, profiles, 1)
	content, err := os.ReadFile(profiles[0])
	require.NoError(t, err)
	commonDir, err := harness.GitCommonDir(repo)
	require.NoError(t, err)
	for _, dir := range harness.GitWorktreeWriteDirs(repo, commonDir, home) {
		assert.Contains(t, string(content), strconv.Quote(dir)+" = \"write\"")
	}
}

func TestResumeLaunchCmd_ClaudeSandboxIncludesGitWorktreeGrants(t *testing.T) {
	setupTestDB(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git unavailable: %v", err)
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	repo := filepath.Join(t.TempDir(), "repo")
	cmd := exec.Command("git", "init", "-q", repo)
	require.NoError(t, cmd.Run())

	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "source-session", ConvID: resumeConvClaude, Harness: harness.DefaultName,
		Cwd: repo, SandboxMode: harness.ClaudeSandboxOn,
	}))
	launch, _, _, err := resumeLaunchCmd(harness.DefaultName, resumeConvClaude[:8], resumeConvClaude, nil)
	require.NoError(t, err)
	commonDir, err := harness.GitCommonDir(repo)
	require.NoError(t, err)
	for _, dir := range harness.GitWorktreeWriteDirs(repo, commonDir, home) {
		assert.Contains(t, launch, dir)
	}
	assert.Contains(t, launch, "allowWrite")
}

// A Claude resume carries the configured threshold as an exported env var, so
// the spawned `claude --resume` never shows the chooser.
func TestResumeLaunchCmd_InjectsResumeOverrideForClaude(t *testing.T) {
	withResumeConfig(t)

	cmd, _, h, err := resumeLaunchCmd("claude", resumeConvClaude[:8], resumeConvClaude, nil)
	require.NoError(t, err)
	require.Equal(t, "claude", h.Name)

	assert.Contains(t, cmd, "CLAUDE_CODE_RESUME_THRESHOLD_MINUTES=525600000",
		"the configured override must be exported onto the Claude resume command")
}

// The override is Claude-Code-specific: a Codex resume must NOT carry it (Codex
// has no such prompt and the env var is meaningless there).
func TestResumeLaunchCmd_NoResumeOverrideForCodex(t *testing.T) {
	withResumeConfig(t)

	cmd, _, h, err := resumeLaunchCmd("codex", resumeConvCodex[:8], resumeConvCodex, nil)
	require.NoError(t, err)
	require.Equal(t, "codex", h.Name)

	assert.NotContains(t, cmd, "CLAUDE_CODE_RESUME_THRESHOLD_MINUTES=525600000",
		"a Codex resume must not get the Claude-specific resume override")
}

// With no claude_resume block configured, a Claude resume stays on Claude
// Code's own defaults — tclaude injects nothing.
func TestResumeLaunchCmd_NoOverrideWhenUnconfigured(t *testing.T) {
	clearAmbientResumeOverride(t)
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	db.ResetForTest()
	require.NoError(t, config.Save(config.DefaultConfig())) // no claude_resume block

	cmd, _, _, err := resumeLaunchCmd("claude", resumeConvClaude[:8], resumeConvClaude, nil)
	require.NoError(t, err)

	assert.NotContains(t, cmd, "CLAUDE_CODE_RESUME_THRESHOLD_MINUTES=525600000",
		"an unconfigured override must not inject the suppress sentinel")
}
