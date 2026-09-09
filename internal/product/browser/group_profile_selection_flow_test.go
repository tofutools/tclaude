package browser

import (
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/host"
	"github.com/tofutools/tclaude/internal/backend/model"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBrowserGroupSpawnSelectsCurrentProfileAndPreservesExplicitFields(t *testing.T) {
	provider := &groupFallbackProvider{automationTeamProvider{name: "claude", delivery: host.ActionCredentialHost{PrivateRoot: filepath.Join(t.TempDir(), "credentials")}, briefs: make(chan string, 4)}}
	ctx, page, operator := processEditorBrowser(t, provider)
	harness, cwd, groupModel, selectedModel := "claude", t.TempDir(), "group-model", "selected-model"
	save := func(id string, options model.ConfigurationOptions, startup model.ProfileStartup) app.ConfigurationProfileResult {
		var result app.ConfigurationProfileResult
		require.NoError(t, operator.Call(ctx, "POST", "/v2/configuration-profiles", map[string]any{"request_id": strings.ToLower(id), "id": strings.ToLower(id), "revision_id": "one", "name": id, "options": options, "startup": startup}, &result))
		return result
	}
	global := save("Global", model.ConfigurationOptions{Harness: &harness, WorkingDirectory: &cwd, Environment: model.Environment{"GLOBAL": "yes"}}, model.ProfileStartup{Description: "Global description"})
	group := save("Group", model.ConfigurationOptions{Model: &groupModel, Environment: model.Environment{"GROUP": "yes"}}, model.ProfileStartup{Role: "reviewer", AgentName: "Suggested group"})
	selected := save("Selected", model.ConfigurationOptions{Model: &selectedModel}, model.ProfileStartup{AgentName: "Suggested selected", Context: "Selected context"})
	require.NoError(t, operator.Call(ctx, "POST", "/v2/configuration-defaults", map[string]any{"request_id": "default", "global": global.Revision.Ref}, nil))
	require.NoError(t, operator.Call(ctx, "POST", "/v2/groups", map[string]any{"id": "team", "name": "Team"}, nil))
	require.NoError(t, operator.Call(ctx, "PUT", "/v2/groups/team/configuration", map[string]any{"profile": group.Revision.Ref}, nil))
	page.MustElement("#refresh").MustClick()
	page.MustWait(`()=>snapshot.groups?.some(g=>g.ID==='team')`)
	page.MustElementR("summary", "^Group settings$").MustClick()
	page.MustElementR("#group-management button", "^Spawn member$").MustClick()
	page.MustElement("#editor [name=name]").MustSelectAllText().MustInput("Operator name")
	page.MustElement("#editor [name=profile]").MustSelect("Selected")
	page.MustElementR("#editor-fields p", "Selected.*selected-model")
	require.Equal(t, "Operator name", page.MustElement("#editor [name=name]").MustProperty("value").Str())
	require.Equal(t, "reviewer", page.MustElement("#editor [name=role_label]").MustProperty("value").Str())
	require.Equal(t, "Global description", page.MustElement("#editor [name=description]").MustProperty("value").Str())
	require.Contains(t, page.MustElement("#editor .environment-effective").MustText(), "GLOBAL")
	require.Contains(t, page.MustElement("#editor .environment-effective").MustText(), "GROUP")
	page.MustElement("#editor [name=role_label]").MustSelectAllText().MustInput("")
	page.MustElement("#editor [name=brief]").MustInput("Task")
	updated := "updated-model"
	require.NoError(t, operator.Call(ctx, "POST", "/v2/configuration-profiles", map[string]any{"request_id": "edit", "id": "selected", "revision_id": "two", "expected_revision": selected.Profile.Revision, "name": "Selected renamed", "options": model.ConfigurationOptions{Model: &updated}}, nil))
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	page.MustWait(`()=>!submitting&&snapshot.agents?.length===1`)
	require.True(t, page.MustEval(`()=>snapshot.agents[0].Name==='Operator name'&&snapshot.agents[0].Desired.Model==='updated-model'&&snapshot.agents[0].ConfigurationProfile.ProfileID==='selected'&&snapshot.agents[0].ConfigurationProfile.RevisionID==='two'&&snapshot.agents[0].Labels.Groups.team.Role===''`).Bool())
	select {
	case body := <-provider.briefs:
		require.Equal(t, "Selected context\n\nTask", body)
	case <-time.After(time.Second):
		t.Fatal("missing launch input")
	}
}
