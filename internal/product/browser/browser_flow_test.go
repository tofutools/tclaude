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
	page.MustElement("[name=body]").MustInput("Durable browser message")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	page.MustElementR("#message-list pre", "Durable browser message")
	require.False(t, page.MustElement("#error").MustVisible())
	// The fragment was removed after exchanging the one-use login credential.
	require.NotContains(t, page.MustInfo().URL, "login=")
}
