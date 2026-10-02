package harness

import (
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGeminiDescriptor pins the first Gemini wave's capability surface. The
// negative half matters as much as the positive one: a contract that quietly
// appeared here would make callers act on a capability nobody backed.
func TestGeminiDescriptor(t *testing.T) {
	h, ok := Get(GeminiName)
	require.True(t, ok, "gemini harness is not registered")
	assert.Equal(t, "Gemini CLI", h.DisplayName)
	require.NotNil(t, h.Spawn)
	require.NotNil(t, h.Models)
	require.NotNil(t, h.Life)

	_, err := ResolveSpawnable(GeminiName)
	require.NoError(t, err)
	assert.Equal(t, "gemini", h.Spawn.Binary())
	assert.True(t, slices.Contains(SpawnBinaries(), "gemini"))

	assert.True(t, h.SupportsLaunchEnrollment(), "--session-id makes the conv-id knowable before launch")
	assert.False(t, h.NeedsSpawnSeed())
	assert.False(t, h.TmuxScrollback, "Gemini's TUI renders its own scroll-back")

	require.NotNil(t, h.Convs, "the cold store reads Gemini's own chat files")
	require.NotNil(t, h.Ask, "headless --prompt backs the one-shot ask surface")
	assert.False(t, h.SupportsAskStream(), "stream-json parsing is not contracted")
	assert.False(t, h.CanReplayOneShotLaunchPosture())
	require.NotNil(t, h.Hooks, "settings.json hooks back live status")
	require.NotNil(t, h.Sandbox)
	mode, err := TclaudeLayerHarnessBuiltinMode(h)
	require.NoError(t, err)
	assert.Equal(t, GeminiSandboxOff, mode, "tclaude-layer forces Gemini's own sandbox off")
	off, err := SandboxOffMode(h)
	require.NoError(t, err)
	assert.Equal(t, GeminiSandboxOff, off)
	assert.False(t, h.SupportsBuiltinOSSandbox(), "no catalog mode selects Gemini's own sandbox yet")
	assert.True(t, h.SupportsApproval())
	assert.True(t, h.SupportsDirTrust())

	assert.False(t, h.SupportsRename(), "Gemini CLI has no in-pane rename command")
	assert.True(t, h.CanRename(), "rename is delivered through the ConvStore title overlay")
	assert.True(t, h.SupportsCompact())
	assert.True(t, h.SupportsSoftExit())
	assert.False(t, h.SupportsRemoteControl())
	assert.NotEmpty(t, h.SignalExitKeys())
}

func TestGeminiLifecycleTokensAreConstants(t *testing.T) {
	life := geminiLifecycle{}
	assert.Equal(t, "", life.RenameCommand())
	assert.Equal(t, "/compress", life.CompactCommand())
	assert.Equal(t, "/quit", life.SoftExitCommand())
	assert.Equal(t, "", life.RemoteControlCommand())
	assert.Equal(t, "", life.FastModeCommand())
	assert.Equal(t, []string{"C-c"}, life.SoftExitPrefixKeys())
	assert.Equal(t, []string{"Escape", "C-c", "C-c", "C-c"}, life.SignalExitKeys())
}

func TestGeminiSpawnerFreshLaunch(t *testing.T) {
	cmd := geminiSpawner{}.BuildCommand(SpawnSpec{
		EnvExports:    "export A=1; ",
		SessionID:     "8d3c0d5e-6f1a-4b8e-9a51-2f6f0e2c1a11",
		Name:          "ignored name",
		Model:         "gemini-3.1-pro-preview",
		ExtraArgs:     []string{"--debug", "it's"},
		InitialPrompt: "hello 'world'",
	})
	assert.Equal(t, "export A=1; gemini"+
		" --session-id 8d3c0d5e-6f1a-4b8e-9a51-2f6f0e2c1a11"+
		" --model=gemini-3.1-pro-preview"+
		" --debug 'it'\\''s'"+
		" '--prompt-interactive=hello '\\''world'\\'''", cmd)
	assert.NotContains(t, cmd, "ignored name", "Gemini has no launch-name flag; the name must not leak into argv")
}

func TestGeminiSpawnerMinimalAndResume(t *testing.T) {
	assert.Equal(t, "gemini", geminiSpawner{}.BuildCommand(SpawnSpec{}))

	cmd := geminiSpawner{}.BuildCommand(SpawnSpec{
		ResumeID:      "8d3c0d5e-6f1a-4b8e-9a51-2f6f0e2c1a11",
		SessionID:     "must-not-appear",
		InitialPrompt: "welcome back",
	})
	assert.Equal(t, "gemini --resume 8d3c0d5e-6f1a-4b8e-9a51-2f6f0e2c1a11 '--prompt-interactive=welcome back'", cmd)
	assert.NotContains(t, cmd, "--session-id", "--resume and --session-id are mutually exclusive in Gemini CLI")
}

// A first turn that starts with a dash must still bind as the option's value:
// `gemini -i "- fix"` exits with "Not enough arguments" before the TUI starts.
func TestGeminiSpawnerDashLeadingPromptStaysBound(t *testing.T) {
	cmd := geminiSpawner{}.BuildCommand(SpawnSpec{InitialPrompt: "- fix the tests"})
	assert.Equal(t, "gemini '--prompt-interactive=- fix the tests'", cmd)
}

func TestGeminiSpawnerExecutablePathIsQuoted(t *testing.T) {
	cmd := geminiSpawner{}.BuildCommand(SpawnSpec{ExecutablePath: "/opt/my tools/gemini"})
	assert.Equal(t, "'/opt/my tools/gemini'", cmd)
}

func TestGeminiModelCatalog(t *testing.T) {
	m := geminiModels{}
	for _, ok := range []string{"auto", "pro", "flash", "gemini-3.1-pro-preview", "gemma-4-31b-it", "Custom-Tuned-Model"} {
		got, err := m.ValidateModel("  " + ok + " ")
		require.NoError(t, err, ok)
		assert.Equal(t, ok, got, "case and bytes are preserved")
	}
	got, err := m.ValidateModel("")
	require.NoError(t, err)
	assert.Equal(t, "", got)

	for _, bad := range []string{"-y", "--yolo", "claude-sonnet-5", "opus", "sonnet[1m]", "gpt-5.4", "o3-mini", "two words", strings.Repeat("x", 129)} {
		_, err := m.ValidateModel(bad)
		assert.Error(t, err, bad)
	}

	got, err = m.ValidateEffort(" ")
	require.NoError(t, err)
	assert.Equal(t, "", got)
	_, err = m.ValidateEffort("high")
	assert.ErrorContains(t, err, "no reasoning-effort")
	assert.Empty(t, m.EffortLevels())
	assert.Contains(t, m.Models(), "auto")
}

func TestGeminiExtraArgsAudit(t *testing.T) {
	h, ok := Get(GeminiName)
	require.True(t, ok)

	for _, allowed := range [][]string{
		{"--debug"},
		{"-d"},
		{"--include-directories", "../shared", "../other"},
		{"-e", "ext-one", "ext-two", "--debug"},
		{"--include-directories=../shared"},
		{"--screen-reader"},
	} {
		assert.NoError(t, ValidateLaunchExtraArgs(h, allowed), "%v", allowed)
	}

	for _, refused := range [][]string{
		{"--resume", "latest"},
		{"-r", "3"},
		{"--resume=abc"},
		{"--session-id", "x"},
		{"--sessionId", "x"},
		{"--session-file", "/tmp/s.json"},
		{"-i", "hi"},
		{"--prompt-interactive=hi"},
		{"--promptInteractive", "hi"},
		{"-p", "hi"},
		{"-phi"},
		{"--model", "pro"},
		{"-mpro"},
		{"--approval-mode", "yolo"},
		{"--yolo"},
		{"--no-yolo"},
		{"-y"},
		{"-dy"},
		{"--skip-trust"},
		{"--sandbox"},
		{"-s"},
		{"--worktree"},
		{"--acp"},
		{"--policy", "p.toml"},
		{"some positional text"},
		{"mcp"},
		{"--debug", "stray"},
		{"--include-directories=../shared", "stray"},
		{"--"},
		{"--", "x"},
		{"-v"},
		{"-l"},
		{"--list-extensions"},
	} {
		err := ValidateLaunchExtraArgs(h, refused)
		assert.Error(t, err, "%v", refused)
	}
	err := ValidateLaunchExtraArgs(h, []string{"--yolo"})
	assert.ErrorContains(t, err, "the approval mode")
	err = ValidateLaunchExtraArgs(h, []string{"fix the tests"})
	assert.ErrorContains(t, err, "initial prompt or a subcommand")
}

func TestGeminiSpawnerSandboxOffForcesTheEnvironment(t *testing.T) {
	cmd := geminiSpawner{}.BuildCommand(SpawnSpec{
		EnvExports:         "export GEMINI_SANDBOX=docker; ",
		PreLaunchScript:    "echo pre; ",
		HarnessBuiltinMode: GeminiSandboxOff,
	})
	assert.Equal(t, "export GEMINI_SANDBOX=docker; echo pre; export GEMINI_SANDBOX=false; export SANDBOX=; gemini", cmd,
		"the forced posture comes last so nothing earlier can override it")
	assert.Equal(t, "gemini", geminiSpawner{}.BuildCommand(SpawnSpec{HarnessBuiltinMode: GeminiSandboxInherit}))

	m := geminiSandbox{}
	for _, ok := range []string{"inherit", " off "} {
		_, err := m.ValidateMode(ok)
		assert.NoError(t, err)
	}
	_, err := m.ValidateMode("docker")
	assert.Error(t, err)
	assert.Equal(t, GeminiSandboxInherit, m.DefaultMode())
	for _, mode := range m.Modes() {
		assert.NotEmpty(t, m.ModeHelp(mode))
	}
}
