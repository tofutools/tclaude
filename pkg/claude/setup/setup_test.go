package setup

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/wsl"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// captureStdout redirects os.Stdout for the duration of fn and returns
// everything written to it. Used to assert that setup actually emits a
// section, not just that the section's text-builder works in isolation.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w
	defer func() { os.Stdout = orig }()
	fn()
	require.NoError(t, w.Close())
	var buf bytes.Buffer
	_, err = io.Copy(&buf, r)
	require.NoError(t, err)
	return buf.String()
}

// tempHome points HOME (and USERPROFILE, which os.UserHomeDir reads on
// Windows) at a fresh temp dir so setup's writes — ~/.claude/settings.json,
// ~/.claude/skills/, ~/.tclaude/config.json — land in an isolated,
// throwaway tree instead of the developer's real home.
func tempHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	return dir
}

func assertSkillsInstalled(t *testing.T, home string) {
	t.Helper()
	assert.DirExists(t, filepath.Join(home, ".claude", "skills", "agent-coord"))
}

func assertNoSkills(t *testing.T, home string) {
	t.Helper()
	assert.NoDirExists(t, filepath.Join(home, ".claude", "skills"))
}

func assertBundledPermsGranted(t *testing.T) {
	t.Helper()
	cfg, err := config.Load()
	require.NoError(t, err)
	require.NotNil(t, cfg.Agent)
	for _, slug := range selfPermsForBundledSkills {
		assert.Contains(t, cfg.Agent.DefaultPermissions, slug)
	}
}

func assertBundledPermsNotGranted(t *testing.T) {
	t.Helper()
	cfg, err := config.Load()
	require.NoError(t, err)
	if cfg.Agent == nil {
		return
	}
	for _, slug := range selfPermsForBundledSkills {
		assert.NotContains(t, cfg.Agent.DefaultPermissions, slug)
	}
}

// With no --install-* flags, installExtras is a no-op: a baseline-only
// `tclaude setup` installs no skills and grants no default permissions.
func TestInstallExtras_NoFlags_NoOp(t *testing.T) {
	home := tempHome(t)

	require.NoError(t, installExtras(&Params{}))

	assertNoSkills(t, home)
	assertBundledPermsNotGranted(t)
}

// --install-agent-skills installs skills only — it does not grant
// default permissions.
func TestInstallExtras_SkillsOnly(t *testing.T) {
	home := tempHome(t)

	require.NoError(t, installExtras(&Params{InstallAgentSkills: true}))

	assertSkillsInstalled(t, home)
	assertBundledPermsNotGranted(t)
}

// --install-default-agent-permissions grants permissions only — it does
// not install skills.
func TestInstallExtras_PermsOnly(t *testing.T) {
	home := tempHome(t)

	require.NoError(t, installExtras(&Params{InstallDefaultAgentPerms: true}))

	assertNoSkills(t, home)
	assertBundledPermsGranted(t)
}

// --install-all installs every optional extra.
func TestInstallExtras_All(t *testing.T) {
	home := tempHome(t)

	require.NoError(t, installExtras(&Params{InstallAll: true}))

	assertSkillsInstalled(t, home)
	assertBundledPermsGranted(t)
}

// --install-all must be equivalent to passing every individual
// --install-* flag.
func TestInstallExtras_AllEqualsBothFlags(t *testing.T) {
	t.Run("install-all", func(t *testing.T) {
		home := tempHome(t)
		require.NoError(t, installExtras(&Params{InstallAll: true}))
		assertSkillsInstalled(t, home)
		assertBundledPermsGranted(t)
	})
	t.Run("both-individual-flags", func(t *testing.T) {
		home := tempHome(t)
		require.NoError(t, installExtras(&Params{
			InstallAgentSkills:       true,
			InstallDefaultAgentPerms: true,
		}))
		assertSkillsInstalled(t, home)
		assertBundledPermsGranted(t)
	})
}

// Running installExtras twice must not duplicate granted permission
// slugs in the config.
func TestInstallExtras_Idempotent(t *testing.T) {
	home := tempHome(t)

	require.NoError(t, installExtras(&Params{InstallAll: true}))
	require.NoError(t, installExtras(&Params{InstallAll: true}))

	assertSkillsInstalled(t, home)
	cfg, err := config.Load()
	require.NoError(t, err)
	require.NotNil(t, cfg.Agent)
	for _, slug := range selfPermsForBundledSkills {
		count := 0
		for _, p := range cfg.Agent.DefaultPermissions {
			if p == slug {
				count++
			}
		}
		assert.Equalf(t, 1, count, "slug %s should appear exactly once, got %d", slug, count)
	}
}

// The baseline always runs, even when an --install-* flag is passed.
// Before this redesign, an --install-* flag triggered a "focused
// branch" that skipped the baseline entirely — so `tclaude setup
// --install-agent-skills` left a machine with skills but no hooks. This
// guards against that regression.
//
// runSetup is only side-effect-free to exercise end-to-end on native
// Linux: on macOS it may `brew install`, and on WSL it writes a Windows
// registry key. Elsewhere the test skips.
func TestRunSetup_BaselineRunsAlongsideExtras(t *testing.T) {
	if runtime.GOOS != "linux" || wsl.IsWSL() {
		t.Skip("runSetup is only safe to exercise end-to-end on native Linux")
	}
	if !isTmuxInstalled() {
		t.Skip("tmux missing: baseline halts at the prerequisite check")
	}
	home := tempHome(t)

	out := captureStdout(t, func() {
		require.NoError(t, runSetup(&Params{Yes: true, InstallAgentSkills: true}))
	})

	// Baseline ran despite the --install-* flag: hooks are installed.
	installed, missing, _ := session.CheckHooksInstalled()
	assert.True(t, installed, "baseline must install hooks alongside --install-agent-skills, missing: %v", missing)
	assert.FileExists(t, filepath.Join(home, ".claude", "settings.json"))
	// The requested extra ran too.
	assertSkillsInstalled(t, home)
	// The agent-sandbox advisory is part of the baseline output.
	assert.Contains(t, out, "=== Agent Sandbox ===")
}

// checkStatus must surface the agent-sandbox advisory so that
// `tclaude setup --check` points operators at the hardening doc. This
// guards the call site, which TestSandboxAdvisory_NamesPathsAndDoc
// (which only exercises sandboxAdvisory itself) does not.
func TestCheckStatus_PrintsSandboxAdvisory(t *testing.T) {
	tempHome(t)

	out := captureStdout(t, func() {
		require.NoError(t, checkStatus())
	})

	assert.Contains(t, out, "=== Agent Sandbox ===")
	assert.Contains(t, out, sandboxHardeningDocURL)
}

// The agent-sandbox advisory must name both sensitive trees, frame the
// daemon layer as a guardrail, and link the hardening doc — that pointer
// is the whole point of the advisory.
func TestSandboxAdvisory_NamesPathsAndDoc(t *testing.T) {
	adv := sandboxAdvisory()
	for _, want := range []string{
		"=== Agent Sandbox ===",
		"~/.tclaude",
		"~/.claude/sessions",
		"guardrail",
		sandboxHardeningDocURL,
	} {
		assert.Containsf(t, adv, want, "advisory should mention %q", want)
	}
}

// findRepoRoot returns the repository root, derived from this test
// file's own location (pkg/claude/setup/setup_test.go).
func findRepoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	require.True(t, ok, "runtime.Caller failed")
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "..")
}

// The doc the advisory points operators to must actually exist in the
// repo — this guards against the const drifting from the file.
func TestSandboxHardeningDocExists(t *testing.T) {
	docPath := filepath.Join(findRepoRoot(t), filepath.FromSlash(sandboxHardeningDocPath))
	assert.FileExistsf(t, docPath, "advisory points at %s; expected it at %s",
		sandboxHardeningDocPath, docPath)
}

// Every in-repo reference to the sandbox-hardening doc must use the name
// the setup advisory's const points at. Catches a rename that updates
// the const and the file but leaves a markdown cross-reference dangling.
func TestSandboxDocCrossReferencesConsistent(t *testing.T) {
	root := findRepoRoot(t)
	base := filepath.Base(sandboxHardeningDocPath) // sandbox-hardening.md
	for _, ref := range []string{
		// docs/plans/ was removed (#253 — planning moved to Linear); only the
		// surviving in-repo docs are cross-checked now.
		filepath.Join("docs", "index.md"),
	} {
		body, err := os.ReadFile(filepath.Join(root, ref))
		require.NoErrorf(t, err, "reading %s", ref)
		assert.Containsf(t, string(body), base,
			"%s must reference the sandbox-hardening doc by name (%s)", ref, base)
	}
}
