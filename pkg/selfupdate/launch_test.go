package selfupdate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/testutil"
)

func TestCompareLaunchIdenticalIgnoresVolatileAndUpdaterEnv(t *testing.T) {
	argv := []string{"/usr/bin/tclaude", "agentd", "serve", "--no-tray", "--dashboard-port", "8123"}
	before, err := CaptureLaunch(argv, []string{"PATH=/a:/b", "HOME=/h", "INVOCATION_ID=1", "JOURNAL_STREAM=8:1"}, "systemd-user", "tclaude.service", "supervisor")
	require.NoError(t, err)
	check := CompareLaunch(before, argv, []string{"HOME=/h", "PATH=/a:/b", "INVOCATION_ID=2", "TCLAUDE_UPDATE_JOB_ID=x"}, "systemd-user", "tclaude.service", "supervisor", nil)
	assert.Equal(t, "identical", check.State, check.Differences)
	assert.Equal(t, "Kept flags and environment.", check.Summary())
}

func TestCompareLaunchReportsEveryDifferenceWithoutValues(t *testing.T) {
	before, err := CaptureLaunch(
		[]string{"/old/tclaude", "agentd", "serve", "--no-tray", "--dashboard-port", "8123", "--terminal=kitty", "--api-token", "s3cret"},
		[]string{"PATH=/a", "OPENAI_API_KEY=old-secret", "GONE=1"}, "systemd-user", "u.service", "supervisor")
	require.NoError(t, err)
	check := CompareLaunch(before,
		[]string{"/old/tclaude", "agentd", "serve", "--dashboard-port=9000", "--terminal=kitty", "--api-token", "other", "--pprof"},
		[]string{"PATH=/a", "OPENAI_API_KEY=new-secret", "NEW=1"}, "systemd-user", "u.service", "supervisor", nil)
	require.Equal(t, "differs", check.State)
	got := map[string]LaunchDifference{}
	for _, d := range check.Differences {
		got[d.Kind+" "+d.Name] = d
	}
	assert.Equal(t, "removed", got["flag --no-tray"].Change)
	assert.Equal(t, "added", got["flag --pprof"].Change)
	assert.Equal(t, LaunchDifference{Kind: "flag", Change: "changed", Name: "--dashboard-port", Before: "--dashboard-port=8123", After: "--dashboard-port=9000"}, got["flag --dashboard-port"])
	assert.NotContains(t, got, "flag --terminal")
	// A secret flag's change is reported, its values never.
	assert.Equal(t, "changed", got["flag --api-token"].Change)
	assert.Equal(t, LaunchDifference{Kind: "env", Change: "changed", Name: "OPENAI_API_KEY"}, got["env OPENAI_API_KEY"])
	assert.Equal(t, "removed", got["env GONE"].Change)
	assert.Equal(t, "added", got["env NEW"].Change)
	summary := check.Summary()
	for _, secret := range []string{"s3cret", "other", "old-secret", "new-secret"} {
		assert.NotContains(t, summary, secret)
	}
	assert.Contains(t, summary, "environment OPENAI_API_KEY changed")
	// The snapshot itself keeps no value in clear either.
	for _, v := range before.Env {
		assert.Len(t, v, 64)
	}
	assert.NotContains(t, strings.Join(before.Argv, " "), "s3cret")
}

func TestCompareLaunchExpectsTheRelocatedBinary(t *testing.T) {
	before, err := CaptureLaunch([]string{"/opt/brew/bin/tclaude", "agentd", "serve"}, nil, "shell", "", "exec")
	require.NoError(t, err)
	moved := CompareLaunch(before, []string{"/home/u/.local/bin/tclaude", "agentd", "serve"}, nil, "shell", "", "exec",
		[]Relocation{{Name: "tclaude", From: "/opt/brew/bin/tclaude", To: "/home/u/.local/bin/tclaude"}})
	assert.Equal(t, "identical", moved.State, moved.Differences)
	other := CompareLaunch(before, []string{"/elsewhere/tclaude", "agentd", "serve"}, nil, "shell", "", "exec", nil)
	require.Equal(t, "differs", other.State)
	assert.Equal(t, "binary", other.Differences[0].Kind)
}

// fakeTclaude writes a shell script standing in for a staged binary.
func fakeTclaude(t *testing.T, dir, script string) string {
	t.Helper()
	p := filepath.Join(dir, "tclaude")
	require.NoError(t, os.WriteFile(p, []byte("#!/bin/sh\n"+script), 0o755))
	return p
}

func TestCheckLaunchArgsAsksTheNewBinary(t *testing.T) {
	dir := testutil.CanonicalTempDir(t)
	bin := fakeTclaude(t, dir, `[ "$1" = "--tclaude-check-args" ] || exit 9
for a in "$@"; do [ "$a" = "--removed" ] && { echo "Error: unknown flag: --removed" >&2; exit 2; }; done
exit 0
`)
	mode, err := checkLaunchArgs(context.Background(), bin, []string{"agentd", "serve", "--no-tray"})
	require.NoError(t, err)
	assert.Equal(t, "passed", mode)
	_, err = checkLaunchArgs(context.Background(), bin, []string{"agentd", "serve", "--removed"})
	require.EqualError(t, err, "unknown flag: --removed")
}

func TestCheckLaunchArgsFallsBackToHelpOnOlderBinaries(t *testing.T) {
	dir := testutil.CanonicalTempDir(t)
	bin := fakeTclaude(t, dir, `if [ "$1" = "--tclaude-check-args" ]; then echo "Error: unknown flag: --tclaude-check-args" >&2; exit 1; fi
[ "$1 $2 $3" = "agentd serve --help" ] || exit 9
cat <<'EOF'
Flags:
      --dashboard-port int   port
  -h, --help                 help
      --no-tray              no tray
  -s, --socket string        socket
EOF
`)
	mode, err := checkLaunchArgs(context.Background(), bin, []string{"agentd", "serve", "--no-tray", "--dashboard-port=1", "-s", "/x"})
	require.NoError(t, err)
	assert.Equal(t, "passed_help", mode)
	_, err = checkLaunchArgs(context.Background(), bin, []string{"agentd", "serve", "--wizard"})
	require.EqualError(t, err, "unknown flag: --wizard")
}

// An apply whose new binary rejects the daemon's flags replaces nothing,
// unless forced.
func TestApplyRefusesABinaryThatRejectsTheDaemonFlags(t *testing.T) {
	dir := testutil.CanonicalTempDir(t)
	bin := filepath.Join(dir, "bin", "tclaude")
	writeBin(t, bin, "v1.0.0")
	s := versionedService(t, dir, []Binary{{Name: "tclaude", Path: bin, Version: "v1.0.0", Method: "release"}}, "v1.1.0")
	var checked []string
	s.checkArgs = func(_ context.Context, path string, args []string) (string, error) {
		checked = args
		return "", errors.New("unknown flag: --old-flag")
	}
	snap, err := CaptureLaunch([]string{bin, "agentd", "serve", "--old-flag"}, []string{"PATH=/a"}, "shell", "", "exec")
	require.NoError(t, err)
	plan := &LaunchPlan{Snapshot: snap, Binary: "tclaude", Args: [][]string{{"agentd", "serve", "--old-flag"}}}

	job, err := s.StartWith(Request{Action: "apply"}, "operator", func() bool { return true }, ApplyOptions{Launch: plan})
	require.NoError(t, err)
	require.Equal(t, "recorded", job.Launch.State)
	job = waitJob(t, s, job.ID)
	require.Equal(t, "failed", job.State)
	assert.Contains(t, job.Error, "would not start with the daemon's current flags: unknown flag: --old-flag")
	assert.Equal(t, "failed", job.Launch.ArgsCheck)
	assert.Equal(t, []string{"agentd", "serve", "--old-flag"}, checked)
	raw, err := os.ReadFile(bin)
	require.NoError(t, err)
	assert.Equal(t, "v1.0.0", string(raw), "nothing was replaced")
	assert.Contains(t, job.Error, "nothing was replaced")
	assert.Equal(t, "not_restarted", job.Launch.State, "no comparison follows a refused update")
	_, err = os.Stat(s.launchPath(job.ID))
	assert.True(t, os.IsNotExist(err), "the snapshot's hashes are dropped when no restart follows")

	job, err = s.StartWith(Request{Action: "apply", Force: true}, "operator", func() bool { return true }, ApplyOptions{Launch: plan})
	require.NoError(t, err)
	job = waitJob(t, s, job.ID)
	require.Equal(t, "restarting", job.State, job.Error)
	assert.Equal(t, "forced", job.Launch.ArgsCheck)
	raw, err = os.ReadFile(bin)
	require.NoError(t, err)
	assert.Equal(t, "v1.1.0", string(raw))
}

// The snapshot is durable across the restart and compared exactly once by
// the new process.
func TestRestartedServiceComparesWithTheDurableSnapshot(t *testing.T) {
	dir := testutil.CanonicalTempDir(t)
	bin := filepath.Join(dir, "bin", "tclaude")
	writeBin(t, bin, "v1.0.0")
	s := versionedService(t, dir, []Binary{{Name: "tclaude", Path: bin, Version: "v1.0.0", Method: "release"}}, "v1.1.0")
	s.hooks.Restart = func() error { return nil }
	argv := []string{bin, "agentd", "serve", "--no-tray"}
	snap, err := CaptureLaunch(argv, []string{"PATH=/a", "SECRET_TOKEN=x"}, "shell", "", "exec")
	require.NoError(t, err)
	job, err := s.StartWith(Request{Action: "apply"}, "operator", func() bool { return true }, ApplyOptions{Launch: &LaunchPlan{Snapshot: snap, Binary: "tclaude", Args: [][]string{argv[1:]}}})
	require.NoError(t, err)
	job = waitJob(t, s, job.ID)
	require.Equal(t, "restarting", job.State, job.Error)
	assert.Equal(t, "passed", job.Launch.ArgsCheck)
	raw, err := os.ReadFile(s.launchPath(job.ID))
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "SECRET_TOKEN=x")

	// The new process: same argv, the secret value changed.
	restarted, err := New(filepath.Join(dir, "updates"), []Binary{{Name: "tclaude", Path: bin, Version: "v1.1.0", Method: "release"}}, Hooks{})
	require.NoError(t, err)
	done, err := restarted.CompareLaunch(argv, []string{"PATH=/a", "SECRET_TOKEN=y"}, "shell", "", "exec")
	require.NoError(t, err)
	require.NotNil(t, done)
	require.Equal(t, "succeeded", done.State, done.Error)
	assert.Equal(t, "differs", done.Launch.State)
	assert.Equal(t, "passed", done.Launch.ArgsCheck, "the pre-flight result survives the comparison")
	assert.Equal(t, []LaunchDifference{{Kind: "env", Change: "changed", Name: "SECRET_TOKEN"}}, done.Launch.Differences)
	assert.Contains(t, strings.Join(done.Warnings, "\n"), "environment SECRET_TOKEN changed")
	_, err = os.Stat(restarted.launchPath(job.ID))
	assert.True(t, os.IsNotExist(err), "the hashes are dropped once compared")
	again, err := restarted.CompareLaunch(argv, nil, "shell", "", "exec")
	require.NoError(t, err)
	assert.Nil(t, again, "compared once")
}

// A restart that refreshed the environment from the login shell on purpose
// compares only tclaude's own variables: a new PATH or profile variable is
// the refresh, not a difference, while a lost tclaude marker still is.
func TestRestartedServiceComparesOnlyTclaudesEnvAfterARefresh(t *testing.T) {
	dir := testutil.CanonicalTempDir(t)
	bin := filepath.Join(dir, "bin", "tclaude")
	writeBin(t, bin, "v1.0.0")
	s := versionedService(t, dir, []Binary{{Name: "tclaude", Path: bin, Version: "v1.0.0", Method: "release"}}, "v1.1.0")
	s.hooks.Restart = func() error { return nil }
	argv := []string{bin, "agentd", "serve"}
	snap, err := CaptureLaunch(argv, []string{"PATH=/go-1.27.3/bin", "GOROOT=/go-1.27.3", "TCLAUDE_A=1", "TCLAUDE_B=1"}, "shell", "", "exec")
	require.NoError(t, err)
	job, err := s.StartWith(Request{Action: "apply"}, "operator", func() bool { return true }, ApplyOptions{Launch: &LaunchPlan{Snapshot: snap, Binary: "tclaude", Args: [][]string{argv[1:]}}})
	require.NoError(t, err)
	job = waitJob(t, s, job.ID)
	require.Equal(t, "restarting", job.State, job.Error)
	require.NoError(t, s.MarkLaunchEnvRefreshed())
	require.NoError(t, s.MarkLaunchEnvRefreshed(), "marking twice is harmless")

	restarted, err := New(filepath.Join(dir, "updates"), []Binary{{Name: "tclaude", Path: bin, Version: "v1.1.0", Method: "release"}}, Hooks{})
	require.NoError(t, err)
	done, err := restarted.CompareLaunch(argv, []string{"PATH=/go-1.27.4/bin", "EDITOR=vim", "TCLAUDE_A=1"}, "shell", "", "exec")
	require.NoError(t, err)
	require.NotNil(t, done)
	assert.True(t, done.Launch.EnvRefreshed)
	assert.Equal(t, []LaunchDifference{{Kind: "env", Change: "removed", Name: "TCLAUDE_B"}}, done.Launch.Differences)
}

func TestCompareLaunchAfterARefreshSaysSo(t *testing.T) {
	argv := []string{"/usr/bin/tclaude", "agentd", "serve"}
	before, err := CaptureLaunch(argv, []string{"PATH=/a", "TCLAUDE_X=1"}, "shell", "", "exec")
	require.NoError(t, err)
	before.EnvRefreshed = true
	check := CompareLaunch(before, argv, []string{"PATH=/b", "NEW=1", "TCLAUDE_X=1"}, "shell", "", "exec", nil)
	assert.Equal(t, "identical", check.State, check.Differences)
	assert.Contains(t, check.Summary(), "refreshed from the login shell")
}
