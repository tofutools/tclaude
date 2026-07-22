package harness

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func denyShape(t *testing.T) []sandboxpolicy.FilesystemGrant {
	t.Helper()
	home := os.Getenv("HOME")
	return []sandboxpolicy.FilesystemGrant{
		{Path: home, Access: sandboxpolicy.AccessDeny},
		{Path: filepath.Join(home, "work"), Access: sandboxpolicy.AccessRead},
	}
}

// A plain deny needs no capability at all; only the reopen shape does.
func TestPlainDenyNeedsNoCapability(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	plain := []sandboxpolicy.FilesystemGrant{{Path: home, Access: sandboxpolicy.AccessDeny}}
	for _, mode := range []string{ClaudeSandboxOn, ClaudeSandboxInherit, SandboxWorkspaceWrite} {
		require.NoError(t, ValidateSandboxReopenUnderDeny(DefaultName, mode, plain))
		require.NoError(t, ValidateSandboxReopenUnderDeny(CodexName, mode, plain))
	}
}

func TestReopenUnderDenyCapabilityMatrix(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	installSuccessfulSplitProbe(t, false)
	shape := denyShape(t)

	// Claude: the documented specificity rule applies, but only sandbox "on"
	// guarantees the deny and the reopen are applied at all.
	require.NoError(t, ValidateSandboxReopenUnderDeny(DefaultName, ClaudeSandboxOn, shape))
	err := ValidateSandboxReopenUnderDeny(DefaultName, ClaudeSandboxInherit, shape)
	assertCapabilityKind(t, err, SandboxCapabilityReopenUnderDeny)
	assert.ErrorContains(t, err, "beneath a deny")

	// Codex: managed profile only, and only with the verified split policy.
	require.NoError(t, ValidateSandboxReopenUnderDeny(CodexName, SandboxManagedProfile, shape))
	assertCapabilityKind(t, ValidateSandboxReopenUnderDeny(CodexName, SandboxWorkspaceWrite, shape), SandboxCapabilityReopenUnderDeny)

	// An unknown harness cannot promise anything.
	assertCapabilityKind(t, ValidateSandboxReopenUnderDeny("someone-else", SandboxManagedProfile, shape), SandboxCapabilityReopenUnderDeny)
}

func TestCodexReopenUnderDenyRefusedOnMacOS(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	oldOS := sandboxRuntimeGOOS
	sandboxRuntimeGOOS = "darwin"
	t.Cleanup(func() { sandboxRuntimeGOOS = oldOS })
	err := ValidateSandboxReopenUnderDeny(CodexName, SandboxManagedProfile, denyShape(t))
	assertCapabilityKind(t, err, SandboxCapabilityReopenUnderDeny)
	assert.ErrorContains(t, err, "openai/codex#21081")
}

func TestCodexReopenUnderDenyRefusedWhenProbeFails(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	oldIdentity := codexExecutableIdentityForProbe
	oldProbe := runCodexSplitPolicyProbe
	oldCache := codexSplitProbeCache.entries
	oldOS := sandboxRuntimeGOOS
	codexExecutableIdentityForProbe = func() (string, string, error) { return "/isolated/codex", "probe-fails", nil }
	runCodexSplitPolicyProbe = func(string) (codexSplitPolicyCapability, error) {
		return codexSplitPolicyCapability{}, errors.New("bwrap backend unavailable")
	}
	sandboxRuntimeGOOS = "linux"
	codexSplitProbeCache.entries = map[string]codexSplitProbeCacheEntry{}
	t.Cleanup(func() {
		codexExecutableIdentityForProbe = oldIdentity
		runCodexSplitPolicyProbe = oldProbe
		codexSplitProbeCache.entries = oldCache
		sandboxRuntimeGOOS = oldOS
	})
	err := ValidateSandboxReopenUnderDeny(CodexName, SandboxManagedProfile, denyShape(t))
	assertCapabilityKind(t, err, SandboxCapabilityReopenUnderDeny)
	assert.ErrorContains(t, err, "bubblewrap")
}

// Break-glass and the reopen shape share one verified boundary: once the split
// probe proved a narrower reopen survives a denied parent, an acknowledged
// protected child is representable too.
func TestBreakGlassAndReopenShareVerifiedChildBoundary(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	installSuccessfulSplitProbe(t, false)
	grants := []sandboxpolicy.BreakGlassGrant{{Path: filepath.Join(os.Getenv("HOME"), ".tclaude", "data", "debug"), Access: sandboxpolicy.AccessRead}}
	require.NoError(t, ValidateSandboxBreakGlassWithReopenUnderDeny(CodexName, SandboxManagedProfile, grants, denyShape(t)))

	// WITHOUT the shape, Codex keeps the conservative guard that refuses a
	// grant strictly inside the denied protected directory.
	err := ValidateSandboxBreakGlassWithReopenUnderDeny(CodexName, SandboxManagedProfile, grants, nil)
	assertCapabilityKind(t, err, SandboxCapabilityBreakGlass)
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

func TestCodexSplitCapabilityRejectsExecutableSwapBeforeLaunch(t *testing.T) {
	oldIdentity := codexExecutableIdentityForProbe
	oldProbe := runCodexSplitPolicyProbe
	oldCache := codexSplitProbeCache.entries
	identity := "identity-one"
	codexExecutableIdentityForProbe = func() (string, string, error) { return "/verified/codex", identity, nil }
	runCodexSplitPolicyProbe = func(string) (codexSplitPolicyCapability, error) { return codexSplitPolicyCapability{}, nil }
	codexSplitProbeCache.entries = map[string]codexSplitProbeCacheEntry{}
	t.Cleanup(func() {
		codexExecutableIdentityForProbe = oldIdentity
		runCodexSplitPolicyProbe = oldProbe
		codexSplitProbeCache.entries = oldCache
	})

	capability, err := VerifyCodexHomeSplitPolicy()
	require.NoError(t, err)
	assert.Equal(t, "/verified/codex", capability.ExecutablePath)
	identity = "identity-two"
	err = RevalidateCodexHomeSplitPolicyCapability(capability)
	require.ErrorContains(t, err, "identity changed")
}

func TestCodexSplitCapabilityRechecksBackendBeforeLaunch(t *testing.T) {
	oldIdentity := codexExecutableIdentityForProbe
	oldProbe := runCodexSplitPolicyProbe
	oldCache := codexSplitProbeCache.entries
	codexExecutableIdentityForProbe = func() (string, string, error) { return "/verified/codex", "stable", nil }
	backendAvailable := true
	runCodexSplitPolicyProbe = func(string) (codexSplitPolicyCapability, error) {
		if !backendAvailable {
			return codexSplitPolicyCapability{}, errors.New("bwrap backend unavailable")
		}
		return codexSplitPolicyCapability{RequiresExecutableReopen: true}, nil
	}
	codexSplitProbeCache.entries = map[string]codexSplitProbeCacheEntry{}
	t.Cleanup(func() {
		codexExecutableIdentityForProbe = oldIdentity
		runCodexSplitPolicyProbe = oldProbe
		codexSplitProbeCache.entries = oldCache
	})

	capability, err := VerifyCodexHomeSplitPolicy()
	require.NoError(t, err)
	backendAvailable = false
	err = RevalidateCodexHomeSplitPolicyCapability(capability)
	require.ErrorContains(t, err, "backend changed")
}

func TestCodexSplitPolicyHostSmoke(t *testing.T) {
	if os.Getenv("TCLAUDE_CODEX_SPLIT_SMOKE") != "1" {
		t.Skip("set TCLAUDE_CODEX_SPLIT_SMOKE=1 on an unsandboxed Linux host with Codex+bubblewrap")
	}
	if sandboxRuntimeGOOS != "linux" {
		t.Skip("Linux only")
	}
	capability, err := VerifyCodexHomeSplitPolicy()
	require.NoError(t, err)
	require.NoError(t, RevalidateCodexHomeSplitPolicyCapability(capability))

	root := t.TempDir()
	home := filepath.Join(root, "home")
	config := filepath.Join(root, "codex-config")
	container := filepath.Join(home, "git")
	workspace := filepath.Join(container, "active")
	common := filepath.Join(workspace, ".git")
	admin := filepath.Join(common, "worktrees", "active")
	siblingRepo := filepath.Join(container, "sibling-repo")
	arbitrarySibling := filepath.Join(container, "arbitrary")
	private := filepath.Join(home, ".tclaude", "data")
	breakGlass := filepath.Join(private, "acknowledged")
	privateSibling := filepath.Join(private, "still-private")
	agentCache := filepath.Join(root, "agent-dirs", "GOCACHE")
	explicitRead := filepath.Join(root, "explicit-read")
	explicitWrite := filepath.Join(root, "explicit-write")
	socket := filepath.Join(home, ".tclaude", "api", "agentd.sock")
	for _, dir := range []string{config, workspace, common, admin, siblingRepo, arbitrarySibling, breakGlass, privateSibling, agentCache, explicitRead, explicitWrite, filepath.Dir(socket)} {
		require.NoError(t, os.MkdirAll(dir, 0o700))
	}
	writeFile := func(path, value string) { require.NoError(t, os.WriteFile(path, []byte(value), 0o600)) }
	writeFile(filepath.Join(workspace, "workspace-readable"), "workspace")
	writeFile(filepath.Join(siblingRepo, "secret"), "sibling")
	writeFile(filepath.Join(arbitrarySibling, "secret"), "arbitrary")
	writeFile(filepath.Join(private, "secret"), "private")
	writeFile(filepath.Join(breakGlass, "allowed"), "break-glass")
	writeFile(filepath.Join(privateSibling, "secret"), "private-sibling")
	writeFile(filepath.Join(explicitRead, "allowed"), "explicit-read")

	listener, err := net.Listen("unix", socket)
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", config)

	readDirs := []string{explicitRead}
	if capability.RequiresExecutableReopen {
		readDirs = append(readDirs, capability.ExecutablePath)
	}
	profileName, profilePath, err := EnsureCodexAgentLaunchProfileForRules(CodexSandboxRules{
		ReadDirs:            readDirs,
		WriteDirs:           []string{workspace, common, admin, explicitWrite, agentCache},
		DenyDirs:            []string{home},
		BreakGlassReadDirs:  []string{breakGlass},
		RequireSplitPolicy:  true,
		BreakGlassWriteDirs: nil,
	}, sandboxpolicy.NetworkAccessInherit, "1234567890abcdef")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.Remove(profilePath) })

	q := func(path string) string { return fmt.Sprintf("%q", path) }
	script := strings.Join([]string{
		"test \"$(cat " + q(filepath.Join(workspace, "workspace-readable")) + ")\" = workspace",
		"echo ok > " + q(filepath.Join(workspace, "workspace-write")),
		"test \"$(cat " + q(filepath.Join(explicitRead, "allowed")) + ")\" = explicit-read",
		"! sh -c 'echo no > " + filepath.Join(explicitRead, "blocked-write") + "'",
		"echo ok > " + q(filepath.Join(explicitWrite, "allowed-write")),
		"echo ok > " + q(filepath.Join(agentCache, "cache-write")),
		"echo ok > " + q(filepath.Join(common, "common-write")),
		"echo ok > " + q(filepath.Join(admin, "admin-write")),
		"test \"$(cat " + q(filepath.Join(breakGlass, "allowed")) + ")\" = break-glass",
		"! sh -c 'echo no > " + filepath.Join(breakGlass, "blocked-write") + "'",
		"test ! -r " + q(filepath.Join(siblingRepo, "secret")),
		"! sh -c 'echo no > " + filepath.Join(siblingRepo, "blocked-write") + "'",
		"test ! -r " + q(filepath.Join(arbitrarySibling, "secret")),
		"! sh -c 'echo no > " + filepath.Join(arbitrarySibling, "blocked-write") + "'",
		"test ! -r " + q(filepath.Join(private, "secret")),
		"test ! -r " + q(filepath.Join(privateSibling, "secret")),
		"! sh -c 'echo no > " + filepath.Join(privateSibling, "blocked-write") + "'",
		"test -S " + q(socket),
		"printf tclaude-production-split-ok",
	}, " && ")
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, capability.ExecutablePath, "sandbox", "-p", profileName, "-P", profileName, "-C", workspace, "--", "/bin/sh", "-c", script)
	cmd.Env = []string{"HOME=" + home, "CODEX_HOME=" + config, "PATH=" + os.Getenv("PATH"), "TMPDIR=" + root}
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatal("production split-policy smoke timed out")
	}
	require.NoErrorf(t, err, "production split-policy smoke output: %s", output)
	assert.Contains(t, string(output), "tclaude-production-split-ok")
	t.Logf("production split policy verified; exact executable=%s reopen-required=%t", capability.ExecutablePath, capability.RequiresExecutableReopen)
}
