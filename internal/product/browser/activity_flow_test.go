package browser

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/host"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
)

func TestBrowserActivityFiltersPagesAndExportsAttribution(t *testing.T) {
	p := &accessBrowserProvider{delivery: host.ActionCredentialHost{PrivateRoot: filepath.Join(t.TempDir(), "credentials")}, delivered: make(chan ports.ActionCredentialReceipt, 2)}
	ctx, page, operator := processEditorBrowserWithSetup(t, func(state string) {
		store, err := sqlite.Open(filepath.Join(state, "backend.sqlite"))
		require.NoError(t, err)
		defer store.Close()
		for i := 0; i < 30; i++ {
			id := fmt.Sprintf("audit_%02d", i)
			_, _, err = store.ImportHistoricalActivity(context.Background(), app.HistoricalActivityWrite{SourceKey: id, SourceRevision: "snapshot", Record: model.ActivityRecord{ID: id, Kind: model.ActivityHistorical, Actor: model.ActivityActor{Kind: model.OperatorPrincipal().Kind}, AgentID: "activity_agent", Outcome: "recorded", Reason: "literal <script> historical note " + id, StartedAt: time.Date(2026, 1, 2, 12, i, 0, 0, time.UTC), Historical: true, Provenance: "synthetic import snapshot"}})
			require.NoError(t, err)
		}
	}, p)
	desired := model.DesiredConfiguration{Harness: p.Name(), Model: "fixture", WorkingDirectory: "/tmp", Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}
	require.NoError(t, operator.Call(ctx, "POST", "/v2/agents", map[string]any{"id": "activity_agent", "name": "Activity agent", "desired": desired}, nil))
	require.NoError(t, operator.Call(ctx, "POST", "/v2/launch", map[string]any{"request_id": "activity_launch", "target": map[string]any{"agent": map[string]any{"agent_id": "activity_agent", "expected_revision": 1}}}, nil))
	page.MustElement("#refresh").MustClick()
	page.MustElement("[data-tab=activity]").MustClick()
	page.MustElement("[aria-label='Activity target']").MustSelect("Agent: Activity agent · activity_agent")
	page.MustElementR("#activity-list [role=status]", "25 loaded activity records")
	page.MustElementR("#activity-list button", "^More activity$").MustClick()
	page.MustElementR("#activity-list [role=status]", "31 loaded activity records")
	page.MustElement("[aria-label='Search loaded activity']").MustInput("audit_00")
	page.MustElementR("#activity-list [role=status]", "1 matching of 31")
	require.Len(t, page.MustElements("#activity-list [data-activity]"), 1)
	require.Empty(t, page.MustElements("#activity-list script"))
	page.MustElementR("#activity-list p", "synthetic import snapshot")
	page.MustElement("[aria-label='Search loaded activity']").MustSelectAllText().MustInput("")
	page.MustElement("[aria-label='Activity kind']").MustSelect("Operations")
	page.MustElementR("#activity-list button", "^Apply activity filters$").MustClick()
	page.MustElementR("#activity-list [role=status]", "1 matching of 1")
	page.MustElementR("#activity-list strong", "operation")
	page.MustElement("[aria-label='Activity kind']").MustSelect("Historical audit")
	page.MustElementR("#activity-list button", "^Apply activity filters$").MustClick()
	page.MustElementR("#activity-list [role=status]", "25 loaded activity records")
	page.MustEval(`() => {window.activityExport='';const original=URL.createObjectURL;URL.createObjectURL=b=>{b.text().then(t=>window.activityExport=t);return original(b)};document.addEventListener('click',e=>{if(e.target.download)e.preventDefault()})}`)
	page.MustElementR("#activity-list button", "^Export loaded activity JSON$").MustClick()
	page.MustWait(`() => window.activityExport.includes('audit_')`)
	require.True(t, page.MustEval(`() => JSON.parse(window.activityExport).more_available`).Bool())
	require.Equal(t, 25, page.MustEval(`() => JSON.parse(window.activityExport).records.length`).Int())
	page.MustEval(`() => {document.querySelector('[aria-label="Activity from"]').value='2027-01-01T00:00';document.querySelector('[aria-label="Activity before"]').value='2027-02-01T00:00'}`)
	page.MustElementR("#activity-list button", "^Apply activity filters$").MustClick()
	page.MustElementR("#activity-list p", "No recorded activity")
	page.MustEval(`() => document.querySelector('[aria-label="Activity before"]').value='2026-01-01T00:00'`)
	page.MustElementR("#activity-list button", "^Apply activity filters$").MustClick()
	page.MustElementR("#activity-list [role=status]", "must be later")
	page.MustElement("#logout").MustClick()
	page.MustElementR("#connection", "Signed out")
	require.Empty(t, page.MustElements("#activity-list [data-activity]"))
}
