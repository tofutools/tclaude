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
		backendDone <- backend.Serve(ctx, state, providers.NewRegistry(), backend.JourneyServices{Workspaces: workspaceHost, Programs: host.ProgramProcessHost{PrivateRoot: filepath.Join(state, "programs")}})
	}()
	require.Eventually(t, func() bool {
		select {
		case err := <-backendDone:
			t.Fatalf("backend exited before readiness: %v", err)
		default:
		}
		_, err := os.Stat(filepath.Join(state, "api.sock"))
		return err == nil
	}, 5*time.Second, 10*time.Millisecond)
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
	// Exercise the actual blocked decision browser control with a real bounded
	// program process and a separate owned checkout.
	page.MustEval(`async (root) => {
  await api('/v2/workspaces/create',{request_id:'blocked_workspace',id:'blocked_workspace',intent:{Repository:root,IntendedPath:root+'/blocked-checkout',BaseRevision:'HEAD',Branch:'blocked-fixture',Provenance:'platform_created',Ownership:'owned',RetainOnFinish:true}});
  const p=await api('/v2/program-profiles',{request_id:'blocked_profile',id:'blocked_profile',revision_id:'blocked_profile_v1',name:'Failing browser check',executable:'/bin/sh',argument_prefix:['-c','printf bounded; exit 1'],working_directory:'.',sandbox:'unconfined',timeout:60000000000,output_limit_bytes:1024,effect_authority:[{Action:'program.execute',Resource:{Kind:'workspace'}}]});
  const ref={ProfileID:p.Profile.ID,RevisionID:p.Revision.ID,ContentHash:p.Revision.ContentHash};
  await api('/v2/processes',{request_id:'blocked_start',id:'browser_blocked',start:{Scope:{WorkspaceID:'blocked_workspace'},AuthorizedProgramProfiles:[ref],Deadline:new Date(Date.now()+60000).toISOString(),InlineGraph:{CompilerVersion:'1',EntryNodeID:'check',Nodes:[{ID:'check',Kind:'task',Performer:{Kind:'program',Program:{Profile:ref}},Waivable:true,Retry:{MaxAttempts:1,Retryable:['program_failed']}},{ID:'done',Kind:'end',End:{Outcome:'verified'}}],Edges:[{From:'check',To:'done'}],Outcome:{RequiredNodes:['check']}}}});
 }`, root)
	require.Eventually(t, func() bool {
		value, err := page.Eval(`async()=>{const windows=await api('/v2/decisions');return windows.some(x=>x.Window.Kind==='blocked'&&x.Window.Attempt.RunID==='browser_blocked');}`)
		return err == nil && value.Value.Bool()
	}, 10*time.Second, 100*time.Millisecond)
	page.MustElement("[data-tab=decisions]").MustClick()
	page.MustElementR("#decision-list .card", "browser_blocked").MustElementR("button", "Answer").MustClick()
	page.MustElement("#editor [name=answer]").MustSelect("waive")
	page.MustElement("#editor [name=reason]").MustInput("Explicit browser waiver of failed required proof")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	outcome := page.MustEval(`async()=>{const r=await api('/v2/work/browser_blocked');return {state:r.run.state,waived:r.run.node_attempts.some(a=>a.State==='waived')};}`)
	require.Equal(t, "failed", outcome.Get("state").Str())
	require.True(t, outcome.Get("waived").Bool())

}
