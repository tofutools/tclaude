//go:build linux || darwin

package host_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

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
	require.NotNil(t, stopped.Execution)
	// Stop admission can precede observed process exit under load.
	require.Eventually(t, func() bool {
		observed, observeErr := service.Observe(context.Background(), app.ObserveRequest{Principal: operator, ExecutionID: started.Execution.ID})
		return observeErr == nil && observed.Execution.State == model.ExecutionExited
	}, 15*time.Second, 20*time.Millisecond, "accepted shell stop must reach observed exit")
}

func TestSQLiteJourneyRestoresRetainedCommittedCheckout(t *testing.T) {
	repository := t.TempDir()
	runGit(t, repository, "init", "-b", "main")
	runGit(t, repository, "config", "user.name", "Restore Journey Test")
	runGit(t, repository, "config", "user.email", "restore@example.invalid")
	require.NoError(t, os.WriteFile(filepath.Join(repository, "README.md"), []byte("initial\n"), 0o600))
	runGit(t, repository, "add", "README.md")
	runGit(t, repository, "commit", "-m", "initial")

	checkoutHost, err := backendhost.NewCheckoutHost("git")
	require.NoError(t, err)
	store, err := backendsqlite.Open(filepath.Join(t.TempDir(), "journey.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	service := app.New(store, providers.NewRegistry()).WithWorkspaceHost(checkoutHost)
	operator := model.OperatorPrincipal()
	checkoutPath := filepath.Join(t.TempDir(), "checkout")
	workspace, err := service.CreateCheckout(context.Background(), app.CreateCheckoutRequest{
		Context: app.RequestContext{Principal: operator, RequestID: "create_restore_workspace"}, ID: "workspace_restore",
		Intent: model.WorkspaceIntent{Repository: repository, IntendedPath: checkoutPath,
			BaseRevision: "HEAD", Branch: "feature/restore", Provenance: model.WorkspacePlatformCreated, Ownership: model.WorkspaceOwned},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(checkoutPath, "worker.txt"), []byte("committed worker result\n"), 0o600))
	runGit(t, checkoutPath, "add", "worker.txt")
	runGit(t, checkoutPath, "commit", "-m", "worker result")

	inspected, err := service.InspectWorkspace(context.Background(), app.InspectWorkspaceRequest{
		Principal: operator, WorkspaceID: workspace.Workspace.ID,
	})
	require.NoError(t, err)
	require.NotEqual(t, workspace.Workspace.Observation.Revision, inspected.Workspace.Observation.Revision)
	removed, err := service.RemoveCheckout(context.Background(), app.RemoveCheckoutRequest{
		Context:     app.RequestContext{Principal: operator, RequestID: "remove_restore_workspace"},
		WorkspaceID: workspace.Workspace.ID, ExpectedRevision: inspected.Workspace.Revision,
	})
	require.NoError(t, err)
	require.Equal(t, model.WorkspaceRemoved, removed.Workspace.State)
	require.Equal(t, inspected.Workspace.Observation.Revision, removed.Workspace.Observation.Revision)
	require.NoDirExists(t, checkoutPath)

	restored, err := service.RestoreCheckout(context.Background(), app.RestoreCheckoutRequest{
		Context:     app.RequestContext{Principal: operator, RequestID: "restore_workspace"},
		WorkspaceID: workspace.Workspace.ID, ExpectedRevision: removed.Workspace.Revision,
	})
	require.NoError(t, err)
	require.Equal(t, model.WorkspaceAvailable, restored.Workspace.State)
	require.Equal(t, inspected.Workspace.Observation.Revision, restored.Workspace.Observation.Revision)
	require.FileExists(t, filepath.Join(checkoutPath, "worker.txt"))
}

func runGit(t *testing.T, directory string, args ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", directory}, args...)...)
	output, err := command.CombinedOutput()
	require.NoError(t, err, "%s", output)
}
