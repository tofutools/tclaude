package agentd_test

import (
	"encoding/json"
	"fmt"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
		assert.True(t, strings.HasPrefix(icon.Src, "/pwa/"), "icon %s is host-relative", icon.Src)
		got := get(icon.Src)
		require.Equal(t, http.StatusOK, got.Code, "icon %s loads without a session", icon.Src)
		assert.Equal(t, "image/png", got.Header().Get("Content-Type"))
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

	// Only the PNGs under pwa/ are public: nothing else, and no other asset.
	for _, path := range []string{"/pwa/missing.png", "/pwa/..%2Fdashboard.html", "/pwa/icon-192.svg", "/pwa/"} {
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
		`<link rel="apple-touch-icon" href="/pwa/apple-touch-icon.png">`,
		`<meta name="apple-mobile-web-app-capable" content="yes">`,
	} {
		assert.Contains(t, html, tag)
	}
}
