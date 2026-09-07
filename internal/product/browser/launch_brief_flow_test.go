package browser

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/host"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func TestBrowserStartsWithPreparedInitialBrief(t *testing.T) {
	p := &automationTeamProvider{delivery: host.ActionCredentialHost{PrivateRoot: filepath.Join(t.TempDir(), "credentials")}, briefs: make(chan string, 4)}
	ctx, page, operator := processEditorBrowser(t, p)
	desired := model.DesiredConfiguration{Harness: p.Name(), Model: "fixture", WorkingDirectory: t.TempDir(), Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}
	require.NoError(t, operator.Call(ctx, "POST", "/v2/agents", map[string]any{"id": "writer", "name": "Writer", "desired": desired}, nil))
	page.MustElement("#refresh").MustClick()
	page.MustElementR("#roster button", "^Start with brief$").MustClick()
	const brief = "Inspect the repository.\nDo not modify files until the design is explained."
	page.MustElement("#editor [name=brief]").MustInput(brief)
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustWait(`() => !document.querySelector('#editor').open`)
	select {
	case got := <-p.briefs:
		require.Equal(t, brief, got)
	case <-time.After(time.Second):
		t.Fatal("provider did not receive prepared input")
	}
	page.MustElementR("#roster button", "^Attach$")
	page.MustElement("#refresh").MustClick()
	select {
	case extra := <-p.briefs:
		t.Fatalf("refresh replayed input: %q", extra)
	default:
	}
	var snapshot map[string]any
	require.NoError(t, operator.Call(ctx, "GET", "/v2/snapshot", nil, &snapshot))
	encoded, err := json.Marshal(snapshot)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "Do not modify files")
	require.NotContains(t, page.MustElement("#roster").MustText(), brief)
}
