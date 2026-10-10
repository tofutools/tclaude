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
