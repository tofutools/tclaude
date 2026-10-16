package agentd

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/testutil"
)

func isolateOneShotDirectories(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", testutil.CanonicalTempDir(t))
	t.Setenv("XDG_CACHE_HOME", testutil.CanonicalTempDir(t))
	data := filepath.Join(os.Getenv("HOME"), ".tclaude", "data")
	require.NoError(t, os.MkdirAll(data, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(data, "config.json"), []byte(`{"features":{"agent_dirs_mount_parent":false}}`), 0600))
	previous := prepareNonInteractiveResourceCgroup
	prepareNonInteractiveResourceCgroup = func(string, sandboxpolicy.ResourceLimits) (string, func(), error) {
		return "", func() {}, errors.New("no delegation")
	}
	t.Cleanup(func() { prepareNonInteractiveResourceCgroup = previous })
}

func oneShotPolicy(t *testing.T, blocks ...sandboxpolicy.PreLaunchBlock) *sandboxpolicy.Snapshot {
	t.Helper()
	effective, err := sandboxpolicy.Resolve(sandboxpolicy.Scopes{Global: &sandboxpolicy.Profile{
		Name: "dev", AgentDirectories: []string{"GOCACHE"}, Environment: []sandboxpolicy.EnvironmentEntry{{Name: "POLICY_VALUE", Value: "profile"}}, PreLaunch: blocks,
	}})
	require.NoError(t, err)
	snapshot := sandboxpolicy.NewSnapshot(effective, nil)
	return &snapshot
}

func TestOneShotDirectoriesAndPreLaunchReachShell(t *testing.T) {
	isolateOneShotDirectories(t)
	useDirectNonInteractiveRunner(t)
	snapshot := oneShotPolicy(t, sandboxpolicy.PreLaunchBlock{Name: "setup", Script: "export SCRIPT_VALUE=$POLICY_VALUE; printf ready > \"$GOCACHE/from-script\"\n", Exports: []string{"SCRIPT_VALUE"}})
	var roots []string
	for range 2 {
		result, fail := runNonInteractiveSpawn(context.Background(), spawnParams{Harness: harness.ShellName, Cwd: testutil.CanonicalTempDir(t), SandboxImplementation: "off", EffectiveSandbox: snapshot,
			InitialMessage: "printf '%s|%s|%s' \"$GOCACHE\" \"$SCRIPT_VALUE\" \"$(cat \"$GOCACHE/from-script\")\""}, 10)
		require.Nil(t, fail)
		require.Zero(t, result.ExitCode, result.Stderr)
		parts := strings.Split(result.Stdout, "|")
		require.Equal(t, []string{"profile", "ready"}, parts[1:])
		roots = append(roots, filepath.Dir(parts[0]))
		require.Contains(t, strings.Join(result.Notes, " "), "retained")
		_, err := os.Stat(parts[0])
		require.NoError(t, err)
	}
	require.NotEqual(t, roots[0], roots[1])
	previous := oneShotBootIdentity
	oneShotBootIdentity = func() string { return "next-boot" }
	t.Cleanup(func() { oneShotBootIdentity = previous })
	require.NoError(t, cleanupRetainedOneShotDirectories())
	for _, root := range roots {
		_, err := os.Stat(root)
		require.ErrorIs(t, err, os.ErrNotExist)
	}
}

func TestOneShotPreLaunchFailureAbortsAndAttributesBlock(t *testing.T) {
	isolateOneShotDirectories(t)
	useDirectNonInteractiveRunner(t)
	marker := filepath.Join(testutil.CanonicalTempDir(t), "harness-started")
	result, fail := runNonInteractiveSpawn(context.Background(), spawnParams{Harness: harness.ShellName, Cwd: testutil.CanonicalTempDir(t), SandboxImplementation: "off",
		EffectiveSandbox: oneShotPolicy(t, sandboxpolicy.PreLaunchBlock{Name: "bad-setup", Script: "false\n"}), InitialMessage: "touch " + marker}, 10)
	require.Nil(t, fail)
	require.Equal(t, 126, result.ExitCode)
	require.Contains(t, result.Stderr, "bad-setup")
	_, err := os.Stat(marker)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestOneShotNativeCodexReceivesDirectoryEnvironmentAndGrants(t *testing.T) {
	isolateOneShotDirectories(t)
	useDirectNonInteractiveRunner(t)
	bin := testutil.CanonicalTempDir(t)
	require.NoError(t, os.WriteFile(filepath.Join(bin, "codex"), []byte("#!/bin/sh\nprintf '%s' \"$GOCACHE\"\n"), 0755))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	previous := EnsureSeanceCodexProfile
	var granted string
	EnsureSeanceCodexProfile = func(_, id string, snapshot *sandboxpolicy.Snapshot) (string, string, *harness.CodexSplitPolicyCapability, error) {
		require.NotEmpty(t, id)
		for _, entry := range snapshot.Effective.Environment {
			if entry.Name == "GOCACHE" {
				granted = entry.Value
			}
		}
		require.Contains(t, agentDirWriteGrants(*snapshot), sandboxpolicy.FilesystemGrant{Path: granted, Access: sandboxpolicy.AccessWrite})
		profile := filepath.Join(bin, "profile")
		require.NoError(t, os.WriteFile(profile, nil, 0600))
		return "one-shot-profile", profile, nil, nil
	}
	t.Cleanup(func() { EnsureSeanceCodexProfile = previous })
	result, fail := runNonInteractiveSpawn(context.Background(), spawnParams{Harness: harness.CodexName, Cwd: testutil.CanonicalTempDir(t), SandboxImplementation: "harness-builtin", HarnessBuiltinMode: harness.SandboxManagedProfile,
		EffectiveSandbox: oneShotPolicy(t), InitialMessage: "task"}, 10)
	require.Nil(t, fail)
	require.Zero(t, result.ExitCode, result.Stderr)
	require.Equal(t, granted, result.Stdout)
}

func TestOneShotDirectoryCleanupAfterCgroupReap(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("cgroups are Linux-only")
	}
	isolateOneShotDirectories(t)
	useDirectNonInteractiveRunner(t)
	previousConfigure, previousValidate, previousRemove := configureNonInteractiveResourceCgroup, validateNonInteractivePreparedResourceCgroup, removeNonInteractiveResourceCgroup
	t.Cleanup(func() {
		configureNonInteractiveResourceCgroup = previousConfigure
		validateNonInteractivePreparedResourceCgroup = previousValidate
		removeNonInteractiveResourceCgroup = previousRemove
	})
	prepareNonInteractiveResourceCgroup = func(string, sandboxpolicy.ResourceLimits) (string, func(), error) {
		return testutil.CanonicalTempDir(t), func() {}, nil
	}
	configureNonInteractiveResourceCgroup = func(*exec.Cmd, string) (func(), error) { return func() {}, nil }
	validateNonInteractivePreparedResourceCgroup = func(string, sandboxpolicy.ResourceLimits) error { return nil }
	removeNonInteractiveResourceCgroup = func(dir string) error { return os.Remove(dir) }
	for _, prompt := range []string{"printf '%s' \"$GOCACHE\"", "printf '%s' \"$GOCACHE\"; exit 7", "printf '%s' \"$GOCACHE\"; sleep 10"} {
		result, fail := runNonInteractiveSpawn(context.Background(), spawnParams{Harness: harness.ShellName, Cwd: testutil.CanonicalTempDir(t), SandboxImplementation: "off", EffectiveSandbox: oneShotPolicy(t), InitialMessage: prompt}, 1)
		require.Nil(t, fail)
		require.NotEmpty(t, result.Stdout)
		_, err := os.Stat(filepath.Dir(result.Stdout))
		require.ErrorIs(t, err, os.ErrNotExist)
	}
}

func TestOneShotDirectorySweepKeepsUnprovenAndActiveRuns(t *testing.T) {
	isolateOneShotDirectories(t)
	run, err := newOneShotDirectoryRun()
	require.NoError(t, err)
	snapshot, _, err := materializeAgentDirectories(*oneShotPolicy(t), run.Key)
	require.NoError(t, err)
	require.NotEmpty(t, snapshot.Effective.Environment)
	root := filepath.Join(run.Base, run.Key)
	require.NoError(t, cleanupRetainedOneShotDirectories())
	_, err = os.Stat(root)
	require.NoError(t, err)
	require.NoError(t, run.launched(""))
	run.finish()
	require.NoError(t, cleanupRetainedOneShotDirectories())
	_, err = os.Stat(root)
	require.NoError(t, err)
	// A cache symlink replacement must not redirect a later reboot sweep.
	outside := testutil.CanonicalTempDir(t)
	require.NoError(t, os.WriteFile(filepath.Join(outside, "keep"), nil, 0600))
	require.NoError(t, os.RemoveAll(root))
	require.NoError(t, os.Symlink(outside, root))
	previous := oneShotBootIdentity
	oneShotBootIdentity = func() string { return "new-boot" }
	t.Cleanup(func() { oneShotBootIdentity = previous })
	require.NoError(t, cleanupRetainedOneShotDirectories())
	_, err = os.Stat(filepath.Join(outside, "keep"))
	require.NoError(t, err)
}

func TestOneShotDirectoryPreLaunchValidationFailureCleansTree(t *testing.T) {
	isolateOneShotDirectories(t)
	_, fail := runNonInteractiveSpawn(context.Background(), spawnParams{Harness: harness.ShellName, EffectiveSandbox: oneShotPolicy(t), SandboxImplementation: "off"}, 10)
	require.NotNil(t, fail)
	entries, err := os.ReadDir(oneShotDirectoryJournalRoot())
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestOneShotDirectoryRetentionDisclosedWhenCgroupReapFails(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("cgroups are Linux-only")
	}
	isolateOneShotDirectories(t)
	useDirectNonInteractiveRunner(t)
	previousConfigure, previousValidate, previousRemove := configureNonInteractiveResourceCgroup, validateNonInteractivePreparedResourceCgroup, removeNonInteractiveResourceCgroup
	t.Cleanup(func() {
		configureNonInteractiveResourceCgroup = previousConfigure
		validateNonInteractivePreparedResourceCgroup = previousValidate
		removeNonInteractiveResourceCgroup = previousRemove
	})
	prepareNonInteractiveResourceCgroup = func(string, sandboxpolicy.ResourceLimits) (string, func(), error) {
		return testutil.CanonicalTempDir(t), func() {}, nil
	}
	configureNonInteractiveResourceCgroup = func(*exec.Cmd, string) (func(), error) { return func() {}, nil }
	validateNonInteractivePreparedResourceCgroup = func(string, sandboxpolicy.ResourceLimits) error { return nil }
	removeNonInteractiveResourceCgroup = func(string) error { return errors.New("still busy") }
	result, fail := runNonInteractiveSpawn(context.Background(), spawnParams{Harness: harness.ShellName, Cwd: testutil.CanonicalTempDir(t), SandboxImplementation: "off", EffectiveSandbox: oneShotPolicy(t), InitialMessage: "printf '%s' \"$GOCACHE\""}, 10)
	require.Nil(t, fail)
	require.Contains(t, strings.Join(result.Notes, " "), filepath.Dir(result.Stdout))
	_, err := os.Stat(result.Stdout)
	require.NoError(t, err)
}

func TestOneShotOuterLayerAppliesPrivateDirectoriesAndSetup(t *testing.T) {
	if err := session.TclaudeLayerServerHostAvailability(); err != nil {
		t.Skipf("outer layer unavailable: %v", err)
	}
	isolateOneShotDirectories(t)
	useDirectNonInteractiveRunner(t)
	result, fail := runNonInteractiveSpawn(context.Background(), spawnParams{Harness: harness.ShellName, Cwd: testutil.CanonicalTempDir(t), SandboxImplementation: "tclaude-layer",
		EffectiveSandbox: oneShotPolicy(t, sandboxpolicy.PreLaunchBlock{Name: "setup", Script: "export SCRIPT_VALUE=$POLICY_VALUE; printf seeded > \"$GOCACHE/value\"\n", Exports: []string{"SCRIPT_VALUE"}}),
		InitialMessage:   "printf '%s:%s' \"$SCRIPT_VALUE\" \"$(cat \"$GOCACHE/value\")\""}, 10)
	require.Nil(t, fail)
	require.Zero(t, result.ExitCode, result.Stderr)
	require.Equal(t, "profile:seeded", result.Stdout)
}

func TestOneShotNamedProxyOmissionIsDisclosed(t *testing.T) {
	isolateOneShotDirectories(t)
	useDirectNonInteractiveRunner(t)
	path := filepath.Join(os.Getenv("HOME"), ".tclaude", "data", "config.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"agent":{"http_proxies":{"service":{"base_url":"https://example.com"}}}}`), 0600))
	result, fail := runNonInteractiveSpawn(context.Background(), spawnParams{Harness: harness.ShellName, Cwd: testutil.CanonicalTempDir(t), InitialMessage: "printf answer"}, 10)
	require.Nil(t, fail)
	require.Equal(t, "answer", result.Stdout)
	require.Contains(t, strings.Join(result.Notes, " "), "Named HTTP proxy URLs are omitted")
}

func TestOneShotPolicyRefusalNamesFieldsWithoutValues(t *testing.T) {
	snapshot := oneShotPolicy(t, sandboxpolicy.PreLaunchBlock{Name: "setup", Script: "secret-script-content", Exports: []string{"SAFE_NAME"}})
	message := oneShotPolicyFailure(snapshot, errors.New("missing Bash"))
	require.Contains(t, message, `global "dev"`)
	require.Contains(t, message, "GOCACHE")
	require.Contains(t, message, "setup")
	require.NotContains(t, message, "secret-script-content")
	require.NotContains(t, message, "profile\"")
}

func TestOneShotDirectorySweepReapsAbandonedAuthoredCgroup(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("cgroups are Linux-only")
	}
	isolateOneShotDirectories(t)
	run, err := newOneShotDirectoryRun()
	require.NoError(t, err)
	_, _, err = materializeAgentDirectories(*oneShotPolicy(t), run.Key)
	require.NoError(t, err)
	cgroup := testutil.CanonicalTempDir(t)
	limits := sandboxpolicy.ResourceLimits{Memory: "16MiB"}
	require.NoError(t, run.launched(cgroup, limits))
	activeOneShotDirectories.Delete(run.Key) // model an abandoned helper/daemon
	previousValidate, previousRemove := validateNonInteractivePreparedResourceCgroup, removeNonInteractiveResourceCgroup
	t.Cleanup(func() {
		validateNonInteractivePreparedResourceCgroup = previousValidate
		removeNonInteractiveResourceCgroup = previousRemove
	})
	var validated bool
	validateNonInteractivePreparedResourceCgroup = func(dir string, got sandboxpolicy.ResourceLimits) error {
		require.Equal(t, cgroup, dir)
		require.Equal(t, limits, got)
		validated = true
		return nil
	}
	removeNonInteractiveResourceCgroup = func(dir string) error { require.True(t, validated); return os.Remove(dir) }
	require.NoError(t, cleanupRetainedOneShotDirectories())
	_, err = os.Stat(filepath.Join(run.Base, run.Key))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestOneShotDirectoryJournalCanonicalizesAliasedHome(t *testing.T) {
	isolateOneShotDirectories(t)
	alias := filepath.Join(testutil.CanonicalTempDir(t), "home-alias")
	require.NoError(t, os.Symlink(os.Getenv("HOME"), alias))
	t.Setenv("HOME", alias)
	run, err := newOneShotDirectoryRun()
	require.NoError(t, err)
	run.finish()
	_, err = os.Stat(filepath.Join(oneShotDirectoryJournalRoot(), run.Key+".json"))
	require.ErrorIs(t, err, os.ErrNotExist)
}
