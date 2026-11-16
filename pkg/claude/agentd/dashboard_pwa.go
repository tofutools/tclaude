package agentd

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"net/http"
	"strings"
	"time"
)

// The dashboard as an installable web app: a phone's "Add to home screen"
// (or "Install app") then opens it standalone, without the browser's address
// bar. dashboard.html links the manifest; the manifest names the icons.
//
// Unlike /static/, the manifest and its icons are served without the
// dashboard session: they describe the app and hold nothing about this node,
// and a browser fetches them outside the page's own requests (Chrome's
// installability check and its periodic manifest update, Android's WebAPK
// icons), without the Origin / Referer the session gate pins. Whatever stands
// in front of the dashboard still applies: Cloudflare Access sees its own
// cookie (the page links the manifest crossorigin="use-credentials" so it is
// sent), and the remote listener's boundary gates every path.
//
// start_url and scope are "/" and the icon paths host-relative, so one
// manifest serves every origin the dashboard is reached on (localhost, a
// tunnel's hostname, the remote listener).
//
// Icon URLs carry each icon's content hash (/pwa/v-<hash>/<file>), and those
// are cached for good: a changed icon gets a new URL, so neither a CDN in
// front of a tunnel nor the phone can hand back the old PNG. The manifest,
// which names the current URLs, is no-cache, so a deploy is picked up on the
// browser's next update check. The bare /pwa/<file> an app installed before
// the hash keeps working, revalidated on every fetch.

// dashboardPWAIconPrefix is where the icons are served (GET /pwa/{file}),
// from dashboard/pwa/.
const dashboardPWAIconPrefix = "/pwa/"

// dashboardPWAIconImmutable is a versioned icon's Cache-Control. public: the
// icons are served without the session and hold nothing about the node.
const dashboardPWAIconImmutable = "public, max-age=31536000, immutable"

// pwaIconHash is an icon's content hash, as its URL and ETag carry it.
func pwaIconHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])[:16]
}

// pwaIconURL is the versioned URL of dashboard/pwa/<file> in fsys.
func pwaIconURL(fsys fs.FS, file string) string {
	data, err := fs.ReadFile(fsys, "pwa/"+file)
	if err != nil {
		panic("agentd: reading web app icon " + file + ": " + err.Error())
	}
	return dashboardPWAIconPrefix + "v-" + pwaIconHash(data) + "/" + file
}

// versionPWARefs rewrites a page's quoted "/pwa/<file>" references (the
// apple-touch-icon link) to the icons' versioned URLs.
func versionPWARefs(html []byte, fsys fs.FS) []byte {
	entries, err := fs.ReadDir(fsys, "pwa")
	if err != nil {
		panic("agentd: listing web app icons: " + err.Error())
	}
	for _, e := range entries {
		ref := `"` + dashboardPWAIconPrefix + e.Name() + `"`
		html = bytes.ReplaceAll(html, []byte(ref), []byte(`"`+pwaIconURL(fsys, e.Name())+`"`))
	}
	return html
}

// The dark theme's colours: the page behind the launch splash, and the phone
// bar's, which the system status bar takes.
const (
	dashboardPWABackground = "#0f1419"
	dashboardPWATheme      = "#161b22"
)

type dashboardManifestIcon struct {
	Src     string `json:"src"`
	Sizes   string `json:"sizes"`
	Type    string `json:"type"`
	Purpose string `json:"purpose,omitempty"`
}

type dashboardManifest struct {
	ID              string                  `json:"id"`
	Name            string                  `json:"name"`
	ShortName       string                  `json:"short_name"`
	Description     string                  `json:"description"`
	StartURL        string                  `json:"start_url"`
	Scope           string                  `json:"scope"`
	Display         string                  `json:"display"`
	BackgroundColor string                  `json:"background_color"`
	ThemeColor      string                  `json:"theme_color"`
	Icons           []dashboardManifestIcon `json:"icons"`
}

var dashboardManifestJSON = buildDashboardManifest(dashboardAssetsFS)

// buildDashboardManifest is the manifest for the icons in fsys's pwa/.
func buildDashboardManifest(fsys fs.FS) []byte {
	icon := func(file, sizes, purpose string) dashboardManifestIcon {
		return dashboardManifestIcon{Src: pwaIconURL(fsys, file), Sizes: sizes, Type: "image/png", Purpose: purpose}
	}
	body, err := json.MarshalIndent(dashboardManifest{
		ID:              "/",
		Name:            "tclaude",
		ShortName:       "tclaude",
		Description:     "The tclaude dashboard: your agents, their terminals and the fleet.",
		StartURL:        "/",
		Scope:           "/",
		Display:         "standalone",
		BackgroundColor: dashboardPWABackground,
		ThemeColor:      dashboardPWATheme,
		Icons: []dashboardManifestIcon{
			icon("icon-192.png", "192x192", "any"),
			icon("icon-512.png", "512x512", "any"),
			icon("icon-maskable-512.png", "512x512", "maskable"),
		},
	}, "", "  ")
	if err != nil {
		panic(err)
	}
	return body
}

func registerDashboardPWARoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /manifest.webmanifest", handleDashboardManifest)
	mux.HandleFunc("GET /pwa/{file}", handleDashboardPWAIcon)
	mux.HandleFunc("GET /pwa/{version}/{file}", handleDashboardPWAIcon)
}

func handleDashboardManifest(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/manifest+json")
	// Revalidated, so a changed manifest reaches an installed app's next
	// update check.
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(dashboardManifestJSON)
}

// handleDashboardPWAIcon serves one of dashboard/pwa/'s PNGs, and nothing
// else: no other asset is reachable without the session this way. At its
// current versioned URL an icon is immutable. Bare (an app installed before
// icon URLs were versioned) or at another version (a manifest from before a
// deploy), it is the current icon, revalidated against its ETag every time.
func handleDashboardPWAIcon(w http.ResponseWriter, r *http.Request) {
	file := r.PathValue("file")
	version := r.PathValue("version")
	if !strings.HasSuffix(file, ".png") || strings.ContainsAny(file, `/\`) ||
		(version != "" && !strings.HasPrefix(version, "v-")) {
		http.NotFound(w, r)
		return
	}
	data, err := fs.ReadFile(dashboardAssetsFS, "pwa/"+file)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	hash := pwaIconHash(data)
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("ETag", `"`+hash+`"`)
	if version == "v-"+hash {
		w.Header().Set("Cache-Control", dashboardPWAIconImmutable)
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(data))
}
