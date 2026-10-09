package harnessops

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/harnesscredentials"
	"github.com/tofutools/tclaude/pkg/nodeinfo"
	"github.com/tofutools/tclaude/pkg/testutil"
)

func fixture(t *testing.T) (*Service, *atomic.Bool) {
	t.Helper()
	dir := testutil.CanonicalTempDir(t)
	busy := new(atomic.Bool)
	s, err := New(filepath.Join(dir, "jobs"), dir, Hooks{Busy: func(string) (bool, error) { return busy.Load(), nil }, Probe: func() nodeinfo.Availability {
		return nodeinfo.Availability{Schema: 1, Harnesses: []nodeinfo.HarnessAvailability{{Name: "codex", Installed: true}, {Name: "shell", Installed: true}}}
	}})
	require.NoError(t, err)
	t.Cleanup(s.Close)
	s.plan = func(context.Context, string, string, string) (Command, error) { return Command{Path: "fake"}, nil }
	s.execute = func(context.Context, Command) error { return nil }
	return s, busy
}
func waitJob(t *testing.T, s *Service, id string) Job {
	t.Helper()
	var j Job
	require.Eventually(t, func() bool {
		s.mu.Lock()
		active := s.active
		s.mu.Unlock()
		if active {
			return false
		}
		var err error
		j, err = s.Job(id)
		return err == nil
	}, 3*time.Second, time.Millisecond)
	return j
}
func TestHarnessOperationsIdleAndExplicitNow(t *testing.T) {
	s, busy := fixture(t)
	busy.Store(true)
	_, err := s.Start(Request{Action: "update", Harness: "codex"}, "operator", func(bool) bool { return true })
	require.ErrorIs(t, err, ErrWorkersBusy)
	j, err := s.Start(Request{Action: "update", Harness: "codex", Mode: "now"}, "operator", func(bool) bool { return true })
	require.NoError(t, err)
	j = waitJob(t, s, j.ID)
	require.Equal(t, "succeeded", j.State)
	require.NotEmpty(t, j.Warnings)
	busy.Store(false)
	j, err = s.Start(Request{Action: "update", All: true, Mode: "when_idle"}, "operator", func(bool) bool { return true })
	require.NoError(t, err)
	j = waitJob(t, s, j.ID)
	require.Equal(t, []string{"codex"}, j.Harnesses)
	require.Equal(t, "succeeded", j.State)
}
func TestHarnessOperationsCredentialGrantAndSecretFreeJob(t *testing.T) {
	s, _ := fixture(t)
	bundle := harnesscredentials.Bundle{Harness: "codex", Files: []harnesscredentials.File{{Name: "auth.json", Data: []byte(`{"token":"SECRET-MUST-NOT-PERSIST"}`)}}}
	req := Request{Action: "install", Harness: "codex", CopyCredentials: true, Credentials: &bundle}
	_, err := s.Start(req, "peer", func(copy bool) bool { return !copy })
	require.Error(t, err)
	copied := false
	s.receive = func(string, string, harnesscredentials.Bundle, bool, func() bool) (harnesscredentials.Receipt, error) {
		copied = true
		return harnesscredentials.Receipt{BackupID: "backup", Copied: true}, nil
	}
	j, err := s.Start(req, "peer", func(bool) bool { return true })
	require.NoError(t, err)
	j = waitJob(t, s, j.ID)
	require.Equal(t, "succeeded", j.State)
	require.True(t, copied)
	raw, err := os.ReadFile(filepath.Join(s.dir, j.ID+".json"))
	require.NoError(t, err)
	require.NotContains(t, string(raw), "SECRET-MUST-NOT-PERSIST")
	require.NotContains(t, string(raw), "data")
}
func TestHarnessOperationsRevocationAndRestart(t *testing.T) {
	s, _ := fixture(t)
	allowed := new(atomic.Bool)
	allowed.Store(true)
	s.plan = func(context.Context, string, string, string) (Command, error) {
		allowed.Store(false)
		return Command{}, nil
	}
	ran := false
	s.execute = func(context.Context, Command) error { ran = true; return nil }
	j, err := s.Start(Request{Action: "update", Harness: "codex"}, "peer", func(bool) bool { return allowed.Load() })
	require.NoError(t, err)
	require.Equal(t, "failed", waitJob(t, s, j.ID).State)
	require.False(t, ran)
	j.State = "running"
	require.NoError(t, s.save(j))
	recovered, err := New(s.dir, s.home, s.hooks)
	require.NoError(t, err)
	defer recovered.Close()
	j, err = recovered.Job(j.ID)
	require.NoError(t, err)
	require.Equal(t, "failed", j.State)
	require.Contains(t, j.Error, "restarted")
}
func TestHarnessRequestValidationAndFixedRecipes(t *testing.T) {
	for _, r := range []Request{{Action: "install", Harness: "codex;touch bad"}, {Action: "update", All: true, Harness: "codex"}, {Action: "install", All: true}, {Action: "update", Harness: "codex", CopyCredentials: true}, {Action: "install", Harness: "codex", Mode: "later"}} {
		require.Error(t, r.Validate(false))
	}
	var req Request
	require.NoError(t, json.Unmarshal([]byte(`{"action":"install","harness":"codex","credentials":{"harness":"codex","files":[]}}`), &req))
	require.Error(t, req.Validate(false))
	for _, r := range Recipes() {
		require.Equal(t, "latest", r.Channel)
		require.True(t, strings.HasPrefix(r.Documentation, "https://"))
		require.NotContains(t, r.InstallCommand, "sudo")
	}
}
