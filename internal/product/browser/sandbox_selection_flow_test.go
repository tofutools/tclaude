package browser

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

// Only the external native host is substituted. Selection, receipt, projection,
// immutable revisions and browser input use the production application/store.
type selectionShellHost struct {
	ports.ShellHost
	requests chan ports.ShellPreparationRequest
}

func (h *selectionShellHost) PrepareShell(_ context.Context, req ports.ShellPreparationRequest) (ports.PreparedShell, error) {
	h.requests <- req
	return nil, app.ErrUnavailable
}

func TestBrowserShellSandboxSelectionPinsRevisionAndSurvivesRetry(t *testing.T) {
	if os.Getenv("TCLAUDE_BROWSER_SMOKE") != "1" {
		t.Skip("set TCLAUDE_BROWSER_SMOKE=1")
	}
	shell := &selectionShellHost{requests: make(chan ports.ShellPreparationRequest, 2)}
	ctx, page, operator := processEditorBrowserConfigured(t, nil, nil, shell)
	repo := filepath.Join(t.TempDir(), "checkout")
	for _, args := range [][]string{{"init", "-b", "main", repo}, {"-C", repo, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "--allow-empty", "-m", "base"}} {
		output, err := exec.Command("git", args...).CombinedOutput()
		require.NoError(t, err, "%s", output)
	}
	var profile app.SandboxProfileResult
	require.NoError(t, operator.Call(ctx, "POST", "/v2/sandbox-profiles", map[string]any{"request_id": "policy", "id": "policy", "name": "Selected policy", "policy": model.SandboxPolicy{FilesystemRoot: model.SandboxRootSeparate}}, &profile))
	page.MustElement("[data-tab=workspaces]").MustClick()
	page.MustElement("#register-workspace").MustClick()
	page.MustElement("#editor [name=path]").MustInput(repo)
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	page.MustElementR("#workspace-list button", "^Open shell$").MustClick()
	require.Contains(t, page.MustElement("#editor option[value='profile:policy']").MustText(), string(profile.Revision.Ref.RevisionID))
	page.MustElement("#editor [name=host_sandbox]").MustSelect("Selected policy · policy · " + string(profile.Revision.Ref.RevisionID))
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor-error").MustWaitVisible()
	var prepared ports.ShellPreparationRequest
	select {
	case prepared = <-shell.requests:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	require.Equal(t, profile.Revision.Ref, prepared.HostSandbox.Scopes[0].Ref)
	require.Equal(t, prepared.HostSandbox.PolicyHash, prepared.HostSandboxPolicy.ContentHash)
	// A subsequent catalog change must not rewrite the submitted retry intent.
	require.NoError(t, operator.Call(ctx, "POST", "/v2/sandbox-profiles", map[string]any{"request_id": "changed-policy", "id": "policy", "expected_revision": profile.Profile.Revision, "name": "Changed policy", "policy": model.SandboxPolicy{FilesystemRoot: model.SandboxRootSeparate, Environment: model.Environment{"VALUE": "new"}}}, nil))
	page.MustWait(`() => !submitting`)
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	var snapshot struct {
		Executions []struct {
			Spec model.ResolvedExecutionSpec `json:"spec"`
		} `json:"executions"`
	}
	require.NoError(t, operator.Call(ctx, "GET", "/v2/snapshot", nil, &snapshot))
	require.Len(t, snapshot.Executions, 1)
	require.Equal(t, prepared.HostSandbox, snapshot.Executions[0].Spec.HostSandbox)
	require.Empty(t, shell.requests, "retry must not prepare another native child")
}
