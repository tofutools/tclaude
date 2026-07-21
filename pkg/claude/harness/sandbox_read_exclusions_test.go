package harness

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

func assertCapabilityKind(t *testing.T, err error, kind string) {
	t.Helper()
	var capability *SandboxCapabilityError
	require.Error(t, err)
	require.True(t, errors.As(err, &capability))
	assert.Equal(t, kind, capability.Kind)
}

func installSuccessfulSplitProbe(t *testing.T, executableReopen bool) {
	t.Helper()
	oldIdentity := codexExecutableIdentityForProbe
	oldProbe := runCodexSplitPolicyProbe
	oldCache := codexSplitProbeCache.entries
	oldOS := sandboxRuntimeGOOS
	codexExecutableIdentityForProbe = func() (string, string, error) { return "/isolated/codex", "test-identity", nil }
	runCodexSplitPolicyProbe = func(string) (codexSplitPolicyCapability, error) {
		return codexSplitPolicyCapability{RequiresExecutableReopen: executableReopen}, nil
	}
	sandboxRuntimeGOOS = "linux"
	codexSplitProbeCache.entries = map[string]codexSplitProbeCacheEntry{}
	t.Cleanup(func() {
		codexExecutableIdentityForProbe = oldIdentity
		runCodexSplitPolicyProbe = oldProbe
		codexSplitProbeCache.entries = oldCache
		sandboxRuntimeGOOS = oldOS
	})
}

func TestReadExclusionCapabilityMatrix(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	leaf := []string{sandboxpolicy.ReadExclusionSSH}
	require.NoError(t, ValidateSandboxReadExclusions(DefaultName, ClaudeSandboxOn, leaf))
	assertCapabilityKind(t, ValidateSandboxReadExclusions(DefaultName, ClaudeSandboxInherit, leaf), SandboxCapabilityReadExclusions)
	require.NoError(t, ValidateSandboxReadExclusions(CodexName, SandboxManagedProfile, leaf))
	assertCapabilityKind(t, ValidateSandboxReadExclusions(CodexName, SandboxWorkspaceWrite, leaf), SandboxCapabilityReadExclusions)
	assertCapabilityKind(t, ValidateSandboxReadExclusions(CodexName, SandboxManagedProfile, []string{"future.secret"}), SandboxCapabilityReadExclusions)
}

func TestCodexHomeCapabilityRequiresLinuxVerifiedSplitPolicy(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	oldOS := sandboxRuntimeGOOS
	sandboxRuntimeGOOS = "darwin"
	t.Cleanup(func() { sandboxRuntimeGOOS = oldOS })
	err := ValidateSandboxReadExclusions(CodexName, SandboxManagedProfile, []string{sandboxpolicy.ReadExclusionHome})
	assertCapabilityKind(t, err, SandboxCapabilityReadExclusions)
	assert.ErrorContains(t, err, "openai/codex#21081")
}

func TestHomeAndBreakGlassShareVerifiedChildReopenBoundary(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	installSuccessfulSplitProbe(t, false)
	err := ValidateSandboxBreakGlassWithReadExclusions(CodexName, SandboxManagedProfile,
		[]sandboxpolicy.BreakGlassGrant{{Path: filepath.Join(os.Getenv("HOME"), ".tclaude", "data", "debug"), Access: sandboxpolicy.AccessRead}},
		[]string{sandboxpolicy.ReadExclusionHome})
	require.NoError(t, err)
}

func TestCodexHomeRulesPinBackendAndKeepNarrowReopens(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	private := filepath.Join(home, ".tclaude", "data")
	socket := filepath.Join(home, ".tclaude", "api", "agentd.sock")
	workspace := filepath.Join(home, "work")
	debug := filepath.Join(private, "debug")
	for _, dir := range []string{private, filepath.Dir(socket), workspace, debug} {
		require.NoError(t, os.MkdirAll(dir, 0o700))
	}
	content, err := codexAgentProfileContentForRules("home-test", socket, private, CodexSandboxRules{
		WriteDirs: []string{workspace}, DenyDirs: []string{home}, BreakGlassReadDirs: []string{debug}, RequireSplitPolicy: true,
	}, sandboxpolicy.NetworkAccessInherit, "linux")
	require.NoError(t, err)
	assert.Contains(t, content, "use_legacy_landlock = false")
	assert.Contains(t, content, `"`+home+`" = "none"`)
	assert.Contains(t, content, `"`+workspace+`" = "write"`)
	assert.Contains(t, content, `"`+private+`" = "none"`)
	assert.Contains(t, content, `"`+debug+`" = "read"`)
	assert.Contains(t, content, `"`+socket+`" = "read"`)
	assert.NotContains(t, content, `"`+filepath.Join(home, ".codex")+`" = "read"`)
}

func TestCodexSplitPolicyHostSmoke(t *testing.T) {
	if os.Getenv("TCLAUDE_CODEX_SPLIT_SMOKE") != "1" {
		t.Skip("set TCLAUDE_CODEX_SPLIT_SMOKE=1 on an unsandboxed Linux host with Codex+bubblewrap")
	}
	if sandboxRuntimeGOOS != "linux" {
		t.Skip("Linux only")
	}
	path, _, err := codexExecutableIdentity()
	require.NoError(t, err)
	capability, err := probeCodexSplitPolicy(path)
	require.NoError(t, err)
	t.Logf("split policy verified; exact executable reopen required=%t", capability.RequiresExecutableReopen)
}
