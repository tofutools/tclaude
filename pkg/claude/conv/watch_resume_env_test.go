package conv

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

// The resume renderer must grant the launch directory under a minimal baseline
// exactly as the spawn path does — GitWorktreeWriteDirs is empty outside a Git
// repository, so without it a resumed minimal agent has no workspace at all.
func TestResumeMinimalBaselineGrantsNonGitWorkspace(t *testing.T) {
	setupTestDB(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", t.TempDir())

	// Deliberately not a Git repository.
	workspace := filepath.Join(home, "plain-workspace")
	require.NoError(t, os.MkdirAll(workspace, 0o755))
	canonicalWorkspace, err := filepath.EvalSymlinks(workspace)
	require.NoError(t, err)
	sibling := filepath.Join(home, "sibling")
	require.NoError(t, os.MkdirAll(sibling, 0o755))

	effective, err := sandboxpolicy.Resolve(sandboxpolicy.Scopes{Explicit: &sandboxpolicy.Profile{
		Name: "strict", ReadBaseline: sandboxpolicy.ReadBaselineMinimal,
	}})
	require.NoError(t, err)
	snapshot := sandboxpolicy.NewSnapshot(effective, nil)
	agentID, _, err := db.EnsureAgentForConv(resumeConvCodex, "test")
	require.NoError(t, err)
	require.NoError(t, db.SetAgentEffectiveSandboxConfig(agentID, &snapshot))
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "source-session", ConvID: resumeConvCodex, Harness: harness.CodexName,
		Cwd: workspace, SandboxMode: harness.SandboxManagedProfile,
	}))

	_, path, _, err := resumeLaunchCmd(harness.CodexName, resumeConvCodex[:8], resumeConvCodex, nil)
	require.NoError(t, err)
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	content := string(raw)

	assert.Contains(t, content, `"`+canonicalWorkspace+`" = "write"`,
		"a resumed minimal agent must still be able to work in its launch directory")
	assert.Contains(t, content, `":minimal" = "read"`)
	assert.NotContains(t, content, `extends = ":workspace"`)
	assert.NotContains(t, content, sibling, "nothing outside the workspace is granted")
}

func TestResumeDefaultBaselineKeepsWorkspaceInheritanceUnchanged(t *testing.T) {
	setupTestDB(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", t.TempDir())
	workspace := filepath.Join(home, "plain-workspace")
	require.NoError(t, os.MkdirAll(workspace, 0o755))

	effective, err := sandboxpolicy.Resolve(sandboxpolicy.Scopes{Explicit: &sandboxpolicy.Profile{Name: "default"}})
	require.NoError(t, err)
	snapshot := sandboxpolicy.NewSnapshot(effective, nil)
	agentID, _, err := db.EnsureAgentForConv(resumeConvCodex, "test")
	require.NoError(t, err)
	require.NoError(t, db.SetAgentEffectiveSandboxConfig(agentID, &snapshot))
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "source-session", ConvID: resumeConvCodex, Harness: harness.CodexName,
		Cwd: workspace, SandboxMode: harness.SandboxManagedProfile,
	}))

	_, path, _, err := resumeLaunchCmd(harness.CodexName, resumeConvCodex[:8], resumeConvCodex, nil)
	require.NoError(t, err)
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	content := string(raw)
	assert.Contains(t, content, `extends = ":workspace"`)
	assert.NotContains(t, content, `"`+workspace+`" = "write"`,
		"default resumes keep relying on the unchanged workspace baseline")
}

func TestCodexStrictHomeWatchRendererHostSmoke(t *testing.T) {
	if os.Getenv("TCLAUDE_CODEX_SPLIT_SMOKE") != "1" {
		t.Skip("set TCLAUDE_CODEX_SPLIT_SMOKE=1 on an unsandboxed Linux host with Codex+bubblewrap")
	}
	if runtime.GOOS != "linux" {
		t.Skip("Linux only")
	}
	setupTestDB(t)
	home := t.TempDir()
	canonicalHome, err := filepath.EvalSymlinks(home)
	require.NoError(t, err)
	home = canonicalHome
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", t.TempDir())
	container := filepath.Join(home, "git")
	workspace := filepath.Join(container, "active")
	sibling := filepath.Join(container, "sibling")
	for _, dir := range []string{workspace, sibling} {
		require.NoError(t, os.MkdirAll(dir, 0o755))
	}
	cmd := exec.Command("git", "-C", workspace, "init")
	output, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "git init: %s", output)
	common, err := harness.GitCommonDir(workspace)
	require.NoError(t, err)

	effective, err := sandboxpolicy.Resolve(sandboxpolicy.Scopes{Explicit: &sandboxpolicy.Profile{
		Name: "strict-home", ReadBaselineExclusions: []string{sandboxpolicy.ReadExclusionHome},
	}})
	require.NoError(t, err)
	snapshot := sandboxpolicy.NewSnapshot(effective, nil)
	agentID, _, err := db.EnsureAgentForConv(resumeConvCodex, "test")
	require.NoError(t, err)
	require.NoError(t, db.SetAgentEffectiveSandboxConfig(agentID, &snapshot))
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "source-session", ConvID: resumeConvCodex, Harness: harness.CodexName,
		Cwd: workspace, SandboxMode: harness.SandboxManagedProfile,
	}))

	launch, profilePath, _, err := resumeLaunchCmd(harness.CodexName, resumeConvCodex[:8], resumeConvCodex, nil)
	require.NoError(t, err)
	capability, err := harness.VerifyCodexHomeSplitPolicy()
	require.NoError(t, err)
	assert.Contains(t, launch, capability.ExecutablePath, "watch resume must bind the verified executable rather than PATH")
	raw, err := os.ReadFile(profilePath)
	require.NoError(t, err)
	content := string(raw)
	assert.Contains(t, content, `"`+workspace+`" = "write"`)
	assert.Contains(t, content, `"`+common+`" = "write"`)
	assert.NotContains(t, content, `"`+container+`" = "write"`, "strict Home must not reopen the sibling-worktree container")
	assert.NotContains(t, content, sibling)
}
