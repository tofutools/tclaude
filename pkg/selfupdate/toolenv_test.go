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

// The checkout build runs the go of the given environment's PATH, with
// that environment, not this process's.
func TestBuildCheckoutUsesTheGivenEnvironment(t *testing.T) {
	t.Setenv("GOROOT", "/stale")
	dir := testutil.CanonicalTempDir(t)
	gobin := filepath.Join(dir, "go-bin")
	require.NoError(t, os.MkdirAll(gobin, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(gobin, "go"), []byte("#!/bin/sh\n"), 0o755))
	var argv0 string
	var env []string
	t.Cleanup(SetSystemForTest(System{Run: func(_ context.Context, argv, e []string) ([]byte, error) {
		argv0, env = argv[0], e
		return nil, nil
	}}))
	_, err := buildCheckout(context.Background(), dir, filepath.Join(dir, "out"), []string{"PATH=" + gobin, "GOBIN=/elsewhere", "HOME=/h"})
	require.NoError(t, err)
	require.Equal(t, filepath.Join(gobin, "go"), argv0)
	require.Equal(t, []string{"PATH=" + gobin, "HOME=/h", "GOBIN=" + filepath.Join(dir, "out"), "GOWORK=off"}, env)
}
