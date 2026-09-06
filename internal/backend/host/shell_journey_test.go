//go:build linux || darwin

package host_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	backendhost "github.com/tofutools/tclaude/internal/backend/host"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers"
	backendsqlite "github.com/tofutools/tclaude/internal/backend/sqlite"
)

func TestSQLiteJourneyUsesRealCheckoutAndShellHosts(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is unavailable")
	}
	repository := t.TempDir()
	runGit(t, repository, "init", "-b", "main")
	runGit(t, repository, "config", "user.name", "Shell Journey Test")
	runGit(t, repository, "config", "user.email", "shell@example.invalid")
	require.NoError(t, os.WriteFile(filepath.Join(repository, "README.md"), []byte("initial\n"), 0o600))
	runGit(t, repository, "add", "README.md")
	runGit(t, repository, "commit", "-m", "initial")

	checkoutHost, err := backendhost.NewCheckoutHost("git")
	require.NoError(t, err)
	terminalRoot, err := os.MkdirTemp("/tmp", "tclaude-shell-journey-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(terminalRoot)) })
	shellHost, err := backendhost.NewShellHost(backendhost.ShellConfig{
		Terminal: backendhost.TerminalHost{PrivateRoot: terminalRoot}, Executable: "/bin/sh",
	})
	require.NoError(t, err)
	store, err := backendsqlite.Open(filepath.Join(t.TempDir(), "journey.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	service := app.New(store, providers.NewRegistry()).WithWorkspaceHost(checkoutHost).WithShellHost(shellHost)
	operator := model.OperatorPrincipal()
	workspace, err := service.CreateCheckout(context.Background(), app.CreateCheckoutRequest{
		Context: app.RequestContext{Principal: operator, RequestID: "create_workspace"}, ID: "workspace_shell",
		Intent: model.WorkspaceIntent{Repository: repository, IntendedPath: filepath.Join(t.TempDir(), "checkout"),
			BaseRevision: "HEAD", Branch: "feature/shell", Provenance: model.WorkspacePlatformCreated, Ownership: model.WorkspaceOwned},
	})
	require.NoError(t, err)
	started, err := service.StartShell(context.Background(), app.StartShellRequest{
		Context:     app.RequestContext{Principal: operator, RequestID: "start_shell"},
		WorkspaceID: workspace.Workspace.ID, ExpectedRevision: workspace.Workspace.Revision, Sandbox: model.SandboxUnconfined,
	})
	require.NoError(t, err)
	require.Equal(t, model.ExecutionRunning, started.Execution.State)
	stopped, err := service.Stop(context.Background(), app.StopRequest{RequestContext: app.RequestContext{
		Principal: operator, RequestID: "stop_shell"}, ExecutionID: started.Execution.ID, Force: true})
	require.NoError(t, err)
	require.Equal(t, model.ExecutionExited, stopped.Execution.State)
}

func runGit(t *testing.T, directory string, args ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", directory}, args...)...)
	output, err := command.CombinedOutput()
	require.NoError(t, err, "%s", output)
}
