package selfupdate

import (
	"context"
	"fmt"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/common/buildversion"
	"github.com/tofutools/tclaude/pkg/testutil"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

type fakeHealthChild struct{ path string }

func (c *fakeHealthChild) Stop(context.Context) error { return nil }
func (c *fakeHealthChild) Healthy(ctx context.Context, _ string) error {
	return exec.CommandContext(ctx, c.path).Run()
}
func TestSupervisedGuardianRollsBackFailedBinaryHealth(t *testing.T) {
	dir := testutil.CanonicalTempDir(t)
	binary := Binary{Name: "tclaude-hub", Path: filepath.Join(dir, "hub"), Version: "v1.0.0", Method: "release"}
	old := []byte("#!/bin/sh\nexit 0\n")
	require.NoError(t, os.WriteFile(binary.Path, old, 0700))
	outcome := make(chan Job, 2)
	g, err := NewGuardian(context.Background(), filepath.Join(dir, "updates"), binary, func(_ context.Context, path string) (SupervisedChild, error) {
		return &fakeHealthChild{path: path}, nil
	}, func(j Job) { outcome <- j })
	require.NoError(t, err)
	defer g.Close()
	require.NoError(t, g.Start())
	s := g.Service()
	s.release = func(context.Context, string) (Release, error) { return Release{Tag: "v1.1.0"}, nil }
	s.stageRelease = func(_ context.Context, _ Release, name, dir string) (string, error) {
		path := filepath.Join(dir, name)
		return path, os.WriteFile(path, []byte("#!/bin/sh\nexit 9\n"), 0700)
	}
	s.inspect = func(context.Context, string) (buildversion.Info, error) {
		return buildversion.Info{Version: "v1.1.0", ProtocolVersion: s.status.ProtocolVersion}, nil
	}
	_, err = s.Start(Request{Action: "apply", Version: "v1.1.0"}, "operator", func() bool { return true })
	require.NoError(t, err)
	select {
	case j := <-outcome:
		require.Equal(t, "rolled_back", j.State, j.Error)
		require.True(t, j.RolledBack)
		require.Contains(t, j.Error, "health check")
	case <-time.After(5 * time.Second):
		t.Fatal("guardian failed to recover")
	}
	raw, err := os.ReadFile(binary.Path)
	require.NoError(t, err)
	require.Equal(t, old, raw)
	require.Equal(t, "v1.0.0", s.Status().CurrentVersion)
	reloaded, err := New(s.dir, []Binary{binary}, Hooks{Supervised: true})
	require.NoError(t, err)
	require.Equal(t, "rolled_back", reloaded.Status().Job.State)
}
func TestSupervisedUpdateRejectsExplicitAndResolvedDowngrades(t *testing.T) {
	s, _ := fixture(t)
	s.hooks.NoDowngrade = true
	s.status.CurrentVersion = "v1.1.0"
	_, err := s.Start(Request{Action: "apply", Version: "v1.0.0"}, "operator", func() bool { return true })
	require.ErrorContains(t, err, "downgrades")
	s.release = func(context.Context, string) (Release, error) { return Release{Tag: "v1.0.0"}, nil }
	j, err := s.Start(Request{Action: "apply"}, "operator", func() bool { return true })
	require.NoError(t, err)
	require.Contains(t, waitJob(t, s, j.ID).Error, "downgrades")
}

func TestSupervisedGuardianFailedManualRollbackRestoresWorkingRelease(t *testing.T) {
	dir := testutil.CanonicalTempDir(t)
	binary := Binary{Name: "tclaude-hub", Path: filepath.Join(dir, "hub"), Version: "v1.0.0", Method: "release"}
	require.NoError(t, os.WriteFile(binary.Path, []byte("#!/bin/sh\nexit 0\n"), 0700))
	events := make(chan Job, 4)
	refuseOld := false
	g, err := NewGuardian(context.Background(), filepath.Join(dir, "updates"), binary, func(_ context.Context, path string) (SupervisedChild, error) {
		return &versionHealthChild{path: path, refuse: &refuseOld}, nil
	}, func(j Job) { events <- j })
	require.NoError(t, err)
	defer g.Close()
	require.NoError(t, g.Start())
	s := g.Service()
	s.release = func(context.Context, string) (Release, error) { return Release{Tag: "v1.1.0"}, nil }
	upgraded := []byte("#!/bin/sh\n# newer\nexit 0\n")
	s.stageRelease = func(_ context.Context, _ Release, name, dir string) (string, error) {
		path := filepath.Join(dir, name)
		return path, os.WriteFile(path, upgraded, 0700)
	}
	s.inspect = func(context.Context, string) (buildversion.Info, error) {
		return buildversion.Info{Version: "v1.1.0", ProtocolVersion: s.status.ProtocolVersion}, nil
	}
	_, err = s.Start(Request{Action: "apply", Version: "v1.1.0"}, "operator", func() bool { return true })
	require.NoError(t, err)
	select {
	case j := <-events:
		require.Equal(t, "completed", j.State, j.Error)
	case <-time.After(5 * time.Second):
		t.Fatal("upgrade did not finish")
	}
	require.Equal(t, "v1.1.0", s.Status().CurrentVersion)
	_, err = s.Start(Request{Action: "apply", Version: "v1.0.0"}, "operator", func() bool { return true })
	require.ErrorContains(t, err, "downgrades")
	refuseOld = true
	_, err = s.Start(Request{Action: "rollback"}, "operator", func() bool { return true })
	require.NoError(t, err)
	select {
	case j := <-events:
		require.Equal(t, "rolled_back", j.State, j.Error)
	case <-time.After(5 * time.Second):
		t.Fatal("failed rollback did not recover")
	}
	raw, err := os.ReadFile(binary.Path)
	require.NoError(t, err)
	require.Equal(t, upgraded, raw)
	require.Equal(t, "v1.1.0", s.Status().CurrentVersion)
}

type versionHealthChild struct {
	path   string
	refuse *bool
}

func (c *versionHealthChild) Stop(context.Context) error { return nil }
func (c *versionHealthChild) Healthy(ctx context.Context, version string) error {
	if *c.refuse && version == "v1.0.0" {
		return fmt.Errorf("old release no longer healthy")
	}
	return exec.CommandContext(ctx, c.path).Run()
}

type unhealthyLiveChild struct{ done chan struct{} }

func (c *unhealthyLiveChild) Stop(context.Context) error { return nil } // stuck process: fatal channel still releases Wait
func (c *unhealthyLiveChild) Done() <-chan struct{}      { return c.done }
func (c *unhealthyLiveChild) Healthy(context.Context, string) error {
	return fmt.Errorf("listener unhealthy while process remains alive")
}
func TestSupervisedGuardianFatalRestoredHealthExitsWait(t *testing.T) {
	s, binaries := fixture(t)
	g, err := NewGuardian(context.Background(), s.dir, binaries[0], func(context.Context, string) (SupervisedChild, error) {
		return &unhealthyLiveChild{done: make(chan struct{})}, nil
	}, nil)
	require.NoError(t, err)
	g.child = &unhealthyLiveChild{done: make(chan struct{})}
	g.service.release = s.release
	g.service.stageRelease = s.stageRelease
	g.service.inspect = s.inspect
	_, err = g.service.Start(Request{Action: "apply", Version: "v1.1.0"}, "operator", func() bool { return true })
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.ErrorContains(t, g.Wait(ctx), "health")
	require.Equal(t, "failed", g.service.Pending().State)
}

func TestSupervisedGuardianRecoversDurableRestartAfterGuardianExit(t *testing.T) {
	s, binaries := fixture(t)
	s.binaries = s.binaries[:1]
	s.hooks.Supervised = true
	s.hooks.ReleaseOnly = true
	job, err := s.Start(Request{Action: "apply", Version: "v1.1.0"}, "operator", func() bool { return true })
	require.NoError(t, err)
	require.Equal(t, "restarting", waitJob(t, s, job.ID).State)
	binary := binaries[0]
	binary.Version = "v1.1.0" // the newly installed image starts the next guardian
	g, err := NewGuardian(context.Background(), s.dir, binary, func(context.Context, string) (SupervisedChild, error) { return &recoveryHealthChild{}, nil }, nil)
	require.NoError(t, err)
	defer g.Close()
	require.NoError(t, g.Start())
	require.Equal(t, "rolled_back", g.Service().Pending().State)
	require.Equal(t, "v1.0.0", g.Service().Status().CurrentVersion)
	raw, err := os.ReadFile(binary.Path)
	require.NoError(t, err)
	require.Equal(t, "old-"+binary.Name, string(raw))
}

type recoveryHealthChild struct{}

func (*recoveryHealthChild) Stop(context.Context) error { return nil }
func (*recoveryHealthChild) Healthy(_ context.Context, version string) error {
	if version == "v1.1.0" {
		return fmt.Errorf("newly started guardian observes unhealthy candidate")
	}
	return nil
}

func TestUpdateAdmissionReconcilesOutcomeWhileFinishedCallbackIsBlocked(t *testing.T) {
	s, _ := fixture(t)
	finished := make(chan Job, 1)
	releaseCallback := make(chan struct{})
	s.hooks.Finished = func(j Job) { finished <- j; <-releaseCallback }
	defer close(releaseCallback)
	old, err := s.Start(Request{Action: "check"}, "operator", func() bool { return true })
	require.NoError(t, err)
	select {
	case j := <-finished:
		require.Equal(t, old.ID, j.ID)
	case <-time.After(5 * time.Second):
		t.Fatal("finished callback not reached")
	}
	// The old async callback is blocked and has not persisted an outcome. Even
	// though active is cleared, admission must reconcile it before replacement.
	blocked := true
	reconciled := false
	s.hooks.BeforeStart = func(j Job) error {
		require.Equal(t, old.ID, j.ID)
		require.Equal(t, "succeeded", j.State)
		if blocked {
			return fmt.Errorf("audit unavailable")
		}
		reconciled = true
		return nil
	}
	_, err = s.Start(Request{Action: "check"}, "operator", func() bool { return true })
	require.ErrorContains(t, err, "audit unavailable")
	require.Equal(t, old.ID, s.Pending().ID)
	blocked = false
	next, err := s.Start(Request{Action: "check"}, "operator", func() bool { return true })
	require.NoError(t, err)
	require.True(t, reconciled)
	require.NotEqual(t, old.ID, next.ID)
	select {
	case j := <-finished:
		require.Equal(t, next.ID, j.ID)
	case <-time.After(5 * time.Second):
		t.Fatal("replacement check did not finish")
	}
}
