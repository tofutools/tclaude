package agentd

import (
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// dashboardHTML is the entire single-page UI. Lives in its own file so
// JS template literals (backticks) don't collide with Go raw strings.
//
//go:embed dashboard.html
var dashboardHTML string

// dashboardSessionToken is generated once per agentd process and gates
// every /api/* request. It's set as an HttpOnly + SameSite=Strict
// cookie on the first GET of /, and checked on subsequent /api/*
// fetches. Same threat model as the popup: defense-in-depth against
// drive-by browser tabs and scraped-URL replay; a same-user process
// with /proc access can still snoop, which we accept as the
// inherent same-user trust boundary on this machine.
//
// Empty until initDashboardToken runs in startPopupServer.
var dashboardSessionToken string

const dashboardCookieName = "tclaude_dashboard_session"

func initDashboardToken() {
	if dashboardSessionToken != "" {
		return
	}
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Cryptographic randomness is required for the token to be
		// unguessable. If we can't get it, leave the dashboard
		// disabled (token stays empty → checkDashboardAuth refuses
		// every /api request).
		return
	}
	dashboardSessionToken = hex.EncodeToString(b[:])
}

// registerDashboardRoutes wires the dashboard onto the popup-server
// mux. We share the listener since both views are loopback-only and
// the human only ever wants one process serving them.
func registerDashboardRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/", handleDashboardRoot)
	mux.HandleFunc("/api/snapshot", handleDashboardSnapshot)
}

// handleDashboardRoot serves the HTML page. Sets the dashboard
// session cookie if it isn't present, so subsequent /api/* fetches
// authenticate.
func handleDashboardRoot(w http.ResponseWriter, r *http.Request) {
	// `/` is a catch-all in net/http; reject anything we don't know
	// so /favicon.ico etc. don't silently render the dashboard HTML.
	if r.URL.Path != "/" && r.URL.Path != "/dashboard" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	if dashboardSessionToken == "" {
		http.Error(w, "dashboard token not initialised", http.StatusServiceUnavailable)
		return
	}
	if c, err := r.Cookie(dashboardCookieName); err != nil || c.Value != dashboardSessionToken {
		http.SetCookie(w, &http.Cookie{
			Name:     dashboardCookieName,
			Value:    dashboardSessionToken,
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteStrictMode,
		})
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(dashboardHTML))
}

// checkDashboardAuth mirrors checkPopupAuth: cookie value match +
// Origin/Referer pinned to the popup base URL. Refuses every
// /api/* call when the dashboard token isn't set (cryptographic
// randomness failed at startup).
func checkDashboardAuth(w http.ResponseWriter, r *http.Request) bool {
	if dashboardSessionToken == "" {
		http.Error(w, "dashboard not initialised", http.StatusServiceUnavailable)
		return false
	}
	c, err := r.Cookie(dashboardCookieName)
	if err != nil || c.Value != dashboardSessionToken {
		http.Error(w, "missing or invalid dashboard cookie; load / first", http.StatusForbidden)
		return false
	}
	origin := r.Header.Get("Origin")
	referer := r.Header.Get("Referer")
	if origin == "" && referer == "" {
		http.Error(w, "missing Origin and Referer", http.StatusForbidden)
		return false
	}
	if origin != "" && !strings.HasPrefix(origin, popupBaseURL) {
		http.Error(w, "Origin mismatch", http.StatusForbidden)
		return false
	}
	if origin == "" && !strings.HasPrefix(referer, popupBaseURL) {
		http.Error(w, "Referer mismatch", http.StatusForbidden)
		return false
	}
	return true
}

// snapshotPayload is the wire shape for /api/snapshot. One round-trip
// gives the page everything it needs to render every tab; the page
// re-fetches on a 5s timer.
type snapshotPayload struct {
	GeneratedAt string                  `json:"generated_at"`
	Groups      []dashboardGroup        `json:"groups"`
	Agents      []dashboardAgent        `json:"agents"`
	Permissions snapshotPermissionsView `json:"permissions"`
	Slugs       []PermSlug              `json:"slugs"`
	PopupBase   string                  `json:"popup_base"` // for tray-shareable display
}

type snapshotPermissionsView struct {
	Defaults []string            `json:"defaults"`
	Grants   map[string][]string `json:"grants"`
}

type dashboardGroup struct {
	Name    string            `json:"name"`
	Descr   string            `json:"descr"`
	Members []dashboardMember `json:"members"`
	Online  int               `json:"online"`
}

type dashboardMember struct {
	ConvID string `json:"conv_id"`
	Title  string `json:"title"`
	Alias  string `json:"alias,omitempty"`
	Role   string `json:"role,omitempty"`
	Descr  string `json:"descr,omitempty"`
	Online bool   `json:"online"`
}

type dashboardAgent struct {
	ConvID    string   `json:"conv_id"`
	Title     string   `json:"title"`
	Online    bool     `json:"online"`
	Groups    []string `json:"groups"`
	Effective []string `json:"effective"` // perms = union(defaults, per-conv grants)
}

// handleDashboardSnapshot returns one JSON blob covering everything
// the page renders: groups + members, all known agents, the live
// permission state (defaults + per-conv grants), and the slug
// registry. Read-only.
func handleDashboardSnapshot(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	groups, _ := db.ListAgentGroups()
	allGrants, _ := db.ListAllAgentPermissions()
	cfg, _ := config.Load()
	defaults := []string{}
	if cfg != nil && cfg.Agent != nil {
		defaults = append(defaults, cfg.Agent.DefaultPermissions...)
	}
	sort.Strings(defaults)

	// agentRows: union of (every group member) + (every conv-id with
	// explicit grants). Keyed by conv-id so members appearing in
	// multiple groups dedupe naturally.
	agentRows := map[string]*dashboardAgent{}
	addAgent := func(convID string) *dashboardAgent {
		if existing, ok := agentRows[convID]; ok {
			return existing
		}
		row, _ := db.GetConvIndex(convID)
		title := "(unknown)"
		if row != nil {
			if t := agent.DisplayTitle(row); t != "" {
				title = t
			}
		}
		a := &dashboardAgent{
			ConvID: convID,
			Title:  title,
			Online: isConvOnline(convID),
		}
		agentRows[convID] = a
		return a
	}

	out := snapshotPayload{
		GeneratedAt: time.Now().Format(time.RFC3339),
		PopupBase:   popupBaseURL,
		Permissions: snapshotPermissionsView{
			Defaults: defaults,
			Grants:   map[string][]string{},
		},
		Slugs: append([]PermSlug{}, permissionRegistry...),
	}
	sort.Slice(out.Slugs, func(i, j int) bool { return out.Slugs[i].Slug < out.Slugs[j].Slug })

	for _, g := range groups {
		dg := dashboardGroup{Name: g.Name, Descr: g.Descr}
		members, _ := db.ListAgentGroupMembers(g.ID)
		for _, m := range members {
			row, _ := db.GetConvIndex(m.ConvID)
			title := "(unknown)"
			if row != nil {
				if t := agent.DisplayTitle(row); t != "" {
					title = t
				}
			}
			online := isConvOnline(m.ConvID)
			dg.Members = append(dg.Members, dashboardMember{
				ConvID: m.ConvID,
				Title:  title,
				Alias:  m.Alias,
				Role:   m.Role,
				Descr:  m.Descr,
				Online: online,
			})
			if online {
				dg.Online++
			}
			a := addAgent(m.ConvID)
			a.Groups = append(a.Groups, g.Name)
		}
		out.Groups = append(out.Groups, dg)
	}
	for convID, slugs := range allGrants {
		addAgent(convID)
		copySlice := append([]string{}, slugs...)
		sort.Strings(copySlice)
		out.Permissions.Grants[convID] = copySlice
	}
	for _, a := range agentRows {
		// Effective = defaults ∪ grants. Defaults come from config;
		// grants from agent_permissions for that conv.
		seen := map[string]bool{}
		merged := []string{}
		for _, s := range defaults {
			if !seen[s] {
				seen[s] = true
				merged = append(merged, s)
			}
		}
		if extras, ok := out.Permissions.Grants[a.ConvID]; ok {
			for _, s := range extras {
				if !seen[s] {
					seen[s] = true
					merged = append(merged, s)
				}
			}
		}
		sort.Strings(merged)
		a.Effective = merged
		sort.Strings(a.Groups)
		out.Agents = append(out.Agents, *a)
	}
	sort.Slice(out.Agents, func(i, j int) bool {
		return out.Agents[i].Title < out.Agents[j].Title
	})

	writeJSON(w, http.StatusOK, out)
}

