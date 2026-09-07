package browser

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/host"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func TestBrowserProfileStartupUsesPinnedSuggestionsBeforeExplicitLaunch(t *testing.T) {
	p := &automationTeamProvider{name: "claude", delivery: host.ActionCredentialHost{PrivateRoot: filepath.Join(t.TempDir(), "credentials")}, briefs: make(chan string, 4)}
	ctx, page, operator := processEditorBrowser(t, p)
	page = page.Timeout(40 * time.Second)
	page.MustElement("[data-tab=configurations]").MustClick()
	page.MustElement("#new-configuration").MustClick()
	page.MustElement("#editor [name=name]").MustInput("Startup profile")
	page.MustElement("#editor [name=harness]").MustSelect("claude")
	page.MustElement("#editor [name=model]").MustInput("fixture")
	page.MustElement("#editor [name=cwd]").MustInput(t.TempDir())
	page.MustElement("#editor [name=startup_name]").MustInput("Suggested writer")
	page.MustElement("#editor [name=startup_context]").MustInput("Pinned context")
	page.MustElement("#editor [name=startup_brief]").MustInput("Pinned brief")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustWait(`() => !document.querySelector('#editor').open`)
	page.MustElementR("#configuration-list button", "^Use as default$").MustClick()
	page.MustElementR("#editor-title", "^Set default configuration$")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustWait(`() => !document.querySelector('#editor').open`)
	page.MustElementR("#configuration-list button", "^Create from global$").MustClick()
	page.MustElementR("#editor-title", "^Create agent from default$")
	require.Equal(t, "Suggested writer", page.MustElement("#editor [name=name]").MustProperty("value").Str())
	page.MustElement("#editor button[value=cancel]").MustClick()
	page.MustElementR("#configuration-list button", "^Create agent$").MustClick()
	page.MustElementR("#editor-title", "^Create agent from configuration$")
	require.Equal(t, "Suggested writer", page.MustElement("#editor [name=name]").MustProperty("value").Str())
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustWait(`() => !document.querySelector('#editor').open`)
	var snapshot struct {
		Agents []model.Agent `json:"agents"`
	}
	require.NoError(t, operator.Call(ctx, "GET", "/v2/snapshot", nil, &snapshot))
	require.Len(t, snapshot.Agents, 1)
	pinned := snapshot.Agents[0].ConfigurationProfile
	require.NotNil(t, pinned)
	page.MustElementR("#configuration-list button", "^Edit configuration$").MustClick()
	page.MustElementR("#editor-title", "^Save new configuration revision$")
	require.Equal(t, "Pinned brief", page.MustElement("#editor [name=startup_brief]").MustProperty("value").Str())
	page.MustElement("#editor [name=startup_brief]").MustSelectAllText().MustInput("Future brief")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustWait(`() => !document.querySelector('#editor').open`)
	// Old agents retain the exact startup revision they were created from.
	var saved app.ConfigurationProfileResult
	require.NoError(t, operator.Call(ctx, "GET", "/v2/configuration-profiles/"+string(pinned.ProfileID), nil, &saved))
	require.Equal(t, "Future brief", saved.Revision.Startup.InitialMessage)
	page.MustElement("[data-tab=groups]").MustClick()
	page.MustElementR("#roster button", "^Start with brief$").MustClick()
	page.MustWait(`() => document.querySelector('#editor').open`)
	require.Equal(t, "Pinned context", page.MustElement("#editor [name=context]").MustProperty("value").Str())
	require.Equal(t, "Pinned brief", page.MustElement("#editor [name=brief]").MustProperty("value").Str())
	page.MustElement("#editor button[value=cancel]").MustClick()
	page.MustEval(`() => {window.beforeStartupRefresh=[...document.querySelectorAll('#roster button')].find(b=>b.textContent==='Start with brief')}`)
	page.MustElement("#refresh").MustClick()
	page.MustWait(`() => !window.beforeStartupRefresh.isConnected`)
	select {
	case body := <-p.briefs:
		t.Fatalf("cancel/refresh launched %q", body)
	default:
	}
	page.MustElementR("#roster button", "^Start with brief$").MustClick()
	page.MustWait(`() => document.querySelector('#editor').open`)
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustWait(`() => !document.querySelector('#editor').open`)
	select {
	case body := <-p.briefs:
		require.Equal(t, "Pinned context\n\nPinned brief", body)
	case <-time.After(time.Second):
		t.Fatal("missing prepared input")
	}
	var raw map[string]any
	require.NoError(t, operator.Call(ctx, "GET", "/v2/snapshot", nil, &raw))
	encoded, err := json.Marshal(raw)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "Pinned brief")
	require.NotContains(t, string(encoded), "Pinned context")
}
