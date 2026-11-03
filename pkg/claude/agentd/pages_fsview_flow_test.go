package agentd_test

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// Static live pages go back to their agent only when the stage serves them
// exactly as the agent sees its files (tcl-kxiqqd): through a Linux
// tclaude-layer agent's own mount namespace, or for an unconfined agent.
// Anything else is served to the operator but never read back.

// sandboxAgent records a launch for a's session: the implementation, the
// effective profile it ran under when eff is non-nil, and any further fields.
func (e *pageEnv) sandboxAgent(t *testing.T, a pageAgent, implementation string, eff *sandboxpolicy.EffectiveProfile, more ...func(*db.SessionRow)) {
	t.Helper()
	row, err := db.FindSessionByConvID(a.conv)
	require.NoError(t, err)
	require.NotNil(t, row)
	row.SandboxImplementation = implementation
	row.EffectiveSandbox = nil
	if eff != nil {
		snap := sandboxpolicy.NewSnapshot(*eff, nil)
		row.EffectiveSandbox = &snap
	}
	for _, f := range more {
		f(row)
	}
	require.NoError(t, db.SaveSession(row))
}

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		p := filepath.Join(root, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte(body), 0o644))
	}
}

// staticSite writes a site under the agent's startup directory with a public
// part, a part the profile denies and a part a tmpfs covers.
func staticSite(t *testing.T, cwd string) string {
	t.Helper()
	site := filepath.Join(cwd, "site")
	writeTree(t, site, map[string]string{
		"index.html":        "<html><head></head><body>home</body></html>",
		"public/ok.html":    "<html><head></head><body>public</body></html>",
		"secret/key.html":   "<html><head></head><body>SECRET</body></html>",
		"secret/notes.txt":  "SECRET NOTES",
		"scratch/host.html": "<html><head></head><body>HOST UNDER TMPFS</body></html>",
	})
	return site
}

func siteProfile(cwd string) *sandboxpolicy.EffectiveProfile {
	site := filepath.Join(cwd, "site")
	return &sandboxpolicy.EffectiveProfile{
		Filesystem: []sandboxpolicy.FilesystemGrant{
			{Path: cwd, Access: sandboxpolicy.AccessWrite},
			{Path: filepath.Join(site, "secret"), Access: sandboxpolicy.AccessDeny},
		},
		Tmpfs: []sandboxpolicy.TmpfsMount{{Path: filepath.Join(site, "scratch")}},
	}
}

// claudeSandbox records Claude Code's own OS sandbox verdict.
func claudeSandbox(state string, unverified bool) func(*db.SessionRow) {
	return func(row *db.SessionRow) {
		row.Harness = "claude"
		row.OSSandboxState = state
		row.OSSandboxUnverified = unverified
	}
}

func (e *pageEnv) stubSnapshot(t *testing.T) {
	t.Cleanup(agentd.SetPageSnapshotRenderForTest(func(agentd.PageSnapshotJobForTest) ([]byte, error) {
		return []byte("png"), nil
	}))
}

// assertReadBack checks snapshot, dom and the text under marks.
func (e *pageEnv) assertReadBack(t *testing.T, a pageAgent, p pageOut, name string, want bool) {
	t.Helper()
	rec := e.call(t, a, http.MethodPost, "/v1/pages/"+name+"/snapshot", nil)
	if want {
		assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	} else {
		assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "cannot be read back")
	}
	e.assertLiveReadBack(t, a, p, name, want)
}

// assertLiveReadBack checks what comes from the operator's browser: dom and
// the page's own data on a sent batch.
func (e *pageEnv) assertLiveReadBack(t *testing.T, a pageAgent, p pageOut, name string, want bool) {
	t.Helper()
	rec := e.call(t, a, http.MethodPost, "/v1/pages/"+name+"/dom", map[string]any{"selector": "body"})
	assert.Equal(t, http.StatusConflict, rec.Code)
	if want {
		assert.Contains(t, rec.Body.String(), "no dashboard view", "refused for want of a view, not for being static")
	} else {
		assert.Contains(t, rec.Body.String(), "cannot be read back")
	}
	rec = e.call(t, a, http.MethodPost, "/v1/pages/"+name+"/eval", map[string]any{"js": "document.title"})
	assert.Equal(t, http.StatusConflict, rec.Code)
	if want {
		assert.Contains(t, rec.Body.String(), "not open", "refused for want of a view, not for being static")
	} else {
		assert.Contains(t, rec.Body.String(), "cannot be read back", "an eval's value is the page's content")
	}
	rec = e.dashJSON(t, http.MethodPost, "/api/pages/"+p.ID+"/marks", sampleMarks())
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got := e.marks(t, a, name, "")
	require.NotEmpty(t, got.Batches)
	b := got.Batches[len(got.Batches)-1]
	if want {
		assert.NotEmpty(t, b.Marks[0].Anchors, "the text under marks arrives")
		assert.Equal(t, "/checkout?step=2", b.Path)
	} else {
		for _, m := range b.Marks {
			assert.Empty(t, m.Anchors)
		}
		assert.Empty(t, b.Path, "the path the page's script reports is dropped too")
		assert.Equal(t, "Make the pay button blue", b.Note, "the operator's own note still arrives")
		assert.Len(t, b.Marks, 2, "and so do the marks")
		assert.Equal(t, [2]int{40, 180}, b.Marks[0].Points[0], "points are where the operator drew them, not shifted by the page's scroll")
		assert.Zero(t, b.Viewport.W, "the viewport the page's script reports is dropped")
	}
}

func TestPageStaticOfATclaudeLayerAgentIsServedFromItsOwnSandbox(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("only Linux serves through the agent's mount namespace")
	}
	e := newPageEnv(t)
	a := e.agent(t, "fsns", true)
	site := staticSite(t, a.cwd)
	// The sandbox shows the site differently from the host: its secret is
	// gone (a tmpfs, wherever it was renamed to) and its home page is the
	// agent's own.
	fake := &agentd.FakeSandboxRootForTest{Dir: t.TempDir()}
	t.Cleanup(agentd.SetPageSandboxRootForTest(fake))
	writeTree(t, filepath.Join(fake.Dir, site), map[string]string{
		"index.html":     "<html><head></head><body>sandbox home</body></html>",
		"public/ok.html": "<html><head></head><body>public</body></html>",
		"public/app.css": "body{}",
	})
	writeTree(t, site, map[string]string{"public/app.css": "body{}"})
	e.sandboxAgent(t, a, "tclaude-layer", siteProfile(a.cwd))

	p, rec := e.show(t, a, site, "site", "")
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	for path, want := range map[string]int{
		"/":                  http.StatusOK,
		"/public/ok.html":    http.StatusOK,
		"/secret/key.html":   http.StatusNotFound,
		"/scratch/host.html": http.StatusNotFound,
	} {
		code, body := e.frame(t, p.Origin, path)
		assert.Equal(t, want, code, "%s: %s", path, body)
		assert.NotContains(t, body, "SECRET", path)
		assert.NotContains(t, body, "HOST UNDER TMPFS", path)
	}
	_, body := e.frame(t, p.Origin, "/")
	assert.Contains(t, body, "sandbox home", "the sandbox's file, not the host's")

	e.stubSnapshot(t)
	e.assertReadBack(t, a, p, "site", true)

	// A relaunch into another sandbox: what the browser shows came from the
	// old one, so it is not read back until the page loads again.
	fake.Relaunch()
	code, _ := e.frame(t, p.Origin, "/public/app.css")
	require.Equal(t, http.StatusOK, code)
	e.assertLiveReadBack(t, a, p, "site", false)
	code, _ = e.frame(t, p.Origin, "/")
	require.Equal(t, http.StatusOK, code)
	e.assertLiveReadBack(t, a, p, "site", true)

	// The relaunch may leave a gap with no sandbox at all: the next one is
	// still another sandbox.
	fake.Fail(errors.New("the agent's sandbox is not running"))
	code, _ = e.frame(t, p.Origin, "/public/app.css")
	require.Equal(t, http.StatusServiceUnavailable, code)
	fake.Fail(nil)
	fake.Relaunch()
	code, _ = e.frame(t, p.Origin, "/public/app.css")
	require.Equal(t, http.StatusOK, code)
	e.assertLiveReadBack(t, a, p, "site", false)
	code, _ = e.frame(t, p.Origin, "/")
	require.Equal(t, http.StatusOK, code)

	// A sandbox that cannot be opened serves nothing: never the host's view.
	fake.Fail(errors.New("permission denied"))
	code, body = e.frame(t, p.Origin, "/")
	assert.Equal(t, http.StatusServiceUnavailable, code, body)
	assert.NotContains(t, body, "home")
	rec = e.call(t, a, http.MethodPost, "/v1/pages/site/snapshot", nil)
	assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
}

func TestPageStaticOfATclaudeLayerAgentWhoseSandboxIsNotRunningIsRefused(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("only Linux serves through the agent's mount namespace")
	}
	e := newPageEnv(t)
	a := e.agent(t, "fsnone", true)
	site := staticSite(t, a.cwd)
	fake := &agentd.FakeSandboxRootForTest{Dir: t.TempDir()}
	fake.Fail(errors.New("the agent's sandbox is not running"))
	t.Cleanup(agentd.SetPageSandboxRootForTest(fake))
	e.sandboxAgent(t, a, "tclaude-layer", nil)
	_, rec := e.show(t, a, site, "site", "")
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "sandbox")
}

func TestPageStaticUnderAHarnessSandboxIsServedButNotReadBack(t *testing.T) {
	e := newPageEnv(t)
	a := e.agent(t, "fsharness", true)
	site := staticSite(t, a.cwd)
	// Claude Code's own sandbox is on: its rules are invisible to the stage.
	e.sandboxAgent(t, a, "harness-builtin", siteProfile(a.cwd), claudeSandbox("on", false))

	p, rec := e.show(t, a, site, "site", "")
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	for path, want := range map[string]int{
		"/":                  http.StatusOK,
		"/public/ok.html":    http.StatusOK,
		"/secret/key.html":   http.StatusNotFound, // the recorded profile's rules still apply, by path
		"/secret":            http.StatusNotFound, // no redirect: its existence does not show
		"/scratch/host.html": http.StatusNotFound,
	} {
		code, body := e.frame(t, p.Origin, path)
		assert.Equal(t, want, code, "%s: %s", path, body)
		assert.NotContains(t, body, "SECRET", path)
	}
	_, rec = e.show(t, a, filepath.Join(site, "secret"), "secret", "")
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "sandbox hides")

	t.Cleanup(agentd.SetPageSnapshotRenderForTest(func(agentd.PageSnapshotJobForTest) ([]byte, error) {
		t.Fatal("a page served through a view the stage cannot verify is never rendered for its agent")
		return nil, nil
	}))
	e.assertReadBack(t, a, p, "site", false)
}

func TestPageStaticWithNothingRecordedIsServedButNotReadBack(t *testing.T) {
	e := newPageEnv(t)
	a := e.agent(t, "fsunknown", true)
	site := staticSite(t, a.cwd)
	e.sandboxAgent(t, a, "harness-builtin", nil)
	p, rec := e.show(t, a, site, "site", "")
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	code, _ := e.frame(t, p.Origin, "/")
	assert.Equal(t, http.StatusOK, code, "the operator still sees the page")
	e.assertReadBack(t, a, p, "site", false)
}

func TestPageStaticConstructedRootStillShowsTheStartupDir(t *testing.T) {
	e := newPageEnv(t)
	a := e.agent(t, "fsroot", true)
	site := staticSite(t, a.cwd)
	e.sandboxAgent(t, a, "harness-builtin", &sandboxpolicy.EffectiveProfile{
		FilesystemRoot: sandboxpolicy.FilesystemRootSeparate,
		Filesystem: []sandboxpolicy.FilesystemGrant{
			{Path: filepath.Join(site, "secret"), Access: sandboxpolicy.AccessDeny},
		},
	}, claudeSandbox("on", false))
	// The launch contract binds the startup directory into a constructed root.
	p, rec := e.show(t, a, site, "site", "")
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	code, body := e.frame(t, p.Origin, "/public/ok.html")
	assert.Equal(t, http.StatusOK, code, body)
	code, _ = e.frame(t, p.Origin, "/secret/key.html")
	assert.Equal(t, http.StatusNotFound, code)
}

func TestPageStaticForAnUnconfinedAgentIsReadBack(t *testing.T) {
	for name, launch := range map[string]func(e *pageEnv, a pageAgent){
		"off": func(e *pageEnv, a pageAgent) { e.sandboxAgent(t, a, "off", nil, claudeSandbox("off", false)) },
		"harness sandbox off": func(e *pageEnv, a pageAgent) {
			e.sandboxAgent(t, a, "harness-builtin", siteProfile(a.cwd), claudeSandbox("off", false))
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := newPageEnv(t)
			a := e.agent(t, "fsoff", true)
			site := staticSite(t, a.cwd)
			launch(e, a)
			p, rec := e.show(t, a, site, "site", "")
			require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
			code, _ := e.frame(t, p.Origin, "/")
			require.Equal(t, http.StatusOK, code)
			e.stubSnapshot(t)
			e.assertReadBack(t, a, p, "site", true)
		})
	}

	// A launch tclaude ran with no sandbox that Claude Code still confines (a
	// managed policy keeps its sandbox on) is not unconfined.
	e0 := newPageEnv(t)
	a0 := e0.agent(t, "fsmanaged", true)
	site0 := staticSite(t, a0.cwd)
	e0.sandboxAgent(t, a0, "off", nil, claudeSandbox("on", false))
	p0, rec0 := e0.show(t, a0, site0, "site", "")
	require.Equal(t, http.StatusCreated, rec0.Code, rec0.Body.String())
	e0.assertReadBack(t, a0, p0, "site", false)

	// An unverified "off" verdict is not proof the harness's wall is down.
	e := newPageEnv(t)
	a := e.agent(t, "fsunverified", true)
	site := staticSite(t, a.cwd)
	e.sandboxAgent(t, a, "harness-builtin", nil, claudeSandbox("off", true))
	p, rec := e.show(t, a, site, "site", "")
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	e.assertReadBack(t, a, p, "site", false)
}

// A Codex launch with no sandbox is unconfined only when no verdict overrules
// its mode: managed requirements can keep Codex's own sandbox on despite the
// request (session.VerifyCodexSandboxOff records "on" for such a launch).
func TestPageStaticForACodexAgentWithNoSandboxIsReadBackUnlessRequirementsApply(t *testing.T) {
	for label, verdict := range map[string]struct {
		state      string
		unverified bool
	}{"fscodexoff": {}, "fscodexmanaged": {"on", false}, "fscodexmaybe": {"on", true}} {
		t.Run(label, func(t *testing.T) {
			e := newPageEnv(t)
			a := e.agent(t, label, true)
			site := staticSite(t, a.cwd)
			e.sandboxAgent(t, a, "harness-builtin", nil, func(row *db.SessionRow) {
				row.Harness = "codex"
				row.HarnessBuiltinMode = "danger-full-access"
				row.OSSandboxState = verdict.state
				row.OSSandboxUnverified = verdict.unverified
			})
			p, rec := e.show(t, a, site, "site", "")
			require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
			code, _ := e.frame(t, p.Origin, "/")
			require.Equal(t, http.StatusOK, code, "the operator sees the page either way")
			unconfined := verdict.state == ""
			if unconfined {
				e.stubSnapshot(t)
			}
			e.assertReadBack(t, a, p, "site", unconfined)
		})
	}
}

func TestPageStaticFollowsTheAgentsNextGeneration(t *testing.T) {
	t.Cleanup(agentd.SetPageFSViewRecheckForTest(0))
	e := newPageEnv(t)
	a := e.agent(t, "fsgen", true)
	site := staticSite(t, a.cwd)
	e.sandboxAgent(t, a, "off", nil, claudeSandbox("off", false))
	p, rec := e.show(t, a, site, "site", "")
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	code, _ := e.frame(t, p.Origin, "/")
	require.Equal(t, http.StatusOK, code)
	e.stubSnapshot(t)
	e.assertReadBack(t, a, p, "site", true)

	// The agent comes back confined by its harness: the page stops being
	// read back, and the hidden part stops being served.
	next := a.conv + "-next"
	e.f.HaveAliveSession(next, "fsgen-next", "tmux-page-fsgen-next", a.cwd)
	require.NoError(t, db.LinkConvToAgent(next, a.id, db.ConvRoleHead, "reincarnate"))
	moved, err := db.SetAgentCurrentConv(a.id, a.conv, next)
	require.NoError(t, err)
	require.True(t, moved)
	require.NoError(t, db.GrantAgentPermission(next, agentd.PermSelfPage, "test"))
	a = pageAgent{conv: next, id: a.id, cwd: a.cwd}
	row, err := db.FindSessionByConvID(next)
	require.NoError(t, err)
	require.NotNil(t, row)
	row.SandboxImplementation = "harness-builtin"
	snap := sandboxpolicy.NewSnapshot(*siteProfile(a.cwd), nil)
	row.EffectiveSandbox = &snap
	claudeSandbox("on", false)(row)
	require.NoError(t, db.SaveSession(row))
	code, _ = e.frame(t, p.Origin, "/secret/key.html")
	assert.Equal(t, http.StatusNotFound, code)
	rec = e.call(t, a, http.MethodPost, "/v1/pages/site/snapshot", nil)
	assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
}

func TestPageStaticFollowsARelaunchOfTheSameConversation(t *testing.T) {
	e := newPageEnv(t)
	a := e.agent(t, "fsrestore", true)
	site := staticSite(t, a.cwd)
	// Shown during a temporary sandbox unlock.
	e.sandboxAgent(t, a, "off", nil, claudeSandbox("off", false))
	p, rec := e.show(t, a, site, "site", "")
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	code, _ := e.frame(t, p.Origin, "/")
	require.Equal(t, http.StatusOK, code)
	e.stubSnapshot(t)
	e.assertReadBack(t, a, p, "site", true)

	// The sandbox is restored: the same conversation resumes confined by its
	// harness. The page is not read back any more, at once.
	e.sandboxAgent(t, a, "harness-builtin", siteProfile(a.cwd), claudeSandbox("on", false))
	e.assertReadBack(t, a, p, "site", false)
}
