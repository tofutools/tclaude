package browser

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func TestBrowserRegistersExistingCheckoutWithoutTakingOwnership(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t)
	repo := filepath.Join(t.TempDir(), "existing")
	for _, args := range [][]string{{"init", "-b", "main", repo}, {"-C", repo, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "--allow-empty", "-m", "base"}} {
		output, err := exec.Command("git", args...).CombinedOutput()
		require.NoError(t, err, "%s", output)
	}
	require.NoError(t, os.WriteFile(filepath.Join(repo, "retained.txt"), []byte("uncommitted work"), 0600))
	before, err := os.ReadDir(filepath.Join(repo, ".git"))
	require.NoError(t, err)
	page.MustElement("[data-tab=workspaces]").MustClick()
	page.MustElement("#register-workspace").MustClick()
	page.MustElement("#editor [name=path]").MustInput(repo)
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	page.MustElementR("#workspace-list .status", "^available$")
	require.NotContains(t, page.MustElement("#workspace-list").MustText(), "Remove checkout")
	var snapshot struct {
		Workspaces []model.Workspace `json:"workspaces"`
	}
	require.NoError(t, operator.Call(ctx, "GET", "/v2/snapshot", nil, &snapshot))
	require.Len(t, snapshot.Workspaces, 1)
	space := snapshot.Workspaces[0]
	require.Equal(t, model.WorkspaceExternal, space.Intent.Ownership)
	require.Equal(t, model.WorkspaceRegistered, space.Intent.Provenance)
	actual, err := filepath.EvalSymlinks(repo)
	require.NoError(t, err)
	require.Equal(t, actual, space.Observation.ActualPath)
	var repeated app.WorkspaceResult
	require.NoError(t, operator.Call(ctx, "POST", "/v2/workspaces/register", map[string]any{"request_id": space.ID, "id": space.ID, "intent": space.Intent}, &repeated))
	require.Equal(t, space.Revision, repeated.Workspace.Revision)
	require.Error(t, operator.Call(ctx, "POST", "/v2/workspaces/remove", map[string]any{"request_id": "remove", "workspace_id": space.ID, "expected_revision": space.Revision, "destructive": true}, nil))
	data, err := os.ReadFile(filepath.Join(repo, "retained.txt"))
	require.NoError(t, err)
	require.Equal(t, "uncommitted work", string(data))
	after, err := os.ReadDir(filepath.Join(repo, ".git"))
	require.NoError(t, err)
	names := func(entries []os.DirEntry) []string {
		result := []string{}
		for _, e := range entries {
			result = append(result, e.Name())
		}
		return result
	}
	require.Equal(t, names(before), names(after), "registration must not write a Git owner marker")
	require.Error(t, operator.Call(ctx, "POST", "/v2/workspaces/register", map[string]any{"request_id": "claim", "id": "claim", "intent": model.WorkspaceIntent{IntendedPath: repo, Ownership: model.WorkspaceOwned}}, nil))
}
