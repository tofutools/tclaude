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
	"github.com/tofutools/tclaude/internal/backend/providers"
	backend "github.com/tofutools/tclaude/internal/backend/server"
)

// Opt-in browser acceptance uses a disposable SQLite/Unix backend and real
// browser event handlers. It launches no harness or native model turn.
func TestBrowserOfflineAgentGroupAndMessageFlow(t *testing.T) {
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
	go func() { backendDone <- backend.Serve(ctx, state, providers.NewRegistry()) }()
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
	page.MustElement("[data-tab=configurations]").MustClick()
	page.MustElement("#new-configuration").MustClick()
	page.MustElement("[name=name]").MustInput("Saved browser worker")
	page.MustElement("[name=model]").MustInput("fixture")
	page.MustElement("[name=cwd]").MustInput(root)
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	page.MustElementR("#configuration-list button", "Create agent").MustClick()
	page.MustElement("#editor").MustWaitVisible()
	page.MustElement("[name=name]").MustSelectAllText().MustInput("Browser worker")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	page.MustElement("[data-tab=groups]").MustClick()
	page.MustElementR("#roster .name", "Browser worker")
	page.MustElementR("#roster .muted", "Saved configuration")
	page.MustElement("#new-group").MustClick()
	page.MustElement("[name=name]").MustInput("Review team")
	page.MustElement("[name=members]").MustSelect("Browser worker")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	page.MustElementR("#roster h2", "Review team")
	page.MustElement("[data-tab=messages]").MustClick()
	page.MustElement("#compose").MustClick()
	page.MustElement("[name=to]").MustSelect("Browser worker")
	page.MustElement("[name=cc]").MustSelect("Operator")
	page.MustElement("[name=subject]").MustInput("Browser review")
	attachmentPath := filepath.Join(root, "review.txt")
	require.NoError(t, os.WriteFile(attachmentPath, []byte("Browser attachment"), 0600))
	page.MustElement("[name=files]").MustSetFiles(attachmentPath)
	page.MustElement("[name=body]").MustInput("Durable browser message")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	page.MustElementR("#message-list pre", "Durable browser message")
	page.MustElementR("#message-list button", "Download review.txt")
	page.MustElementR("#message-list button", "Mark read").MustClick()
	page.MustElementR("#message-list p", "Operator · read")
	page.MustElementR("#message-list button", "Reply all").MustClick()
	page.MustElement("[name=body]").MustInput("Thread reply")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	page.MustElementR("#message-list pre", "Thread reply")
	page.MustElement("[data-tab=groups]").MustClick()
	page.MustElementR("#roster .row button", "^Retire$").MustClick()
	page.MustElement("[name=reason]").MustInput("Finished browser fixture")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	page.MustElementR("#roster .row button", "^Reactivate$").MustClick()
	page.MustElementR("#roster .row button", "^Retire$")

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

	require.False(t, page.MustElement("#error").MustVisible())
	// The fragment was removed after exchanging the one-use login credential.
	require.NotContains(t, page.MustInfo().URL, "login=")
}
