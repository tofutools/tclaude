package browser

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/client"
	"github.com/tofutools/tclaude/internal/backend/host"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
	"github.com/tofutools/tclaude/internal/backend/providers"
	backend "github.com/tofutools/tclaude/internal/backend/server"
)

func processEditorBrowser(t *testing.T, cohort ...ports.Provider) (context.Context, *rod.Page, *client.Client) {
	t.Helper()
	return processEditorBrowserWithHistory(t, nil, cohort...)
}

func processEditorBrowserWithHistory(t *testing.T, history ports.HistorySourceRegistry, cohort ...ports.Provider) (context.Context, *rod.Page, *client.Client) {
	t.Helper()
	if os.Getenv("TCLAUDE_BROWSER_SMOKE") != "1" {
		t.Skip("set TCLAUDE_BROWSER_SMOKE=1 for installed-Chrome product acceptance")
	}
	chrome, err := exec.LookPath("google-chrome")
	if err != nil {
		chrome, err = exec.LookPath("chromium")
	}
	require.NoError(t, err)
	root, err := os.MkdirTemp("/tmp", "editor-ui-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	state := filepath.Join(root, "state")
	require.NoError(t, backend.Initialize(state))
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	backendDone := make(chan error, 1)
	checkout, err := host.NewCheckoutHost("")
	require.NoError(t, err)
	go func() {
		backendDone <- backend.Serve(ctx, state, providers.NewRegistry(cohort...), backend.JourneyServices{Workspaces: checkout, History: history})
	}()
	require.Eventually(t, func() bool { _, err := os.Stat(filepath.Join(state, "api.sock")); return err == nil }, 5*time.Second, 10*time.Millisecond)
	operator, err := client.New(filepath.Join(state, "api.sock"), filepath.Join(state, "operator.token"))
	require.NoError(t, err)
	t.Cleanup(func() { operator.Close() })
	view, err := Open(state, "127.0.0.1:0")
	require.NoError(t, err)
	viewDone := make(chan error, 1)
	go func() { viewDone <- view.Serve(ctx) }()
	t.Cleanup(func() { cancel(); require.NoError(t, <-viewDone); require.NoError(t, <-backendDone) })
	profile := filepath.Join(root, "chrome")
	l := launcher.New().Context(ctx).Bin(chrome).UserDataDir(filepath.Join(profile, "profile")).Headless(true).NoSandbox(true).Leakless(false).Set("disable-gpu")
	env := []string{}
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "XDG_CONFIG_HOME=") && !strings.HasPrefix(entry, "XDG_CACHE_HOME=") && !strings.HasPrefix(entry, "XDG_DATA_HOME=") {
			env = append(env, entry)
		}
	}
	l.Env(append(env, "XDG_CONFIG_HOME="+filepath.Join(profile, "config"), "XDG_CACHE_HOME="+filepath.Join(profile, "cache"), "XDG_DATA_HOME="+filepath.Join(profile, "data"))...)
	t.Cleanup(l.Kill)
	control, err := l.Launch()
	require.NoError(t, err)
	browser := rod.New().Context(ctx).ControlURL(control)
	require.NoError(t, browser.Connect())
	t.Cleanup(func() { _ = browser.Close() })
	page := browser.MustPage(view.URL())
	page.MustElementR("#connection", "^Updated ")
	return ctx, page, operator
}

func TestBrowserProcessEditorAuthorsSavesReopensAndPreservesConflicts(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t)
	page.MustElement("[data-tab=processes]").MustClick()
	page.MustElementR("#definition-list button", "^New process$").MustClick()
	page.MustElement("#process-editor-canvas svg")
	page.MustElement("[aria-label='Process name']").MustSelectAllText().MustInput("Visual wait process")
	page.MustElementR("#process-editor button", "^Add wait$").MustClick()
	page.MustElement("#process-inspector [name=name]").MustSelectAllText().MustInput("Pause")
	page.MustElement("#process-inspector [name=duration]").MustSelectAllText().MustInput("1")
	page.MustElementR("#process-inspector button", "^Apply changes$").MustClick()
	page.MustElementR("#process-inspector button", "^Make entry$").MustClick()
	page.MustElementR("#process-inspector button", "^Connect$").MustClick()
	page.MustElementR("#process-editor button", "^Validate$").MustClick()
	page.MustElementR("#process-editor-message", "Validation passed")
	// Actual pointer drag exercises the reused SVG widget and the v2 layout adapter.
	node := page.MustElement("#process-editor-canvas .process-node[aria-label='Pause, wait']")
	box := node.MustShape().Box()
	start := proto.Point{X: box.X + box.Width/2, Y: box.Y + box.Height/2}
	require.NoError(t, page.Mouse.MoveTo(start))
	require.NoError(t, page.Mouse.Down(proto.InputMouseButtonLeft, 1))
	require.NoError(t, page.Mouse.MoveLinear(proto.Point{X: start.X + 90, Y: start.Y + 55}, 8))
	require.NoError(t, page.Mouse.Up(proto.InputMouseButtonLeft, 1))
	page.MustElementR("#process-editor button", "^Save revision$").MustClick()
	page.MustElementR("#process-editor-message", "Revision 1 · saved")
	var definitions []model.Definition
	require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions", nil, &definitions))
	require.Len(t, definitions, 1)
	var first app.DefinitionResult
	require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions/"+string(definitions[0].ID), nil, &first))
	require.Len(t, first.Revision.Process.Graph.Nodes, 2)
	require.Len(t, first.Revision.Process.Graph.Edges, 1)
	positions := first.Revision.EditorLayout.Nodes
	require.Len(t, positions, 2)
	pause := first.Revision.Process.Graph.EntryNodeID
	require.NotEqual(t, positions[pause], positions[first.Revision.Process.Graph.Edges[0].To])
	page.MustElementR("#process-editor button", "^Close editor$").MustClick()
	page.MustElementR("#definition-list button", "^Edit process$").MustClick()
	page.MustElementR("#process-editor-message", "Revision 1 · saved")
	page.MustElement("#process-editor-canvas .process-node[aria-label='Pause, wait']").MustClick()
	require.Equal(t, "1", page.MustElement("#process-inspector [name=duration]").MustProperty("value").Str())
	page.MustElement("[aria-label='Process name']").MustSelectAllText().MustInput("Focused draft name")
	require.True(t, page.MustEval(`() => { const event = new Event('beforeunload', {cancelable:true}); window.dispatchEvent(event); return event.defaultPrevented; }`).Bool())
	// A second operator writes while this browser keeps an edited stale draft.
	draft := app.DefinitionDraft{ID: first.Definition.ID, RevisionID: "external-revision", Name: "Remote name", Kind: model.DefinitionProcess, SchemaVersion: 1, Source: first.Revision.Source, Process: first.Revision.Process, EditorLayout: first.Revision.EditorLayout}
	require.NoError(t, operator.Call(ctx, "POST", "/v2/definitions", map[string]any{"request_id": "external-save", "expected_revision": 1, "draft": draft}, nil))
	page.MustElement("[aria-label='Process name']").MustSelectAllText().MustInput("Local unsaved name")
	page.MustElementR("#process-editor button", "^Save revision$").MustClick()
	page.MustElementR("#process-editor-errors", "saved state changed")
	require.Equal(t, "Local unsaved name", page.MustElement("[aria-label='Process name']").MustProperty("value").Str())
	page.MustElementR("#process-editor button", "^Export$") // Export remains available on conflict.
	var latest app.DefinitionResult
	require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions/"+string(first.Definition.ID), nil, &latest))
	require.Equal(t, "Remote name", latest.Definition.Name)
	require.Equal(t, positions, latest.Revision.EditorLayout.Nodes)
	wait, handle := page.MustHandleDialog()
	done := make(chan struct{})
	go func() { defer close(done); wait(); handle(true, "") }()
	page.MustElementR("#process-editor-errors button", "Load saved revision").MustClick()
	<-done
	page.MustElementR("#process-editor-message", "Revision 2 · saved")
	require.Equal(t, "Remote name", page.MustElement("[aria-label='Process name']").MustProperty("value").Str())
	if output := os.Getenv("TCLAUDE_PARITY_SCREENSHOT"); output != "" {
		page.MustScreenshot(output)
	}
	page.MustElementR("#process-editor button", "^Close editor$").MustClick()
	require.False(t, page.MustElement("#error").MustVisible())
}

func TestBrowserProcessEditorWorkerDecisionAndDraftHistory(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t)
	page.MustElement("[data-tab=processes]").MustClick()
	page.MustElementR("#definition-list button", "^New process$").MustClick()
	page.MustElementR("#process-editor button", "^Add task$").MustClick()
	page.MustElement("#process-inspector [name=name]").MustSelectAllText().MustInput("Implement")
	page.MustElement("#process-inspector [name=brief]").MustInput("Implement the requested change and report evidence.")
	page.MustElementR("#process-inspector button", "^Apply changes$").MustClick()
	page.MustElementR("#process-inspector button", "^Make entry$").MustClick()
	page.MustElementR("#process-editor button", "^Copy nodes$").MustClick()
	page.MustElementR("#process-editor button", "^Paste nodes$").MustClick()
	page.MustElement("#process-editor-canvas [aria-label='Implement copy, task']")
	page.MustElementR("#process-editor button", "^Undo$").MustClick()
	require.False(t, page.MustHas("#process-editor-canvas [aria-label='Implement copy, task']"))
	page.MustElementR("#process-editor button", "^Redo$").MustClick()
	page.MustElement("#process-editor-canvas [aria-label='Implement copy, task']").MustClick()
	page.MustElementR("#process-editor button", "^Delete selected$").MustClick()
	page.MustElementR("#process-editor button", "^Add decision$").MustClick()
	page.MustElement("#process-inspector [name=name]").MustSelectAllText().MustInput("Review")
	page.MustElementR("#process-inspector button", "^Apply changes$").MustClick()
	page.MustElement("#process-inspector [aria-label='Connection answer or outcome']").MustInput("approve")
	page.MustElementR("#process-inspector button", "^Connect$").MustClick()
	page.MustElement("#process-editor-canvas [aria-label='Implement, task']").MustClick()
	page.MustElement("#process-inspector [aria-label='Connect to']").MustSelect("Review")
	page.MustElementR("#process-inspector button", "^Connect$").MustClick()
	page.MustElementR("#process-editor button", "^Parameters$").MustClick()
	page.MustElementR("#process-inspector button", "^Add parameter$").MustClick()
	page.MustElement("#process-inspector [name=name]").MustInput("attempts")
	page.MustElement("#process-inspector [name=type]").MustSelect("number")
	page.MustElement("#process-inspector [name=default]").MustInput("3")
	page.MustElementR("#process-inspector button", "^Apply changes$").MustClick()
	page.MustElementR("#process-editor button", "^Save revision$").MustClick()
	page.MustElementR("#process-editor-message", "Revision 1 · saved")
	var definitions []model.Definition
	require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions", nil, &definitions))
	require.Len(t, definitions, 1)
	var saved app.DefinitionResult
	require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions/"+string(definitions[0].ID), nil, &saved))
	require.Len(t, saved.Revision.Parameters, 1)
	require.JSONEq(t, "3", string(saved.Revision.Parameters[0].Default))
	require.Len(t, saved.Revision.Process.Graph.Nodes, 3)
	require.Len(t, saved.Revision.Process.Graph.Edges, 2)
	var worker model.WorkNode
	for _, node := range saved.Revision.Process.Graph.Nodes {
		if node.Name == "Implement" {
			worker = node
		}
	}
	require.NotNil(t, worker.Performer)
	require.Equal(t, "worker", worker.Performer.Agent.MemberKey)
	require.Equal(t, "Implement the requested change and report evidence.", worker.Performer.Agent.Brief)
	page.MustElementR("#process-editor button", "^Close editor$").MustClick()
	page.MustElementR("#definition-list button", "^Edit process$").MustClick()
	page.MustElementR("#process-editor button", "^Parameters$").MustClick()
	page.MustElementR("#process-inspector button", "^Edit attempts$").MustClick()
	require.Equal(t, "3", page.MustElement("#process-inspector [name=default]").MustProperty("value").Str())
	page.MustElement("#process-editor-canvas [aria-label='Review, decision']").MustClick()
	page.MustElement("#process-inspector [name=answers]").MustSelectAllText().MustInput("yes\nno")
	page.MustElementR("#process-inspector button", "^Apply changes$").MustClick()
	page.MustElementR("#process-editor button", "^Validate$").MustClick()
	page.MustElementR("#process-editor-errors", "no longer permitted")
	var unchanged app.DefinitionResult
	require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions/"+string(definitions[0].ID), nil, &unchanged))
	require.Equal(t, model.Revision(1), unchanged.Definition.Revision)
	wait, handle := page.MustHandleDialog()
	done := make(chan struct{})
	go func() { defer close(done); wait(); handle(true, "") }()
	page.MustElementR("#process-editor button", "^Close editor$").MustClick()
	<-done
}
