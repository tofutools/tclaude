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
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/host"
	"github.com/tofutools/tclaude/internal/backend/providers"
	backend "github.com/tofutools/tclaude/internal/backend/server"
)

// Opt-in browser acceptance uses a disposable SQLite/Unix backend and real
// browser event handlers. It launches no harness or native model turn.
func TestBrowserPinnedProcessBindsNamedAgent(t *testing.T) {
	if os.Getenv("TCLAUDE_BROWSER_SMOKE") != "1" {
		t.Skip("set TCLAUDE_BROWSER_SMOKE=1 for installed-Chrome product acceptance")
	}
	chrome, err := exec.LookPath("google-chrome")
	if err != nil {
		chrome, err = exec.LookPath("chromium")
	}
	require.NoError(t, err)
	root, err := os.MkdirTemp("", "product-browser-")
	require.NoError(t, err)
	defer os.RemoveAll(root)
	state := filepath.Join(root, "state")
	require.NoError(t, backend.Initialize(state))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	backendDone := make(chan error, 1)
	workspaceHost, err := host.NewCheckoutHost("")
	require.NoError(t, err)
	go func() {
		backendDone <- backend.Serve(ctx, state, providers.NewRegistry(), backend.JourneyServices{Workspaces: workspaceHost})
	}()
	require.Eventually(t, func() bool { _, err := os.Stat(filepath.Join(state, "api.sock")); return err == nil }, 5*time.Second, 10*time.Millisecond)
	view, err := Open(state, "127.0.0.1:0")
	require.NoError(t, err)
	viewDone := make(chan error, 1)
	go func() { viewDone <- view.Serve(ctx) }()
	defer func() { cancel(); require.NoError(t, <-viewDone); require.NoError(t, <-backendDone) }()
	profile := filepath.Join(root, "chrome")
	l := launcher.New().Context(ctx).Bin(chrome).UserDataDir(filepath.Join(profile, "profile")).Headless(true).NoSandbox(true).Leakless(false).Set("disable-gpu")
	env := []string{}
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "XDG_CONFIG_HOME=") && !strings.HasPrefix(entry, "XDG_CACHE_HOME=") && !strings.HasPrefix(entry, "XDG_DATA_HOME=") {
			env = append(env, entry)
		}
	}
	l.Env(append(env, "XDG_CONFIG_HOME="+filepath.Join(profile, "config"), "XDG_CACHE_HOME="+filepath.Join(profile, "cache"), "XDG_DATA_HOME="+filepath.Join(profile, "data"))...)
	defer l.Kill()
	control, err := l.Launch()
	require.NoError(t, err)
	browser := rod.New().Context(ctx).ControlURL(control)
	require.NoError(t, browser.Connect())
	defer browser.Close()
	page := browser.MustPage(view.URL())
	require.Eventually(t, func() bool {
		text, err := page.Eval(`() => document.getElementById('connection')?.textContent || ''`)
		if err != nil {
			return false
		}
		return strings.HasPrefix(text.Value.Str(), "Updated")
	}, 5*time.Second, 100*time.Millisecond, "browser did not connect")
	page.MustElement("#new-agent").MustClick()
	page.MustElement("[name=name]").MustInput("Browser worker")
	page.MustElement("[name=model]").MustInput("fixture")
	page.MustElement("[name=cwd]").MustInput(root)
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	page.MustElementR("#roster .name", "Browser worker")
	page.MustElement("#new-group").MustClick()
	page.MustElement("[name=name]").MustInput("Review team")
	page.MustElement("[name=members]").MustSelect("Browser worker")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	page.MustElementR("#roster h2", "Review team")
	page.MustElement("[data-tab=messages]").MustClick()
	page.MustElement("#compose").MustClick()
	page.MustElement("[name=to]").MustSelect("Browser worker")
	page.MustElement("[name=subject]").MustInput("Process binding review")
	page.MustElement("[name=body]").MustInput("Durable browser message")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	page.MustElementR("#message-list pre", "Durable browser message")
	// Seed an authored human-decision process through the same authenticated API,
	// then answer it through browser controls (no simulated decision handler).
	page.MustEval(`async () => {
	 await api('/v2/processes', {request_id:'browser_process_start',id:'browser_process',start:{Deadline:new Date(Date.now()+60000).toISOString(),InlineGraph:{CompilerVersion:'1',EntryNodeID:'approve',Nodes:[{ID:'approve',Name:'Approve browser outcome',Kind:'decision',Decision:{Kind:'work',Audience:[{Subject:{Kind:'operator'}}],PermittedAnswers:['approve'],ExpiresAfter:60000000000}},{ID:'done',Kind:'end',End:{Outcome:'verified'}}],Edges:[{From:'approve',To:'done'}]}}});
	}`)
	page.MustElement("[data-tab=decisions]").MustClick()
	page.MustElementR("#decision-list button", "Answer").MustClick()
	page.MustElement("[name=reason]").MustInput("Browser evidence inspected")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	page.MustElement("[data-tab=work]").MustClick()
	page.MustElementR("#work-list .card", "browser_process")
	require.Eventually(t, func() bool {
		value, err := page.Eval(`async () => {await refresh();return document.getElementById('work-list').textContent;}`)
		return err == nil && strings.Contains(value.Value.Str(), "succeeded")
	}, 5*time.Second, 100*time.Millisecond)

	require.NoError(t, exec.Command("git", "init", root).Run())
	require.NoError(t, exec.Command("git", "-C", root, "-c", "user.name=Reviewer", "-c", "user.email=review@example.invalid", "commit", "--allow-empty", "-m", "fixture").Run())
	page.MustEval(`async () => {
  await api('/v2/definitions', {request_id:'binding_definition',draft:{ID:'binding_process',Name:'Binding process',Kind:'process',SchemaVersion:1,Source:'binding fixture',Process:{Graph:{CompilerVersion:'1',EntryNodeID:'task',Nodes:[{ID:'task',Kind:'task',Performer:{Kind:'agent',Agent:{MemberKey:'implementer',Brief:'work'}}},{ID:'done',Kind:'end',End:{Outcome:'verified'}}],Edges:[{From:'task',To:'done'}]}}}});
  await api('/v2/workspaces/create',{request_id:'binding_workspace',id:'binding_workspace',intent:{Repository:"` + root + `",IntendedPath:"` + root + `/checkout",BaseRevision:"HEAD",Branch:"review-fixture",Provenance:'platform_created',Ownership:'owned',RetainOnFinish:true}});
  await refresh();
 }`)
	page.MustElement("[data-tab=processes]").MustClick()
	page.MustElementR("#definition-list button", "Start process").MustClick()
	page.MustElement("#editor [name=workspace]")
	page.MustElement("#editor [name=binding_implementer]").MustSelect("Browser worker")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	result := page.MustEval(`async () => {const data=await api('/v2/snapshot');const run=(data.work_runs||[]).find(r=>r.run.definition_closure?.some(d=>d.DefinitionID==='binding_process'));const worker=data.agents.find(a=>a.Name==='Browser worker');const performer=run?.run.graph?.Nodes.find(n=>n.ID==='task')?.Performer;return !!worker&&performer?.Agent?.AgentID===worker.ID&&!performer?.Agent?.MemberKey;}`)
	require.True(t, result.Bool(), "public run must pin the selected agent into its graph")
}
