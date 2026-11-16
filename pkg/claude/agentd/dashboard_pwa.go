package agentd

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"strings"
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

// dashboardPWAIconPrefix is where the icons are served (GET /pwa/{file}),
// from dashboard/pwa/.
const dashboardPWAIconPrefix = "/pwa/"

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

var dashboardManifestJSON = func() []byte {
	icon := func(file, sizes, purpose string) dashboardManifestIcon {
		return dashboardManifestIcon{Src: dashboardPWAIconPrefix + file, Sizes: sizes, Type: "image/png", Purpose: purpose}
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
}()

func registerDashboardPWARoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /manifest.webmanifest", handleDashboardManifest)
	mux.HandleFunc("GET /pwa/{file}", handleDashboardPWAIcon)
}

func handleDashboardManifest(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/manifest+json")
	// Revalidated, so a changed manifest reaches an installed app's next
	// update check.
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(dashboardManifestJSON)
}

// handleDashboardPWAIcon serves one of dashboard/pwa/'s PNGs, and nothing
// else: no other asset is reachable without the session this way.
func handleDashboardPWAIcon(w http.ResponseWriter, r *http.Request) {
	file := r.PathValue("file")
	if !strings.HasSuffix(file, ".png") || strings.ContainsAny(file, `/\`) {
		http.NotFound(w, r)
		return
	}
	data, err := fs.ReadFile(dashboardAssetsFS, "pwa/"+file)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write(data)
}
