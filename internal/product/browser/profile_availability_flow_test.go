package browser

import (
	"encoding/json"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"testing"
)

func TestBrowserConfigurationAvailabilityRetainsReasonAndDefault(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t)
	var profile app.ConfigurationProfileResult
	require.NoError(t, operator.Call(ctx, "POST", "/v2/configuration-profiles", map[string]any{"request_id": "save", "id": "profile", "revision_id": "one", "name": "Saved worker", "desired": model.DesiredConfiguration{Harness: "claude", Model: "fixture", WorkingDirectory: "/tmp", Approval: model.ApprovalSupervised, Sandbox: model.SandboxUnconfined}}, &profile))
	require.NoError(t, operator.Call(ctx, "POST", "/v2/configuration-defaults", map[string]any{"request_id": "default", "global": profile.Revision.Ref}, nil))
	for _, body := range []map[string]any{{"request_id": "missing", "expected_revision": 1}, {"request_id": "null", "expected_revision": 1, "disabled": nil}} {
		require.Error(t, operator.Call(ctx, "POST", "/v2/configuration-profiles/profile/availability", body, nil))
	}
	page.MustElement("[data-tab=configurations]").MustClick()
	page.MustElementR("#configuration-list button", "^Disable configuration$").MustClick()
	page.MustElement("#editor [name=reason]").MustInput("Provider maintenance")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustWait(`() => !document.querySelector('#editor').open && !submitting`)
	page.MustElementR("#configuration-list p", "Disabled for new agents")

	page.MustElementR("#configuration-list button", "^Export configurations$").MustClick()
	page.MustElementR("dialog[aria-label='Export configurations']", "Saved worker · Disabled · Provider maintenance")
	page.MustElementR("dialog[aria-label='Export configurations'] button", "^Cancel$").MustClick()
	bundle, _ := json.Marshal(app.ConfigurationBundle{Format: app.ConfigurationBundleFormat, Version: 1, Profiles: []app.ConfigurationBundleEntry{{Key: "source", Name: "Previewed worker", Desired: profile.Revision.Desired, Disabled: true, DisabledReason: "Provider maintenance"}}})
	page.MustElementR("#configuration-list button", "^Import configurations$").MustClick()
	page.MustElement("[aria-label='Configuration bundle']").MustInput(string(bundle))
	page.MustElementR("dialog[aria-label='Import configurations'] button", "^Preview$").MustClick()
	page.MustElementR("dialog[aria-label='Import configurations']", "Previewed worker · Disabled · Provider maintenance")
	page.MustElementR("dialog[aria-label='Import configurations']", "also replaces its enabled/disabled state")
	// The draft is deliberately discarded, never published.
	page.MustEval(`() => window.confirm=()=>true`)
	page.MustElementR("dialog[aria-label='Import configurations'] button", "^Cancel$").MustClick()
	page.MustElementR("#configuration-list button", "^Create from global$").MustClick()
	page.MustElement("#editor [name=name]").MustInput("Blocked worker")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElementR("#editor", "Provider maintenance")
	page.MustElementR("#editor button", "^Cancel$").MustClick()
	page.MustElementR("#configuration-list button", "^Enable configuration$").MustClick()
	require.Equal(t, "Provider maintenance", page.MustElement("#editor [name=reason]").MustProperty("value").String())
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustWait(`() => !document.querySelector('#editor').open && !submitting`)
	page.MustReload()
	page.MustElementR("#connection", "^Updated")
	page.MustElement("[data-tab=configurations]").MustClick()
	page.MustElementR("#configuration-list p", "Enabled for new agents")
	page.MustElementR("#configuration-list p", "Provider maintenance")
	var reopened app.ConfigurationProfileResult
	require.NoError(t, operator.Call(ctx, "GET", "/v2/configuration-profiles/profile", nil, &reopened))
	require.False(t, reopened.Profile.Disabled)
	require.Equal(t, "Provider maintenance", reopened.Profile.DisabledReason)
}
