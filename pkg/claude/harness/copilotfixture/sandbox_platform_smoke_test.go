package copilotfixture_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/harness/copilotfixture"
)

// Platform proofs for the outer sandbox (TCL-978).
//
// The baseline (TCL-975) makes a claim that is DIFFERENT on macOS from Linux,
// and it is the claim an operator is least likely to expect: the package cache
// moves to ~/Library/Caches/copilot on darwin, while the device-id cache stays
// XDG-shaped at ~/.cache on both platforms because the bundled runtime that
// writes it has no darwin branch.
//
// Every other fixture scenario overrides COPILOT_CACHE_HOME and XDG_CACHE_HOME,
// which is exactly what makes them portable — and exactly what makes them
// unable to observe that split. These scenarios drop the overrides so the CLI
// resolves its platform defaults, and then require the baseline resolved for
// the SAME environment to name the directories the CLI actually used.
//
// They run on both platforms rather than being skipped off-darwin: a macOS-only
// test tells you nothing about whether the Linux claim still holds, and the
// failure mode this guards against — a resolver whose two branches drift — is
// only visible if both branches are exercised.

// TestCopilotDefaultCachePlacementMatchesBaseline is the credential-free proof
// behind the baseline's platform split.
func TestCopilotDefaultCachePlacementMatchesBaseline(t *testing.T) {
	requireSmoke(t)

	mock := copilotfixture.NewMockProvider(t, []copilotfixture.Turn{
		{Text: "MOCK PLATFORM ANSWER"},
	})
	dirs := copilotfixture.NewSandboxDirs(t)
	result := copilotfixture.Run(t, copilotfixture.RunOptions{
		Root: dirs.Root, Home: dirs.Home, WorkDir: dirs.WorkDir,
		// The whole point of this scenario: no cache overrides, so the CLI
		// resolves ~/Library/Caches/copilot or ~/.cache/copilot itself.
		OmitCacheOverrides: true,
		BaseURL:            mock.BaseURL(), Prompt: "Platform question.",
	})
	require.Equal(t, 0, result.ExitCode, "stderr: %s", result.Stderr)

	// The baseline is resolved from the run's ACTUAL environment: COPILOT_HOME
	// is the only variable still set, and HOME is the disposable root, so every
	// other row comes from the platform default the run just exercised.
	runEnv := map[string]string{harness.CopilotHomeEnvVar: dirs.Home}
	entries, err := harness.CopilotSandboxBaseline(harness.CopilotBaselineInput{
		Home:      dirs.Root,
		Getenv:    func(k string) string { return runEnv[k] },
		Workspace: dirs.WorkDir,
	})
	require.NoError(t, err)

	paths := map[string]string{}
	for _, entry := range entries {
		paths[entry.ID] = entry.Path
	}

	wantPackageCache := filepath.Join(dirs.Root, ".cache", "copilot")
	if runtime.GOOS == "darwin" {
		wantPackageCache = filepath.Join(dirs.Root, "Library", "Caches", "copilot")
	}
	assert.Equal(t, wantPackageCache, paths[harness.CopilotBaselinePackageCache],
		"the baseline's default package-cache resolution does not match this platform")

	// The device-id cache is XDG-shaped on BOTH platforms. Asserting it here,
	// beside the package cache, is what makes the split visible: on darwin these
	// two rows deliberately live in different trees.
	wantDeviceID := filepath.Join(dirs.Root, ".cache", "Microsoft", "DeveloperTools")
	assert.Equal(t, wantDeviceID, paths[harness.CopilotBaselineDeviceIDCache],
		"the device-id cache must stay XDG-shaped on every platform")

	// Now the observation half: the CLI must actually have written where the
	// catalog says. A claim that merely agreed with itself would prove nothing.
	payload := filepath.Join(wantPackageCache, "pkg")
	info, err := os.Stat(payload)
	require.NoError(t, err,
		"Copilot did not unpack its payload under the platform-default package cache %s; "+
			"either the CLI changed its resolver or the baseline's platform branch is wrong",
		wantPackageCache)
	require.True(t, info.IsDir())

	deviceID := filepath.Join(wantDeviceID, "deviceid")
	_, err = os.Stat(deviceID)
	require.NoError(t, err,
		"the bundled runtime did not write %s; the device-id row's XDG shape is the "+
			"claim this asserts, and on darwin it is the one that has no platform branch",
		deviceID)

	// Nothing may have landed in the OTHER platform's location: a CLI that wrote
	// both would make the baseline's single package-cache row an under-grant.
	strayRoots := []string{filepath.Join(dirs.Root, "Library", "Caches", "copilot")}
	if runtime.GOOS == "darwin" {
		strayRoots = []string{filepath.Join(dirs.Root, ".cache", "copilot")}
	}
	for _, stray := range strayRoots {
		_, err := os.Stat(stray)
		assert.True(t, os.IsNotExist(err),
			"Copilot also wrote the other platform's package cache at %s; the baseline "+
				"grants one package-cache root, so this would be an under-grant", stray)
	}
}

// TestCopilotPackageCacheIsExecutable proves the row the outer layer is most
// likely to get quietly wrong.
//
// The package cache carries rwx in the catalog, and the x is not decorative:
// the CLI unpacks the bundled ripgrep binary and prebuilt native modules there
// and RUNS them. bubblewrap and Seatbelt both allow execution from an ordinary
// readable bind, so a mount plan that mounted this path noexec would still pass
// every read/write assertion and break only tool search — a failure that reads
// as a Copilot bug rather than a sandbox one.
//
// This asserts the property at its source: the binary the CLI unpacked is
// really executable, and really executes.
func TestCopilotPackageCacheIsExecutable(t *testing.T) {
	requireSmoke(t)

	mock := copilotfixture.NewMockProvider(t, []copilotfixture.Turn{
		{Text: "MOCK EXEC ANSWER"},
	})
	dirs := copilotfixture.NewSandboxDirs(t)
	result := copilotfixture.Run(t, copilotfixture.RunOptions{
		Root: dirs.Root, Home: dirs.Home, Cache: dirs.Cache, XDGCache: dirs.XDGCache,
		WorkDir: dirs.WorkDir,
		BaseURL: mock.BaseURL(), Prompt: "Exec question.",
	})
	require.Equal(t, 0, result.ExitCode, "stderr: %s", result.Stderr)

	// The payload root is <cache>/pkg/<platform>/<version>, and the bundled
	// ripgrep sits under a ripgrep/bin/ subdirectory below it. Both the
	// platform tuple and the exact depth are host- and version-dependent, so
	// the binary is DISCOVERED by walking rather than spelled out: a hard-coded
	// path would turn a layout change into a confusing "not found" instead of
	// the real signal, which is whether an executable exists there at all.
	var ripgrep string
	err := filepath.WalkDir(filepath.Join(dirs.Cache, "pkg"),
		func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil || entry.IsDir() || ripgrep != "" {
				return nil //nolint:nilerr // an unreadable entry is not this test's subject
			}
			if name := entry.Name(); name != "rg" && name != "rg.exe" {
				return nil
			}
			info, statErr := entry.Info()
			if statErr != nil || info.Mode().Perm()&0o111 == 0 {
				return nil
			}
			ripgrep = path
			return nil
		})
	require.NoError(t, err)
	require.NotEmpty(t, ripgrep,
		"no executable was found under the unpacked package cache %s; the catalog's rwx "+
			"package-cache row exists because the CLI runs binaries from there, so an empty "+
			"result means either the payload layout changed or the run did not unpack",
		dirs.Cache)

	// Executing it is the actual proof. A permission bit can be set on a path a
	// sandbox would still refuse to exec.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, ripgrep, "--version").CombinedOutput()
	require.NoError(t, err,
		"could not execute %s from the package cache: %s", ripgrep, string(output))
	assert.Contains(t, strings.ToLower(string(output)), "ripgrep",
		"unexpected output from the bundled ripgrep at %s", ripgrep)

	// And the catalog must actually carry the execute bit for that root, which
	// is what a mount-plan translation keys on.
	runEnv := map[string]string{
		harness.CopilotHomeEnvVar:      dirs.Home,
		harness.CopilotCacheHomeEnvVar: dirs.Cache,
		"XDG_CACHE_HOME":               dirs.XDGCache,
	}
	entries, err := harness.CopilotSandboxBaseline(harness.CopilotBaselineInput{
		Home:      dirs.Root,
		Getenv:    func(k string) string { return runEnv[k] },
		Workspace: dirs.WorkDir,
	})
	require.NoError(t, err)
	found := false
	for _, entry := range entries {
		if entry.ID != harness.CopilotBaselinePackageCache {
			continue
		}
		found = true
		assert.True(t, entry.Access.Execute,
			"the package-cache row must be exec-bearing: %s was just executed out of it", ripgrep)
		assert.True(t, strings.HasPrefix(ripgrep, entry.Path+string(filepath.Separator)),
			"the executed binary %s lies outside the granted package-cache root %s",
			ripgrep, entry.Path)
	}
	require.True(t, found, "the baseline produced no package-cache row")
}

// TestCopilotInnerSandboxDefaultIsOff settles, against the real binary, the
// premise the whole assert-off contract rests on: an ordinary Copilot launch
// does not engage its own command sandbox, and does not write a settings file
// that would turn it on.
//
// If this ever fails, the tclaude-layer posture is no longer "one wall" and the
// contract in harness/copilot_sandbox.go has to change rather than be trusted.
func TestCopilotInnerSandboxDefaultIsOff(t *testing.T) {
	requireSmoke(t)

	mock := copilotfixture.NewMockProvider(t, []copilotfixture.Turn{
		{Text: "MOCK SANDBOX ANSWER"},
	})
	dirs := copilotfixture.NewSandboxDirs(t)
	result := copilotfixture.Run(t, copilotfixture.RunOptions{
		Root: dirs.Root, Home: dirs.Home, Cache: dirs.Cache, XDGCache: dirs.XDGCache,
		WorkDir: dirs.WorkDir,
		BaseURL: mock.BaseURL(), Prompt: "Sandbox question.",
	})
	require.Equal(t, 0, result.ExitCode, "stderr: %s", result.Stderr)

	runEnv := map[string]string{harness.CopilotHomeEnvVar: dirs.Home}
	state, err := harness.ResolveCopilotInnerSandbox(
		func(k string) string { return runEnv[k] }, dirs.Root)
	require.NoError(t, err,
		"the settings file a real run leaves behind must be readable and unambiguous")
	assert.False(t, state.Enabled,
		"a plain Copilot run enabled its own command sandbox; the tclaude-layer posture "+
			"assumes it does not, so this contract must be revisited rather than trusted")
	assert.False(t, state.Experimental,
		"a plain Copilot run enabled experimental features, which registers the in-pane "+
			"/sandbox command the assert-off gate refuses over")
	require.NoError(t, harness.ValidateCopilotTclaudeLayerInnerSandbox(state),
		"a launch following a plain Copilot run must pass the assert-off gate")
}

// TestCopilotInnerSandboxEnabledIsDetectedBeforeLaunch proves the gate reads
// the same file the CLI does — not a file tclaude merely believes in.
//
// The evidence is behavioural rather than textual: the settings are written
// into the run's own COPILOT_HOME, the CLI is then launched against it and
// still completes a turn (so the file is valid input, not something the CLI
// rejects), and the gate independently refuses that same directory.
func TestCopilotInnerSandboxEnabledIsDetectedBeforeLaunch(t *testing.T) {
	requireSmoke(t)

	mock := copilotfixture.NewMockProvider(t, []copilotfixture.Turn{
		{Text: "MOCK ENABLED-SANDBOX ANSWER"},
	})
	dirs := copilotfixture.NewSandboxDirs(t)
	settingsPath := filepath.Join(dirs.Home, "settings.json")
	require.NoError(t, os.WriteFile(settingsPath,
		[]byte(`{"sandbox":{"enabled":true}}`), 0o600))

	runEnv := map[string]string{harness.CopilotHomeEnvVar: dirs.Home}
	state, err := harness.ResolveCopilotInnerSandbox(
		func(k string) string { return runEnv[k] }, dirs.Root)
	require.NoError(t, err)
	assert.Equal(t, settingsPath, state.SettingsPath,
		"the gate must inspect the same settings file the CLI is launched against")
	assert.True(t, state.Enabled)
	require.Error(t, harness.ValidateCopilotTclaudeLayerInnerSandbox(state),
		"a launch under an enabled inner sandbox must be refused, not downgraded")

	// The CLI accepts the file: it is a real configuration, so the refusal above
	// is guarding a reachable posture rather than a hypothetical one.
	result := copilotfixture.Run(t, copilotfixture.RunOptions{
		Root: dirs.Root, Home: dirs.Home, Cache: dirs.Cache, XDGCache: dirs.XDGCache,
		WorkDir: dirs.WorkDir,
		BaseURL: mock.BaseURL(), Prompt: "Enabled-sandbox question.",
	})
	require.Equal(t, 0, result.ExitCode,
		"the CLI rejected the sandbox settings this gate refuses over, so the gate is "+
			"guarding a configuration that cannot occur: stderr: %s", result.Stderr)
}
