package session

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// copilotLaunchRoot builds a hermetic HOME for one launch-spec test and points
// the process at it, so the Copilot baseline resolves inside the temp tree
// rather than against the developer's real home.
func copilotLaunchRoot(t *testing.T) (home, workspace string) {
	t.Helper()
	home = t.TempDir()
	// Canonicalized because the launch spec resolves symlinks before emitting a
	// grant — a mount rule has to name the directory the kernel will see, not a
	// spelling of it. On macOS t.TempDir hands back /var/folders/… while the
	// spec comes back with /private/var/folders/…, so an uncanonicalized
	// expectation here would be asserting against the symlink rather than
	// against the grant.
	if resolved, err := filepath.EvalSymlinks(home); err == nil {
		home = resolved
	}
	t.Setenv("HOME", home)
	t.Setenv("TMUX_TMPDIR", filepath.Join(home, "tmux"))

	// TMPDIR has to move with HOME. t.TempDir hands back a directory UNDER the
	// system temp root, so a test HOME of /tmp/TestX/001 leaves the catalog's
	// temp row resolving to /tmp — which covers that HOME, and the baseline
	// then correctly refuses to hand back a grant covering the home directory.
	// That refusal is the guard working, not a bug, but it is an artifact of a
	// temp HOME that no real launch has: on a real host /tmp does not contain
	// /home/you. Pointing TMPDIR inside HOME restores the real relationship.
	tempDir := filepath.Join(home, "tmp")
	require.NoError(t, os.MkdirAll(tempDir, 0o700))
	t.Setenv("TMPDIR", tempDir)

	workspace = filepath.Join(home, "workspace")
	require.NoError(t, os.MkdirAll(workspace, 0o755))
	return home, workspace
}

// TestBuildTclaudeLayerLaunchSpecComposesCopilotBaseline is the wiring proof:
// the pre-approved catalog does not merely exist, it reaches the launch the
// mount plan is rendered from.
//
// It asserts the three properties a Copilot launch actually depends on — the
// state directory is writable, the package cache is writable, and the workspace
// is granted by the CALLER's contract rather than by the catalog — instead of
// pinning the whole grant list, which would turn every future catalog row into
// a failing test here rather than in the catalog's own suite.
func TestBuildTclaudeLayerLaunchSpecComposesCopilotBaseline(t *testing.T) {
	home, workspace := copilotLaunchRoot(t)

	spec, err := BuildTclaudeLayerLaunchSpec(TclaudeLayerLaunchInput{
		HarnessName: harness.CopilotName,
		Cwd:         workspace,
	})
	require.NoError(t, err)

	writable := map[string]bool{}
	for _, grant := range spec.Effective.Filesystem {
		if grant.Access == sandboxpolicy.AccessWrite {
			writable[grant.Path] = true
		}
	}
	stateDir := filepath.Join(home, ".copilot")
	assert.True(t, writable[stateDir],
		"COPILOT_HOME must be writable: it is the one path whose denial fails a launch outright")
	// The package cache is the row that MOVES between platforms, so the
	// expectation has to move with it — a hard-coded XDG path would assert the
	// Linux catalog on a Mac and fail for the wrong reason.
	packageCache := filepath.Join(home, ".cache", "copilot")
	if runtime.GOOS == "darwin" {
		packageCache = filepath.Join(home, "Library", "Caches", "copilot")
	}
	assert.True(t, writable[packageCache],
		"the package cache must be writable: a cold cache is unpacked on first launch "+
			"and after every version bump")
	assert.True(t, writable[workspace],
		"the workspace grant is the caller's and must still be present")

	// The state root the launch contract prepares must be the SAME directory the
	// catalog granted. Two resolvers disagreeing here would produce a launch that
	// creates one Copilot home and confines another.
	assert.Equal(t, stateDir, spec.Contract.StateRoot)
	assert.Contains(t, spec.Contract.StateDirs, stateDir,
		"the state directory must be part of the launch contract, not only of the profile")
}

// TestBuildTclaudeLayerLaunchSpecAllowsProfileWriteToCopilotPackageCache is the
// operator-reported composition: a general development profile grants the
// Copilot package cache writable, and Copilot's mandatory launch baseline adds
// the same authority independently. The duplicate is compatible and must not
// turn a valid launch into a harness-state conflict.
func TestBuildTclaudeLayerLaunchSpecAllowsProfileWriteToCopilotPackageCache(t *testing.T) {
	home, workspace := copilotLaunchRoot(t)
	packageCache := filepath.Join(home, ".cache", "copilot")
	if runtime.GOOS == "darwin" {
		packageCache = filepath.Join(home, "Library", "Caches", "copilot")
	}
	require.NoError(t, os.MkdirAll(packageCache, 0o700))

	profileGrant := sandboxpolicy.FilesystemGrant{
		Path: packageCache, Access: sandboxpolicy.AccessWrite,
	}
	spec, err := BuildTclaudeLayerLaunchSpec(TclaudeLayerLaunchInput{
		HarnessName: harness.CopilotName,
		Cwd:         workspace,
		Snapshot: &sandboxpolicy.Snapshot{Effective: sandboxpolicy.EffectiveProfile{
			Filesystem: []sandboxpolicy.FilesystemGrant{profileGrant},
		}},
	})
	require.NoError(t, err)
	require.Contains(t, spec.Contract.StateDirs, packageCache)
	require.Contains(t, spec.Contract.ProfileFilesystem, profileGrant)
	require.NoError(t, ValidateTclaudeLayerLaunchSpec(spec),
		"the profile write and Copilot's generated package-cache write are semantically identical")
}

// TestBuildTclaudeLayerLaunchSpecHonorsCopilotHome pins the relocation case,
// which is the one where a second resolver would silently drift: the contract's
// state root and the catalog's grant both have to follow COPILOT_HOME.
func TestBuildTclaudeLayerLaunchSpecHonorsCopilotHome(t *testing.T) {
	home, workspace := copilotLaunchRoot(t)
	moved := filepath.Join(home, "elsewhere", "copilot")
	require.NoError(t, os.MkdirAll(moved, 0o700))
	t.Setenv(harness.CopilotHomeEnvVar, moved)

	spec, err := BuildTclaudeLayerLaunchSpec(TclaudeLayerLaunchInput{
		HarnessName: harness.CopilotName,
		Cwd:         workspace,
	})
	require.NoError(t, err)

	assert.Equal(t, moved, spec.Contract.StateRoot)
	writable := map[string]bool{}
	for _, grant := range spec.Effective.Filesystem {
		if grant.Access == sandboxpolicy.AccessWrite {
			writable[grant.Path] = true
		}
	}
	assert.True(t, writable[moved],
		"a relocated COPILOT_HOME must be the directory that is granted")
	assert.False(t, writable[filepath.Join(home, ".copilot")],
		"the default location must NOT be granted when COPILOT_HOME moved it; "+
			"granting both would confine one directory and create another")
}

// TestBuildTclaudeLayerLaunchSpecRefusesCopilotHomeOverTheWorkspace is the
// cwd-conflict case, and the reason the catalog takes a Workspace at all.
//
// The launch contract grants the workspace deliberately and scoped. A CATALOG
// row covering it would mean an environment variable had quietly widened a
// Copilot state path over the repository — a grant nobody authored, arriving
// through a harness baseline. The launch fails instead.
func TestBuildTclaudeLayerLaunchSpecRefusesCopilotHomeOverTheWorkspace(t *testing.T) {
	_, workspace := copilotLaunchRoot(t)
	t.Setenv(harness.CopilotHomeEnvVar, workspace)

	_, err := BuildTclaudeLayerLaunchSpec(TclaudeLayerLaunchInput{
		HarnessName: harness.CopilotName,
		Cwd:         workspace,
	})
	require.Error(t, err)
	var capErr *harness.SandboxCapabilityError
	require.ErrorAs(t, err, &capErr)
	assert.Equal(t, "copilot-sandbox-baseline-too-broad", capErr.Kind)
}

// TestBuildTclaudeLayerLaunchSpecRefusesCopilotHomeOverHome covers the other
// environment shape an operator can reach by typing one variable, and the one
// that would silently convert "Copilot runs confined" into "Copilot runs with
// the home directory open".
func TestBuildTclaudeLayerLaunchSpecRefusesCopilotHomeOverHome(t *testing.T) {
	home, workspace := copilotLaunchRoot(t)
	t.Setenv(harness.CopilotHomeEnvVar, home)

	_, err := BuildTclaudeLayerLaunchSpec(TclaudeLayerLaunchInput{
		HarnessName: harness.CopilotName,
		Cwd:         workspace,
	})
	require.Error(t, err)
	var capErr *harness.SandboxCapabilityError
	require.ErrorAs(t, err, &capErr)
	assert.Equal(t, "copilot-sandbox-baseline-too-broad", capErr.Kind)
}

// TestValidateTclaudeLayerHarnessPostureIgnoresOtherHarnesses keeps the gate
// scoped. Claude, Codex and OpenCode all have a real launch-time lever for
// their own sandbox, so their posture IS established by the launch; running a
// configuration check for them would be inventing a refusal none of them needs.
func TestValidateTclaudeLayerHarnessPostureIgnoresOtherHarnesses(t *testing.T) {
	home, _ := copilotLaunchRoot(t)
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".copilot"), 0o700))
	require.NoError(t, os.WriteFile(
		filepath.Join(home, ".copilot", harness.CopilotSettingsFileName),
		[]byte(`{"sandbox":{"enabled":true}}`), 0o600))

	for _, name := range []string{
		harness.DefaultName, harness.CodexName, harness.OpenCodeName,
	} {
		h := harness.MustGet(name)
		assert.NoError(t, ValidateTclaudeLayerHarnessPosture(h, nil, nil),
			"harness %q must not be gated on Copilot's configuration", name)
	}

	copilot := harness.MustGet(harness.CopilotName)
	assert.Error(t, ValidateTclaudeLayerHarnessPosture(copilot, nil, nil),
		"the same configuration must still refuse a Copilot launch")
}

// TestValidateTclaudeLayerHarnessPostureReadsTheLaunchEnvironment proves the
// gate follows the launch's own environment rather than tclaude's. A profile
// that relocates COPILOT_HOME moves which file governs the launch, and a gate
// reading the ambient one would be inspecting a file the agent never opens.
func TestValidateTclaudeLayerHarnessPostureReadsTheLaunchEnvironment(t *testing.T) {
	home, _ := copilotLaunchRoot(t)
	profileHome := filepath.Join(home, "profile-copilot")
	require.NoError(t, os.MkdirAll(profileHome, 0o700))
	require.NoError(t, os.WriteFile(
		filepath.Join(profileHome, harness.CopilotSettingsFileName),
		[]byte(`{"sandbox":{"enabled":true}}`), 0o600))

	copilot := harness.MustGet(harness.CopilotName)
	// Ambient environment is clean, so without the override the launch passes.
	require.NoError(t, ValidateTclaudeLayerHarnessPosture(copilot, nil, nil))

	err := ValidateTclaudeLayerHarnessPosture(copilot, []sandboxpolicy.EnvironmentEntry{
		{Name: harness.CopilotHomeEnvVar, Value: profileHome},
	}, nil)
	require.Error(t, err,
		"the gate must inspect the settings file the LAUNCH environment selects")
	assert.Contains(t, err.Error(), profileHome)
}

// stubTmuxOnPath satisfies the tmux PRESENCE check without a host tmux.
//
// runNew calls CheckTmuxInstalled (an exec.LookPath) long before it reaches the
// sandbox gate, so a production-path test on a runner without tmux fails at the
// wrong place and proves nothing about the gate. swapTmux replaces the command
// RUNNER, which is why this file needs both: nothing here ever executes the
// stub, it only has to be found.
//
// Doing it this way keeps the assertion on the real runNew path rather than
// carving out a pure helper to test instead — the whole point of these cases is
// which call sites reach the gate, so testing anything short of runNew would
// pass even with the bug restored. It also makes them hermetic on Linux, where
// they had been quietly depending on the developer's tmux being installed.
func stubTmuxOnPath(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	stub := filepath.Join(dir, "tmux")
	require.NoError(t, os.WriteFile(stub, []byte("#!/bin/sh\nexit 0\n"), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestRunNewRefusesCopilotTclaudeLayerWithNoSandboxProfile is the F1
// regression, and it is a direct-CLI test on purpose.
//
// The posture gate was reachable only from inside the access-axes block, which
// runs when a profile declares network or socket rules. So the SIMPLEST way to
// reach the feature —
//
//	tclaude session new --harness copilot --sandbox-impl tclaude-layer
//
// with no profile at all — went straight to launch with neither the assert-off
// gate nor the pass-through-argument refusal ever running, while the recorded
// posture claimed tclaude's layer was the only boundary. A daemon spawn was
// covered because agentd has its own gate; the direct CLI had nothing.
//
// The test drives runNew itself rather than the validator, because "which call
// sites reach the validator" is precisely what was wrong. Asserting on
// ValidateTclaudeLayerHarnessPosture directly would have passed throughout.
func TestRunNewRefusesCopilotTclaudeLayerWithNoSandboxProfile(t *testing.T) {
	home, workspace := copilotLaunchRoot(t)
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".copilot"), 0o700))
	require.NoError(t, os.WriteFile(
		filepath.Join(home, ".copilot", harness.CopilotSettingsFileName),
		[]byte(`{"sandbox":{"enabled":true}}`), 0o600))

	prevCheck := ClaudeAncestorCheck
	ClaudeAncestorCheck = func() bool { return false }
	t.Cleanup(func() { ClaudeAncestorCheck = prevCheck })
	stubTmuxOnPath(t)
	swapTmux(t, &launchRecordingTmux{})

	err := runNew(&NewParams{
		Label:       "spwn-copilot-noprofile",
		Harness:     harness.CopilotName,
		SandboxImpl: string(sandboxpolicy.ImplementationTclaudeLayer),
		Dir:         workspace,
		Detached:    true,
	})
	require.Error(t, err,
		"a no-profile tclaude-layer Copilot launch must be refused: nothing else on this "+
			"path verifies that Copilot's own sandbox is off")
	var capErr *harness.SandboxCapabilityError
	require.ErrorAs(t, err, &capErr)
	assert.Equal(t, harness.SandboxCapabilityCopilotInnerSandbox, capErr.Kind)
	assert.Contains(t, capErr.Message, "sandbox.enabled")
}

// TestRunNewRefusesCopilotTclaudeLayerExtraExperimentalArg covers the argv half
// of the same hole. The pass-through refusal lived behind the same block, so a
// no-profile launch could hand Copilot `--experimental` — re-registering the
// in-pane `/sandbox` command — with nothing to stop it.
func TestRunNewRefusesCopilotTclaudeLayerExtraExperimentalArg(t *testing.T) {
	_, workspace := copilotLaunchRoot(t)

	prevCheck := ClaudeAncestorCheck
	ClaudeAncestorCheck = func() bool { return false }
	t.Cleanup(func() { ClaudeAncestorCheck = prevCheck })
	stubTmuxOnPath(t)
	swapTmux(t, &launchRecordingTmux{})

	// Pass-through arguments are taken from the real command line (everything
	// after `--`), which is what an operator actually types, so the test has to
	// go through the same source rather than a struct field the CLI never sets.
	previousArgs := os.Args
	os.Args = []string{"tclaude", "session", "new", "--", "--experimental"}
	t.Cleanup(func() { os.Args = previousArgs })

	err := runNew(&NewParams{
		Label:       "spwn-copilot-experimental",
		Harness:     harness.CopilotName,
		SandboxImpl: string(sandboxpolicy.ImplementationTclaudeLayer),
		Dir:         workspace,
		Detached:    true,
	})
	require.Error(t, err, "the argv gate must run on a no-profile launch too")
	var capErr *harness.SandboxCapabilityError
	require.ErrorAs(t, err, &capErr)
	assert.Equal(t, harness.SandboxCapabilityCopilotInnerSandbox, capErr.Kind)
	assert.Contains(t, capErr.Message, "--experimental")
}

// TestRunNewAcceptsCopilotTclaudeLayerWithCleanPosture is the other half: the
// hoisted gate must not turn every ordinary Copilot launch into a refusal.
// Without it, a fix for the hole above could pass both tests above by refusing
// unconditionally.
func TestRunNewAcceptsCopilotTclaudeLayerWithCleanPosture(t *testing.T) {
	_, workspace := copilotLaunchRoot(t)

	prevCheck := ClaudeAncestorCheck
	ClaudeAncestorCheck = func() bool { return false }
	t.Cleanup(func() { ClaudeAncestorCheck = prevCheck })
	stubTmuxOnPath(t)
	swapTmux(t, &launchRecordingTmux{})

	err := runNew(&NewParams{
		Label:       "spwn-copilot-clean",
		Harness:     harness.CopilotName,
		SandboxImpl: string(sandboxpolicy.ImplementationTclaudeLayer),
		Dir:         workspace,
		Detached:    true,
	})
	var capErr *harness.SandboxCapabilityError
	if errors.As(err, &capErr) &&
		capErr.Kind == harness.SandboxCapabilityCopilotInnerSandbox {
		t.Fatalf("a clean Copilot posture must not be refused by the assert-off gate: %v", err)
	}
}
