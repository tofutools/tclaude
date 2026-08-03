package session

import (
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
