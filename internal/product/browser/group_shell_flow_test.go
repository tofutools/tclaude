package browser

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/host"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func TestBrowserGroupShellUsesExplicitWorkspaceAndPinnedEnvironment(t *testing.T) {
	if os.Getenv("TCLAUDE_BROWSER_SMOKE") != "1" {
		t.Skip("set TCLAUDE_BROWSER_SMOKE=1")
	}
	root, err := os.MkdirTemp("/tmp", "group-shell-")
	require.NoError(t, err)
	defer os.RemoveAll(root)
	shells, err := host.NewShellHost(host.ShellConfig{Terminal: host.TerminalHost{PrivateRoot: filepath.Join(root, "terminal")}})
	require.NoError(t, err)
	ctx, page, operator := processEditorBrowserConfigured(t, nil, nil, shells)
	repo := filepath.Join(root, "checkout")
	for _, args := range [][]string{{"init", "-b", "main", repo}, {"-C", repo, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "--allow-empty", "-m", "base"}} {
		out, err := exec.Command("git", args...).CombinedOutput()
		require.NoError(t, err, "%s", out)
	}
	require.NoError(t, operator.Call(ctx, "POST", "/v2/groups", map[string]any{"id": "shell-team", "name": "Shell team"}, nil))
	require.NoError(t, operator.Call(ctx, "PUT", "/v2/groups/shell-team/configuration", map[string]any{"expected_revision": 0, "environment": model.Environment{"GROUP_VALUE": "literal $HOME\nvalue"}}, nil))
	page.MustElement("[data-tab=workspaces]").MustClick()
	page.MustElement("#register-workspace").MustClick()
	page.MustElement("#editor [name=path]").MustInput(repo)
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	page.MustElement("[data-tab=groups]").MustClick()
	page.MustElementR("summary", "^Group settings$").MustClick()
	page.MustElementR("#group-management button", "^Open group shell$").MustClick()
	require.Contains(t, page.MustElement("#editor .environment-effective").MustText(), "GROUP_VALUE")
	page.MustElementR("#editor button", "^Add variable$").MustClick()
	page.MustElement("#editor .launch-environment-row input").MustInput("GROUP_VALUE")
	page.MustElement("#editor .launch-environment-row textarea").MustInput("explicit")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	var snapshot struct {
		Executions []struct {
			ID   model.ExecutionID           `json:"id"`
			Spec model.ResolvedExecutionSpec `json:"spec"`
		} `json:"executions"`
	}
	require.NoError(t, operator.Call(ctx, "GET", "/v2/snapshot", nil, &snapshot))
	require.Len(t, snapshot.Executions, 1)
	execution := snapshot.Executions[0]
	require.Equal(t, model.GroupID("shell-team"), execution.Spec.ShellGroup.GroupID)
	require.Equal(t, model.Environment{"GROUP_VALUE": "explicit"}, execution.Spec.Environment)
	defer func() {
		_ = operator.Call(ctx, "POST", "/v2/stop", map[string]any{"request_id": "cleanup-shell", "execution_id": execution.ID, "force": true}, nil)
	}()
	page.MustReload()
	page.MustElement("main:not([inert])")
	page.MustElementR("summary", "^Group settings$").MustClick()
	page.MustElementR("#group-management button", "^Attach group shell$")
	page.MustElementR("#group-management button", "^Stop group shell$").MustClick()
}
