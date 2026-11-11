package selfupdate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/testutil"
)

// agentd's environment is the one it started with: a GOROOT from before
// a Go upgrade. The custom command runs with the fresh terminal
// environment instead, plus the TCLAUDE_UPDATE_* variables.
func TestCustomCommandRunsWithTheFreshEnvironment(t *testing.T) {
	t.Setenv("GOROOT", "/opt/homebrew/Cellar/go/1.27.3/libexec")
	var env []string
	var bin string
	s, _, bin := customService(t, func(_, e []string) ([]byte, error) {
		env = e
		writeBin(t, bin, "v1.1.0")
		return nil, nil
	})
	calls := 0
	s.hooks.ToolEnv = func(context.Context) ([]string, error) {
		calls++
		return []string{"HOME=/h", "PATH=/opt/homebrew/bin:/usr/bin", "FROM_SHELL=yes"}, nil
	}
	job := applyCustom(t, s, ApplyOptions{})
	require.Equal(t, "restarting", job.State, job.Error)
	require.Contains(t, env, "FROM_SHELL=yes")
	require.Contains(t, env, "TCLAUDE_UPDATE_VERSION=v1.1.0")
	for _, kv := range env {
		require.False(t, strings.HasPrefix(kv, "GOROOT="), "stale %s reached the command", kv)
	}
	require.Equal(t, 1, calls)
	require.NotContains(t, strings.Join(job.Warnings, "\n"), "login shell")
}

// A login shell that does not answer leaves agentd's environment, and the
// job says so.
func TestCustomCommandFallsBackWithAWarning(t *testing.T) {
	t.Setenv("GOROOT", "/stale")
	var env []string
	var bin string
	s, _, bin := customService(t, func(_, e []string) ([]byte, error) {
		env = e
		writeBin(t, bin, "v1.1.0")
		return nil, nil
	})
	s.hooks.ToolEnv = func(context.Context) ([]string, error) {
		return nil, errors.New("/bin/zsh did not finish within 30s")
	}
	job := applyCustom(t, s, ApplyOptions{})
	require.Equal(t, "restarting", job.State, job.Error)
	require.Contains(t, env, "GOROOT=/stale")
	require.Contains(t, env, "TCLAUDE_UPDATE_VERSION=v1.1.0")
	require.Contains(t, strings.Join(job.Warnings, "\n"), "could not get a fresh environment from your login shell (/bin/zsh did not finish within 30s)")
}

// The checkout build runs with the job's environment, GOBIN and GOWORK
// set over it, and go is found on that environment's PATH.
func TestBuildCheckoutUsesTheGivenEnvironment(t *testing.T) {
	t.Setenv("GOROOT", "/stale")
	dir := testutil.CanonicalTempDir(t)
	gobin := filepath.Join(dir, "go-bin")
	require.NoError(t, os.MkdirAll(gobin, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(gobin, "go"), []byte("#!/bin/sh\n"), 0o755))
	var env []string
	t.Cleanup(SetSystemForTest(System{Run: func(_ context.Context, argv, e []string) ([]byte, error) {
		env = e
		return nil, nil
	}}))
	s, _ := fixture(t)
	s.hooks.ToolEnv = func(context.Context) ([]string, error) {
		return []string{"PATH=" + gobin, "GOBIN=/elsewhere", "HOME=/h"}, nil
	}
	_, err := buildCheckout(context.Background(), dir, filepath.Join(dir, "out"), s.runner(&Job{ID: "j"}))
	require.NoError(t, err)
	got, ok := lookPathIn("go", env)
	require.True(t, ok)
	require.Equal(t, filepath.Join(gobin, "go"), got, "runFixed finds go on the fresh PATH")
	require.Equal(t, []string{"PATH=" + gobin, "HOME=/h", "GOBIN=" + filepath.Join(dir, "out"), "GOWORK=off"}, env)
}

// Every update method runs its tools (package manager, go, git, the custom
// command) with the fresh environment, never agentd's stale one. A method
// added to MethodIDs fails here until it says what it runs.
func TestEveryMethodRunsToolsWithTheFreshEnvironment(t *testing.T) {
	type setup struct {
		managedBy, pkg string
		runsTools      bool
	}
	cases := map[string]setup{
		MethodRelease:        {},
		MethodGoInstall:      {runsTools: true},
		MethodSourceCheckout: {runsTools: true},
		MethodHomebrew:       {managedBy: ManagerHomebrew, pkg: HomebrewFormula, runsTools: true},
		MethodNixProfile:     {managedBy: ManagerNixProfile, pkg: "tclaude", runsTools: true},
		MethodMise:           {managedBy: ManagerMise, pkg: "tclaude", runsTools: true},
		MethodApt:            {managedBy: ManagerDpkg, pkg: "tclaude", runsTools: true},
		MethodDnf:            {managedBy: ManagerRpm, pkg: "tclaude", runsTools: true},
		MethodCustom:         {runsTools: true},
		MethodManual:         {},
	}
	for _, id := range MethodIDs {
		c, ok := cases[id]
		if !ok {
			t.Errorf("method %s: say here whether and what it runs", id)
			continue
		}
		variants := []string{""}
		if id == MethodSourceCheckout {
			variants = append(variants, "main") // branch mode
		}
		for _, ref := range variants {
			t.Run(id+"/"+ref, func(t *testing.T) {
				t.Setenv("GOROOT", "/stale/go")
				dir := testutil.CanonicalTempDir(t)
				bin := filepath.Join(dir, "bin", "tclaude")
				writeBin(t, bin, "v1.0.0")
				var envs [][]string
				h := &fakeHost{path: map[string]string{"tclaude": bin}, run: func(_, e []string) ([]byte, error) {
					envs = append(envs, e)
					return nil, nil
				}}
				h.install(t)
				method := "release"
				if id == MethodGoInstall {
					method = "source"
				}
				s := versionedService(t, dir, []Binary{{Name: "tclaude", Path: bin, Version: "v1.0.0", Method: method, ManagedBy: c.managedBy, Package: c.pkg}}, "v1.1.0")
				// The real go and checkout steps, over the fake host.
				s.stageGo, s.stageCheckout, s.buildCheckout = stageGoInstall, stageCheckout, buildCheckout
				s.hooks.ToolEnv = func(context.Context) ([]string, error) {
					return []string{"PATH=/usr/bin:/bin", "FRESH=1"}, nil
				}
				opts := ApplyOptions{Method: id, CustomCommand: "my-installer", Checkout: CheckoutOptions{Path: dir, Ref: ref}, InstallDir: filepath.Join(dir, "install")}
				job, err := s.StartWith(Request{Action: "apply"}, "operator", func() bool { return true }, opts)
				if err == nil {
					waitJob(t, s, job.ID)
				}
				if c.runsTools {
					require.NotEmpty(t, h.ran, "%s ran no tool", id)
				} else {
					require.Empty(t, h.ran, "%s should run no tool", id)
				}
				for i, e := range envs {
					require.Contains(t, e, "FRESH=1", "%v ran without the fresh environment", h.ran[i])
					require.NotContains(t, e, "GOROOT=/stale/go", "%v saw agentd's stale GOROOT", h.ran[i])
				}
			})
		}
	}
}

// Test method says which environment an update would run with.
func TestPlanShowsTheToolEnvironment(t *testing.T) {
	s, _, _ := customService(t, nil)
	opts := ApplyOptions{Method: MethodCustom, CustomCommand: "my-installer"}
	check := func(p Plan) PlanCheck {
		for _, c := range p.Checks {
			if c.Name == "Environment" {
				return c
			}
		}
		t.Fatalf("no Environment check in %+v", p.Checks)
		return PlanCheck{}
	}
	s.hooks.ToolEnv = func(context.Context) ([]string, error) { return []string{"PATH=/usr/bin"}, nil }
	c := check(s.Plan(context.Background(), opts, PlanOptions{}))
	require.Equal(t, planOK, c.State)
	require.Contains(t, c.Detail, "fresh environment from your login shell")

	s.hooks.ToolEnv = func(context.Context) ([]string, error) { return nil, errors.New("/bin/zsh did not finish within 30s") }
	c = check(s.Plan(context.Background(), opts, PlanOptions{}))
	require.Equal(t, planWarn, c.State)
	require.Contains(t, c.Detail, "/bin/zsh did not finish within 30s")
	require.Contains(t, c.Detail, "agentd's environment")
	c = check(s.Plan(context.Background(), opts, PlanOptions{RedactCommand: true}))
	require.NotContains(t, c.Detail, "/bin/zsh", "a peer is not told why")
}
