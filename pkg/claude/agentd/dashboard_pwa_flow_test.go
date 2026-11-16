package agentd_test

import (
	"encoding/json"
	"fmt"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/agentd"
)

// The dashboard is installable as an app: the page links a web app manifest,
// and the manifest and its icons load without the dashboard session (a
// browser's install check and an installed app's update fetch them outside
// the page's own requests), while the rest of the dashboard's assets stay
// behind it.
func TestDashboardPWA_ManifestAndIconsLoadWithoutASession(t *testing.T) {
	mux := http.NewServeMux()
	agentd.RegisterDashboardRoutesForTest(mux)
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}

	rec := get("/manifest.webmanifest")
	require.Equal(t, http.StatusOK, rec.Code, "no cookie, no Origin: still served")
	assert.Equal(t, "application/manifest+json", rec.Header().Get("Content-Type"))
	assert.Equal(t, "no-cache", rec.Header().Get("Cache-Control"))
	var manifest struct {
		Name       string `json:"name"`
		ShortName  string `json:"short_name"`
		StartURL   string `json:"start_url"`
		Scope      string `json:"scope"`
		Display    string `json:"display"`
		Theme      string `json:"theme_color"`
		Background string `json:"background_color"`
		Icons      []struct {
			Src     string `json:"src"`
			Sizes   string `json:"sizes"`
			Purpose string `json:"purpose"`
		} `json:"icons"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &manifest))
	assert.Equal(t, "tclaude", manifest.Name)
	assert.Equal(t, "tclaude", manifest.ShortName)
	assert.Equal(t, "standalone", manifest.Display, "no address bar once installed")
	assert.Equal(t, "/", manifest.StartURL, "host-relative: the same manifest serves every origin")
	assert.Equal(t, "/", manifest.Scope)
	assert.NotEmpty(t, manifest.Theme)
	assert.NotEmpty(t, manifest.Background)

	sizes := map[string]bool{}
	for _, icon := range manifest.Icons {
		assert.Regexp(t, `^/pwa/v-[0-9a-f]{16}/[a-z0-9-]+\.png$`, icon.Src, "icon %s is host-relative and content-hashed", icon.Src)
		got := get(icon.Src)
		require.Equal(t, http.StatusOK, got.Code, "icon %s loads without a session", icon.Src)
		assert.Equal(t, "image/png", got.Header().Get("Content-Type"))
		assert.Equal(t, "public, max-age=31536000, immutable", got.Header().Get("Cache-Control"), "a versioned icon never changes")
		img, err := png.Decode(got.Body)
		require.NoError(t, err, "icon %s is a PNG", icon.Src)
		b := img.Bounds()
		assert.Equal(t, icon.Sizes, fmt.Sprintf("%dx%d", b.Dx(), b.Dy()), "icon %s is the size it says", icon.Src)
		sizes[icon.Sizes+" "+icon.Purpose] = true
	}
	assert.True(t, sizes["192x192 any"], "a 192px icon (Chrome's installability minimum)")
	assert.True(t, sizes["512x512 any"], "a 512px icon (the splash screen)")
	assert.True(t, sizes["512x512 maskable"], "a maskable icon (Android launchers' shapes)")
	assert.Equal(t, http.StatusOK, get("/pwa/apple-touch-icon.png").Code, "iOS's home-screen icon")

	// An app installed before the URLs were versioned asks for the bare path,
	// and one with a manifest from before a deploy for an old version: both
	// get the current icon, revalidated every time rather than cached for a
	// day. The ETag makes the revalidation cheap.
	versioned := get(manifest.Icons[0].Src)
	for _, path := range []string{"/pwa/icon-192.png", "/pwa/v-0123456789abcdef/icon-192.png"} {
		got := get(path)
		require.Equal(t, http.StatusOK, got.Code, path)
		assert.Equal(t, "no-cache", got.Header().Get("Cache-Control"), path)
		assert.Equal(t, versioned.Body.Bytes(), got.Body.Bytes(), "%s is the current icon", path)
		etag := got.Header().Get("ETag")
		require.NotEmpty(t, etag, path)
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("If-None-Match", etag)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusNotModified, rec.Code, "%s revalidates", path)
	}

	// Only the PNGs under pwa/ are public: nothing else, and no other asset.
	for _, path := range []string{"/pwa/missing.png", "/pwa/..%2Fdashboard.html", "/pwa/icon-192.svg", "/pwa/", "/pwa/x/icon-192.png", "/pwa/v-1/missing.png"} {
		assert.Equal(t, http.StatusNotFound, get(path).Code, path)
	}
	assert.Equal(t, http.StatusForbidden, get("/static/js/dashboard.js").Code, "the dashboard's own assets still need the session")
}

// The page links the manifest with credentials (an Access-guarded origin
// needs its cookie on the fetch), with the theme colour and iOS's tags.
func TestDashboardPWA_PageLinksTheManifest(t *testing.T) {
	html := string(agentd.DashboardIndexHTMLForTest())
	for _, tag := range []string{
		`<link rel="manifest" href="/manifest.webmanifest" crossorigin="use-credentials">`,
		`<meta name="theme-color" content="#161b22">`,
		`<meta name="apple-mobile-web-app-capable" content="yes">`,
	} {
		assert.Contains(t, html, tag)
	}
	// iOS's icon link is content-hashed like the manifest's icons.
	assert.Regexp(t, `<link rel="apple-touch-icon" href="/pwa/v-[0-9a-f]{16}/apple-touch-icon\.png">`, html)
	assert.NotContains(t, html, `"/pwa/apple-touch-icon.png"`)
}

// A changed icon gets a new URL in the manifest, so no cache (a CDN in front
// of a tunnel, the phone) can serve the old PNG under it; an unchanged one
// keeps its URL.
func TestDashboardPWA_IconURLsFollowTheIconBytes(t *testing.T) {
	icons := func(maskable string) fstest.MapFS {
		return fstest.MapFS{
			"pwa/icon-192.png":          {Data: []byte("any 192")},
			"pwa/icon-512.png":          {Data: []byte("any 512")},
			"pwa/icon-maskable-512.png": {Data: []byte(maskable)},
		}
	}
	srcs := func(fsys fstest.MapFS) map[string]string {
		var manifest struct {
			Icons []struct {
				Src     string `json:"src"`
				Purpose string `json:"purpose"`
				Sizes   string `json:"sizes"`
			} `json:"icons"`
		}
		require.NoError(t, json.Unmarshal(agentd.DashboardManifestForTest(fsys), &manifest))
		out := map[string]string{}
		for _, icon := range manifest.Icons {
			out[icon.Sizes+" "+icon.Purpose] = icon.Src
		}
		return out
	}
	before := srcs(icons("dark ring"))
	after := srcs(icons("full-bleed orange"))
	assert.NotEqual(t, before["512x512 maskable"], after["512x512 maskable"], "the changed icon has a new URL")
	assert.True(t, strings.HasSuffix(after["512x512 maskable"], "/icon-maskable-512.png"))
	assert.Equal(t, before["192x192 any"], after["192x192 any"], "an unchanged icon keeps its URL")
	assert.Equal(t, before["512x512 any"], after["512x512 any"])
}
