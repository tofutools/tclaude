package agentd_test

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// Static live pages serve only what the page's agent can read (tcl-kxiqqd):
// the stage follows the agent's recorded sandbox plan, and a page whose plan
// is unknown is served but never read back.

// sandboxAgent records a launch for a's session: the implementation and, when
// eff is non-nil, the effective profile it ran under.
func (e *pageEnv) sandboxAgent(t *testing.T, a pageAgent, implementation string, eff *sandboxpolicy.EffectiveProfile) {
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
	require.NoError(t, db.SaveSession(row))
}

// staticSite writes a site under the agent's startup directory with a public
// part, a part the profile denies and a part a tmpfs covers.
func staticSite(t *testing.T, cwd string) string {
	t.Helper()
	site := filepath.Join(cwd, "site")
	for rel, body := range map[string]string{
		"index.html":        "<html><head></head><body>home</body></html>",
		"public/ok.html":    "<html><head></head><body>public</body></html>",
		"secret/key.html":   "<html><head></head><body>SECRET</body></html>",
		"secret/notes.txt":  "SECRET NOTES",
		"scratch/host.html": "<html><head></head><body>HOST UNDER TMPFS</body></html>",
	} {
		p := filepath.Join(site, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte(body), 0o644))
	}
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

func TestPageStaticServesOnlyWhatTheAgentsSandboxShows(t *testing.T) {
	e := newPageEnv(t)
	a := e.agent(t, "fsview", true)
	site := staticSite(t, a.cwd)
	e.sandboxAgent(t, a, "tclaude-layer", siteProfile(a.cwd))

	p, rec := e.show(t, a, site, "site", "")
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	for path, want := range map[string]int{
		"/":                  http.StatusOK,
		"/public/ok.html":    http.StatusOK,
		"/secret/key.html":   http.StatusNotFound, // a deny rule
		"/secret/notes.txt":  http.StatusNotFound,
		"/secret/":           http.StatusNotFound,
		"/secret":            http.StatusNotFound, // no redirect: its existence does not show
		"/scratch/host.html": http.StatusNotFound, // the host file under the agent's tmpfs
	} {
		code, body := e.frame(t, p.Origin, path)
		assert.Equal(t, want, code, "%s: %s", path, body)
		assert.NotContains(t, body, "SECRET", path)
		assert.NotContains(t, body, "HOST UNDER TMPFS", path)
	}

	// A target the sandbox hides is refused outright.
	_, rec = e.show(t, a, filepath.Join(site, "secret"), "secret", "")
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "sandbox hides")

	// With the agent's view known, the page is read back like a dev server's.
	t.Cleanup(agentd.SetPageSnapshotRenderForTest(func(agentd.PageSnapshotJobForTest) ([]byte, error) {
		return []byte("png"), nil
	}))
	rec = e.call(t, a, http.MethodPost, "/v1/pages/site/snapshot", nil)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = e.call(t, a, http.MethodPost, "/v1/pages/site/dom", map[string]any{"selector": "body"})
	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Contains(t, rec.Body.String(), "no dashboard view", "refused for want of a view, not for being static")
	rec = e.dashJSON(t, http.MethodPost, "/api/pages/"+p.ID+"/marks", sampleMarks())
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got := e.marks(t, a, "site", "")
	require.Len(t, got.Batches, 1)
	assert.NotEmpty(t, got.Batches[0].Marks[0].Anchors, "the text under marks arrives")
}

func TestPageStaticConstructedRootServesOnlyBoundPaths(t *testing.T) {
	e := newPageEnv(t)
	a := e.agent(t, "fsroot", true)
	site := staticSite(t, a.cwd)
	e.sandboxAgent(t, a, "tclaude-layer", &sandboxpolicy.EffectiveProfile{
		FilesystemRoot: sandboxpolicy.FilesystemRootSeparate,
		Filesystem: []sandboxpolicy.FilesystemGrant{
			{Path: filepath.Join(site, "public"), Access: sandboxpolicy.AccessRead},
		},
	})
	// The site's root is not bound: showing it is refused; its bound part is served.
	_, rec := e.show(t, a, site, "site", "")
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	p, rec := e.show(t, a, filepath.Join(site, "public"), "public", "")
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	code, body := e.frame(t, p.Origin, "/ok.html")
	assert.Equal(t, http.StatusOK, code, body)
}

func TestPageStaticWithAnUnknownSandboxIsServedButNotReadBack(t *testing.T) {
	e := newPageEnv(t)
	a := e.agent(t, "fsunknown", true)
	site := staticSite(t, a.cwd)
	// Sandboxed, but the launch recorded no profile.
	e.sandboxAgent(t, a, "tclaude-layer", nil)
	p, rec := e.show(t, a, site, "site", "")
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	code, _ := e.frame(t, p.Origin, "/")
	assert.Equal(t, http.StatusOK, code, "the operator still sees the page")

	t.Cleanup(agentd.SetPageSnapshotRenderForTest(func(agentd.PageSnapshotJobForTest) ([]byte, error) {
		t.Fatal("a page whose agent's view is unknown is never rendered for it")
		return nil, nil
	}))
	rec = e.call(t, a, http.MethodPost, "/v1/pages/site/snapshot", nil)
	assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "recorded no sandbox profile")
	rec = e.call(t, a, http.MethodPost, "/v1/pages/site/dom", map[string]any{"selector": "body"})
	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Contains(t, rec.Body.String(), "recorded no sandbox profile")
	rec = e.dashJSON(t, http.MethodPost, "/api/pages/"+p.ID+"/marks", sampleMarks())
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got := e.marks(t, a, "site", "")
	require.Len(t, got.Batches, 1)
	for _, m := range got.Batches[0].Marks {
		assert.Empty(t, m.Anchors)
	}
}

func TestPageStaticForAnUnconfinedAgentIsReadBack(t *testing.T) {
	e := newPageEnv(t)
	a := e.agent(t, "fsoff", true)
	site := staticSite(t, a.cwd)
	e.sandboxAgent(t, a, "off", nil)
	_, rec := e.show(t, a, site, "site", "")
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	t.Cleanup(agentd.SetPageSnapshotRenderForTest(func(agentd.PageSnapshotJobForTest) ([]byte, error) {
		return []byte("png"), nil
	}))
	rec = e.call(t, a, http.MethodPost, "/v1/pages/site/snapshot", nil)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}
