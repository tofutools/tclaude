package agentd_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/selfupdate"
	"github.com/tofutools/tclaude/pkg/testutil"
)

// A scheduled update snapshots the daemon's flags and environment, and the
// restarted daemon reports what changed: on the job (CLI/API/dashboard) and
// in the operator's notification. Values never appear.
func TestSelfUpdateKeepsTrackOfDaemonFlagsAndEnvironment(t *testing.T) {
	f := newFlow(t)
	host := &selfUpdateHost{}
	host.install(t)
	dir := testutil.CanonicalTempDir(t)
	bin := fakeBinary(t, dir, "bin/tclaude", "v1.0.0")
	svc := selfUpdateService(t, dir, []selfupdate.Binary{{Name: "tclaude", Path: bin, Version: "v1.0.0", Method: "release"}})
	var checked [][]string
	svc.SetFakesForTest(selfupdate.TestFakes{CheckArgs: func(_ context.Context, path string, args []string) (string, error) {
		checked = append(checked, args)
		return "passed", nil
	}})
	window := time.Date(2026, 11, 9, 4, 0, 0, 0, time.Local)
	t.Cleanup(agentd.SetSelfUpdateNowForTest(window))
	argv := []string{bin, "agentd", "serve", "--no-tray", "--dashboard-port", "8123"}
	t.Cleanup(agentd.SetSelfUpdateProcessForTest(argv, []string{"PATH=/usr/bin", "ANTHROPIC_API_KEY=sk-before", "LANG=C", "INVOCATION_ID=a"}))
	putSettings(t, f, map[string]any{"settings": map[string]any{
		"method":   "release",
		"schedule": map[string]any{"frequency": "daily", "window_start": "03:00", "window_end": "05:00"},
	}}, 200)

	agentd.SelfUpdateScheduleTickForTest(window)
	fedEventuallyWithin(t, "scheduled job to restart", 5*time.Second, func() bool {
		j := svc.Status().Job
		return j != nil && j.State == "restarting"
	})
	job := svc.Status().Job
	require.Equal(t, "recorded", job.Launch.State)
	require.Equal(t, "passed", job.Launch.ArgsCheck)
	require.Equal(t, [][]string{argv[1:]}, checked, "the staged binary was asked about the daemon's own flags")
	require.Equal(t, 3, job.Launch.EnvKeys, "volatile per-start variables are not tracked")

	// The restarted daemon: a flag gone, the API key and LANG different.
	t.Cleanup(agentd.SetSelfUpdateProcessForTest(argv[:4], []string{"PATH=/usr/bin", "ANTHROPIC_API_KEY=sk-after", "INVOCATION_ID=b"}))
	restarted := selfUpdateService(t, dir, []selfupdate.Binary{{Name: "tclaude", Path: bin, Version: "v1.1.0", Method: "release"}})
	agentd.FinishRestartedUpdateForTest(restarted)

	rec := fedHuman(t, f, "GET", "/v1/node/update/jobs/"+job.ID, nil)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	body := rec.Body.String()
	for _, secret := range []string{"sk-before", "sk-after", "salt", "argv_hashes"} {
		require.NotContains(t, body, secret)
	}
	done, err := restarted.Job(job.ID)
	require.NoError(t, err)
	require.Equal(t, "succeeded", done.State, done.Error)
	require.Equal(t, "differs", done.Launch.State)
	require.Equal(t, []selfupdate.LaunchDifference{
		{Kind: "flag", Change: "removed", Name: "--dashboard-port", Before: "--dashboard-port=8123"},
		{Kind: "env", Change: "changed", Name: "ANTHROPIC_API_KEY"},
		{Kind: "env", Change: "removed", Name: "LANG"},
	}, done.Launch.Differences)

	msgs, err := db.ListHumanMessagesPage(db.HumanMessageFilter{}, 0, 10)
	require.NoError(t, err)
	var note string
	for _, m := range msgs {
		if strings.HasPrefix(m.Subject, "tclaude updated to") {
			note = m.Body
		}
	}
	require.Contains(t, note, "3 difference(s) after the restart")
	require.Contains(t, note, "environment ANTHROPIC_API_KEY changed")
	require.NotContains(t, note, "sk-")
}

// An operator's apply whose new binary rejects the running daemon's flags is
// refused before anything is replaced; --force goes past it.
func TestSelfUpdateRefusesABinaryThatRejectsTheDaemonFlags(t *testing.T) {
	f := newFlow(t)
	dir := testutil.CanonicalTempDir(t)
	bin := fakeBinary(t, dir, "bin/tclaude", "v1.0.0")
	svc := selfUpdateService(t, dir, []selfupdate.Binary{{Name: "tclaude", Path: bin, Version: "v1.0.0", Method: "release"}})
	svc.SetFakesForTest(selfupdate.TestFakes{CheckArgs: func(_ context.Context, _ string, args []string) (string, error) {
		return "", errors.New("unknown flag: --wizard")
	}})
	t.Cleanup(agentd.SetSelfUpdateProcessForTest([]string{bin, "agentd", "serve", "--wizard"}, []string{"PATH=/usr/bin"}))
	require.NoError(t, config.Save(&config.Config{Updates: &config.UpdatesConfig{Self: &config.SelfUpdateConfig{Method: "release"}}}))

	rec := fedHuman(t, f, "POST", "/v1/node/update", map[string]any{"action": "apply"})
	require.Equal(t, 202, rec.Code, rec.Body.String())
	var job selfupdate.Job
	fedEventuallyWithin(t, "apply to fail", 5*time.Second, func() bool {
		j := svc.Status().Job
		if j != nil && j.State != "running" {
			job = *j
			return true
		}
		return false
	})
	require.Equal(t, "failed", job.State)
	require.Contains(t, job.Error, "would not start with the daemon's current flags: unknown flag: --wizard")
	raw, err := os.ReadFile(bin)
	require.NoError(t, err)
	require.Equal(t, "v1.0.0", string(raw), "nothing was replaced")

	rec = fedHuman(t, f, "POST", "/v1/node/update", map[string]any{"action": "check", "force": true})
	require.Equal(t, 400, rec.Code, "force is for apply only")
	rec = fedHuman(t, f, "POST", "/v1/node/update", map[string]any{"action": "apply", "force": true})
	require.Equal(t, 202, rec.Code, rec.Body.String())
	fedEventuallyWithin(t, "forced apply to replace", 5*time.Second, func() bool {
		j := svc.Status().Job
		return j != nil && j.State != "running" && j.ID != job.ID
	})
	forced := svc.Status().Job
	require.NotEqual(t, "failed", forced.State, forced.Error)
	require.Equal(t, "forced", forced.Launch.ArgsCheck)
}
