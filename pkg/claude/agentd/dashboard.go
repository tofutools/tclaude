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
	"github.com/tofutools/tclaude/pkg/claude/session"
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
	registerDashboardEditRoutes(mux)
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
	GeneratedAt string           `json:"generated_at"`
	Groups      []dashboardGroup `json:"groups"`
	Agents      []dashboardAgent `json:"agents"`
	// Ungrouped: every conv-id that has a live tmux session but is NOT
	// a member of any group. Surfaces fresh-spawned agents and other
	// loose convs so the eventual "Ungrouped virtual group" + the
	// `+ add member` overlay can show them as drag/add sources without
	// a second round-trip. Same wire shape as Agents — empty when no
	// loose convs exist.
	Ungrouped   []dashboardAgent        `json:"ungrouped"`
	Permissions snapshotPermissionsView `json:"permissions"`
	Slugs       []PermSlug              `json:"slugs"`
	Cron        []dashboardCronJob      `json:"cron"`
	// Sudo: every active grant across all agents, ordered by conv-id
	// then soonest expiry. Powers the dedicated "Sudo" tab. Per-agent
	// active state also surfaces on Agents[*].ActiveSudo so the Groups
	// + Agents tabs can render the 🔓 indicator without a second
	// round-trip.
	Sudo []dashboardSudoEntry `json:"sudo"`
	// Links surfaces every inter-group link in the system. The dashboard
	// renders these in a dedicated panel (read-only in v1) and uses them
	// to annotate group rows with outbound/inbound counts. Empty slice
	// (not nil) so JS .length / .map() are safe.
	Links     []dashboardLink `json:"links"`
	PopupBase string          `json:"popup_base"` // for tray-shareable display
}

// dashboardLink is the snapshot view of one agent_group_links row.
// Group names are pre-resolved so the renderer doesn't need to do a
// second lookup.
type dashboardLink struct {
	ID        int64  `json:"id"`
	From      string `json:"from"`
	To        string `json:"to"`
	Mode      string `json:"mode"`
	CreatedAt string `json:"created_at,omitempty"`
}

// dashboardCronJob is the snapshot view of one agent_cron_jobs row.
// Mirrors jobJSON in cron_handlers.go but adds a few resolved fields
// — owner/target labels and the most-recent run row — so the
// dashboard can render a self-contained table without a second
// fetch per row.
type dashboardCronJob struct {
	ID              int64  `json:"id"`
	Name            string `json:"name"`
	OwnerConv       string `json:"owner_conv"`
	OwnerLabel      string `json:"owner_label"`
	TargetConv      string `json:"target_conv,omitempty"`
	TargetLabel     string `json:"target_label,omitempty"`
	GroupID         int64  `json:"group_id"`
	GroupName       string `json:"group_name,omitempty"`
	IntervalSeconds int64  `json:"interval_seconds"`
	Subject         string `json:"subject,omitempty"`
	Body            string `json:"body"`
	Enabled         bool   `json:"enabled"`
	CreatedAt       string `json:"created_at,omitempty"`
	LastRunAt       string `json:"last_run_at,omitempty"`
	LastRunStatus   string `json:"last_run_status,omitempty"`
	NextDueAt       string `json:"next_due_at,omitempty"`
}

type snapshotPermissionsView struct {
	Defaults []string            `json:"defaults"`
	Grants   map[string][]string `json:"grants"`
}

type dashboardGroup struct {
	Name       string            `json:"name"`
	Descr      string            `json:"descr"`
	DefaultCwd string            `json:"default_cwd"` // pre-fills the spawn form's cwd; "" = none
	Members    []dashboardMember `json:"members"`
	Online     int               `json:"online"`
}

// dashboardMember.Owner mirrors the memberJSON convention from
// /v1/groups/{name}/members:
//   - true on a member row → that member is also a group owner
//     (rendered as a badge alongside the role).
//   - true on a row with Role=="owner" and no alias/descr → a
//     pure owner who isn't a member (so the list stays comprehensive).
type dashboardMember struct {
	ConvID string     `json:"conv_id"`
	Title  string     `json:"title"`
	Alias  string     `json:"alias,omitempty"`
	Role   string     `json:"role,omitempty"`
	Descr  string     `json:"descr,omitempty"`
	Branch string     `json:"branch,omitempty"` // git branch / worktree, from conv_index
	Online bool       `json:"online"`
	Owner  bool       `json:"owner,omitempty"`
	State  agentState `json:"state"`
}

type dashboardAgent struct {
	ConvID      string               `json:"conv_id"`
	Title       string               `json:"title"`
	Branch      string               `json:"branch,omitempty"` // git branch / worktree, from conv_index
	Online      bool                 `json:"online"`
	State       agentState           `json:"state"`
	Groups      []string             `json:"groups"`
	OwnedGroups []string             `json:"owned_groups"`          // subset of Groups the agent owns; UI tags these distinctly
	Effective   []string             `json:"effective"`             // perms = union(defaults, per-conv grants)
	ActiveSudo  []dashboardSudoEntry `json:"active_sudo,omitempty"` // current sudo grants (slug + id + remaining); empty when none
}

// dashboardSudoEntry is the wire shape for one active sudo grant in
// the snapshot. Used both as agent[*].active_sudo[] (per-row "this
// agent currently holds these") and as the top-level Sudo[] (full
// list across all agents for the dedicated tab).
type dashboardSudoEntry struct {
	ID               int64  `json:"id"`
	ConvID           string `json:"conv_id,omitempty"` // omitted on agent[*].active_sudo (caller already knows)
	ConvTitle        string `json:"conv_title,omitempty"`
	Slug             string `json:"slug"`
	GrantedAt        string `json:"granted_at"`
	ExpiresAt        string `json:"expires_at"`
	GrantedBy        string `json:"granted_by,omitempty"`
	Reason           string `json:"reason,omitempty"`
	RemainingSeconds int64  `json:"remaining_seconds"`
}

// agentState mirrors what `tclaude session ls` shows: status from the
// hook callbacks (idle / working / awaiting_*), last hook timestamp,
// the agent's cwd, and subagent count. Empty string fields when no
// live session row exists for the conv.
type agentState struct {
	Status        string `json:"status,omitempty"`
	StatusDetail  string `json:"status_detail,omitempty"`
	SubagentCount int    `json:"subagent_count,omitempty"`
	LastHook      string `json:"last_hook,omitempty"`
	Cwd           string `json:"cwd,omitempty"`
}

// stateForConv looks up the most-recent live tmux session row for this
// conv-id and returns its hook-tracked state. When no tmux session is
// alive the agent has exited: the hook-recorded Status is frozen at
// whatever it was when the process died (usually "idle" from the final
// Stop hook, since no SessionEnd-style hook fires on exit), so we
// report StatusExited rather than passing the stale value through —
// otherwise a dead agent masquerades as "idle" on the dashboard.
// LastHook is preserved either way so the UI can show when the agent
// was last active.
func stateForConv(convID string) agentState {
	rows, err := db.FindSessionsByConvID(convID)
	if err != nil || len(rows) == 0 {
		return agentState{}
	}
	pick := rows[0] // already sorted most-recent first
	alive := false
	for _, r := range rows {
		if r.TmuxSession != "" && session.IsTmuxSessionAlive(r.TmuxSession) {
			pick = r
			alive = true
			break
		}
	}
	out := agentState{
		Status:        pick.Status,
		StatusDetail:  pick.StatusDetail,
		SubagentCount: pick.SubagentCount,
		Cwd:           pick.Cwd,
	}
	if !pick.LastHook.IsZero() {
		out.LastHook = pick.LastHook.Format(time.RFC3339)
	}
	// No live tmux session — the agent's process is gone. Report it as
	// exited rather than letting the frozen hook status (typically
	// "idle") masquerade as a running state. StatusDetail is cleared so
	// stale "idle: Bash"-style leftovers don't leak into the snapshot.
	if !alive {
		out.Status = session.StatusExited
		out.StatusDetail = ""
	}
	return out
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
		a := &dashboardAgent{
			ConvID: convID,
			Title:  agent.FreshTitle(convID),
			Branch: agent.FreshBranch(convID),
			Online: isConvOnline(convID),
			State:  stateForConv(convID),
			// init non-nil so JSON serializes [] not null;
			// the dashboard's JS does .length / .map without a guard.
			Groups:      []string{},
			OwnedGroups: []string{},
			Effective:   []string{},
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

	// Initialise slices empty (not nil) so JSON serializes [] instead
	// of null — the dashboard's JS does .length on members directly,
	// which would crash on null.
	out.Groups = []dashboardGroup{}
	out.Agents = []dashboardAgent{}
	for _, g := range groups {
		dg := dashboardGroup{Name: g.Name, Descr: g.Descr, DefaultCwd: g.DefaultCwd, Members: []dashboardMember{}}
		members, _ := db.ListAgentGroupMembers(g.ID)
		// Pre-load the owner set so we can tag members who are also
		// owners. Mirrors handleGroupMembersList in handlers.go.
		ownerSet := map[string]bool{}
		if owners, err := db.ListAgentGroupOwners(g.ID); err == nil {
			for _, o := range owners {
				ownerSet[o.ConvID] = true
			}
		}
		memberSet := map[string]bool{}
		for _, m := range members {
			memberSet[m.ConvID] = true
			online := isConvOnline(m.ConvID)
			dg.Members = append(dg.Members, dashboardMember{
				ConvID: m.ConvID,
				Title:  agent.FreshTitle(m.ConvID),
				Alias:  m.Alias,
				Role:   m.Role,
				Descr:  m.Descr,
				Branch: agent.FreshBranch(m.ConvID),
				Online: online,
				Owner:  ownerSet[m.ConvID],
				State:  stateForConv(m.ConvID),
			})
			if online {
				dg.Online++
			}
			a := addAgent(m.ConvID)
			a.Groups = append(a.Groups, g.Name)
			if ownerSet[m.ConvID] {
				a.OwnedGroups = append(a.OwnedGroups, g.Name)
			}
		}
		// Surface owners who aren't members so the list stays
		// comprehensive. Same shape as the CLI:
		// role="owner", no alias/descr.
		for ownerConv := range ownerSet {
			if memberSet[ownerConv] {
				continue
			}
			online := isConvOnline(ownerConv)
			dg.Members = append(dg.Members, dashboardMember{
				ConvID: ownerConv,
				Title:  agent.FreshTitle(ownerConv),
				Role:   "owner",
				Branch: agent.FreshBranch(ownerConv),
				Online: online,
				Owner:  true,
				State:  stateForConv(ownerConv),
			})
			// Pure-owners are reachable via this group too — surface
			// the group on the agent's row in the Agents view so
			// "what groups can this conv see?" matches reality.
			a := addAgent(ownerConv)
			a.Groups = append(a.Groups, g.Name)
			a.OwnedGroups = append(a.OwnedGroups, g.Name)
		}
		out.Groups = append(out.Groups, dg)
	}
	for convID, slugs := range allGrants {
		addAgent(convID)
		copySlice := append([]string{}, slugs...)
		sort.Strings(copySlice)
		out.Permissions.Grants[convID] = copySlice
	}

	// Online conv-sessions that aren't already known via group
	// membership or explicit grants. These become candidates for the
	// dashboard's ungrouped virtual group and the `+ add member`
	// overlay. Filtered to live tmux sessions so we don't surface
	// stale rows from past runs (a non-empty Cwd + alive tmux is the
	// signal that the conv is currently "running").
	if sessions, err := db.ListSessions(); err == nil {
		seenLoose := map[string]bool{}
		for _, s := range sessions {
			if s.ConvID == "" {
				continue
			}
			if seenLoose[s.ConvID] {
				continue
			}
			if _, alreadyKnown := agentRows[s.ConvID]; alreadyKnown {
				continue
			}
			if !isConvOnline(s.ConvID) {
				continue
			}
			addAgent(s.ConvID)
			seenLoose[s.ConvID] = true
		}
	}

	// Active sudo grants across every agent. One DB scan, then we
	// bucket per conv-id so the per-agent Active rendering is O(1)
	// inside the agent loop. The same rows feed the top-level Sudo[]
	// for the dedicated tab.
	sudoByConv := map[string][]dashboardSudoEntry{}
	out.Sudo = []dashboardSudoEntry{}
	if grants, err := db.ListAllActiveSudoGrants(); err == nil {
		now := time.Now()
		for _, g := range grants {
			title := ""
			if row := agent.FreshConvRowResolved(g.ConvID); row != nil {
				title = agent.DisplayTitle(row)
			}
			remaining := int64(0)
			if rem := g.ExpiresAt.Sub(now); rem > 0 {
				remaining = int64(rem.Seconds())
			}
			topEntry := dashboardSudoEntry{
				ID:               g.ID,
				ConvID:           g.ConvID,
				ConvTitle:        title,
				Slug:             g.Slug,
				GrantedAt:        g.GrantedAt.Format(time.RFC3339Nano),
				ExpiresAt:        g.ExpiresAt.Format(time.RFC3339Nano),
				GrantedBy:        g.GrantedBy,
				Reason:           g.Reason,
				RemainingSeconds: remaining,
			}
			out.Sudo = append(out.Sudo, topEntry)
			// On the per-agent slice we omit ConvID — the agent row's
			// own ConvID already identifies who holds the grant. Saves
			// bytes on agents with many grants and keeps the JSON
			// readable in browser devtools.
			rowEntry := topEntry
			rowEntry.ConvID = ""
			sudoByConv[g.ConvID] = append(sudoByConv[g.ConvID], rowEntry)
		}
	}

	out.Ungrouped = []dashboardAgent{}
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
		sort.Strings(a.OwnedGroups)
		if rows := sudoByConv[a.ConvID]; len(rows) > 0 {
			a.ActiveSudo = rows
		}
		out.Agents = append(out.Agents, *a)
		// An agent with no group memberships is "ungrouped" — surfaces
		// in the dedicated array so the dashboard can list them as
		// drag/add sources without re-deriving the membership state.
		// Effective perms still come from the broader Agents row, so
		// the dashboard uses Ungrouped purely as a candidate-set hint.
		if len(a.Groups) == 0 {
			out.Ungrouped = append(out.Ungrouped, *a)
		}
	}
	sort.Slice(out.Agents, func(i, j int) bool {
		return out.Agents[i].Title < out.Agents[j].Title
	})
	sort.Slice(out.Ungrouped, func(i, j int) bool {
		return out.Ungrouped[i].Title < out.Ungrouped[j].Title
	})

	out.Cron = collectCronSnapshot()
	out.Links = collectLinksSnapshot()

	writeJSON(w, http.StatusOK, out)
}

// collectLinksSnapshot enumerates every inter-group link, resolved to
// group names. One DB hit for the group list (via loadGroupNames),
// one for the link list; per-row lookups are map indexed. Returns an
// empty slice (not nil) so JS can safely call .map() / .length
// without a guard.
func collectLinksSnapshot() []dashboardLink {
	out := []dashboardLink{}
	rows, err := db.ListAllAgentGroupLinks()
	if err != nil {
		return out
	}
	names := loadGroupNames()
	for _, l := range rows {
		out = append(out, dashboardLink{
			ID:        l.ID,
			From:      names[l.FromGroupID],
			To:        names[l.ToGroupID],
			Mode:      l.Mode,
			CreatedAt: l.CreatedAt.Format(time.RFC3339),
		})
	}
	return out
}

// collectCronSnapshot builds the wire-shape rows for the dashboard's
// Cron tab. Resolves owner/target conv-ids to display titles and
// computes the next-due timestamp so the UI doesn't need a clock-
// arithmetic helper. Returns an empty slice (not nil) so the page's
// .map() doesn't crash on null.
func collectCronSnapshot() []dashboardCronJob {
	out := []dashboardCronJob{}
	jobs, err := db.ListAgentCronJobs()
	if err != nil {
		return out
	}
	// Cache group-id → name across rows in the same snapshot to avoid
	// a per-row lookup.
	groupNames := map[int64]string{}
	for _, j := range jobs {
		row := dashboardCronJob{
			ID:              j.ID,
			Name:            j.Name,
			OwnerConv:       j.OwnerConv,
			OwnerLabel:      labelForConv(j.OwnerConv),
			TargetConv:      j.TargetConv,
			GroupID:         j.GroupID,
			IntervalSeconds: j.IntervalSeconds,
			Subject:         j.Subject,
			Body:            j.Body,
			Enabled:         j.Enabled,
			LastRunStatus:   j.LastRunStatus,
		}
		if j.TargetConv != "" {
			row.TargetLabel = labelForConv(j.TargetConv)
		}
		if j.GroupID > 0 {
			name, ok := groupNames[j.GroupID]
			if !ok {
				if g, gerr := db.GetAgentGroupByID(j.GroupID); gerr == nil && g != nil {
					name = g.Name
				}
				groupNames[j.GroupID] = name
			}
			row.GroupName = name
		}
		if !j.CreatedAt.IsZero() {
			row.CreatedAt = j.CreatedAt.Format(time.RFC3339)
		}
		if !j.LastRunAt.IsZero() {
			row.LastRunAt = j.LastRunAt.Format(time.RFC3339)
			next := j.LastRunAt.Add(time.Duration(j.IntervalSeconds) * time.Second)
			row.NextDueAt = next.Format(time.RFC3339)
		}
		out = append(out, row)
	}
	return out
}

// labelForConv returns a short display label for a conv-id. Tries
// the conv's display title (custom-title / summary / first-prompt)
// first, then falls back to the 8-char prefix. Mirrors the rendering
// used in the Groups/Agents tabs.
func labelForConv(convID string) string {
	if convID == "" {
		return ""
	}
	row := agent.FreshConvRowResolved(convID)
	if row != nil {
		if t := agent.DisplayTitle(row); t != "" {
			return t
		}
	}
	if len(convID) >= 8 {
		return convID[:8]
	}
	return convID
}
