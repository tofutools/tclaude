package agentd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// /v1/sudo — time-bounded permission elevations. Modeled on Unix
// `sudo` and GCP PAM: an agent requests a bundle of permission slugs
// for a bounded duration, the request always pops a human-approval
// popup (no silent path), and on approve the slugs join the agent's
// effective permission set until expires_at.
//
// Endpoints:
//
//	POST   /v1/sudo                    request (popup-gated)
//	GET    /v1/sudo                    list active for caller
//	GET    /v1/sudo?all=1              list active across all (human-only)
//	DELETE /v1/sudo/{id}               revoke one (human-only)
//	DELETE /v1/sudo?conv=<selector>    revoke all for one conv (human-only)
//	DELETE /v1/sudo?all=1              revoke every active grant (human-only)
//
// Defaults below are the hardcoded fallbacks. The active values used
// at request time come from resolveSudoConfig — config.json's
// agent.sudo block and any matching agent.sudo.overrides[<key>]
// overlay these defaults per caller. Tests override the popup
// decision via the existing StubApprovalForTest indirection and the
// per-test $HOME tmpdir lets them write a config.json that
// resolveSudoConfig will pick up.

// sudoDefaultMaxDuration is the upper bound on a single sudo grant
// when no config or override sets one. Requests exceeding the
// resolved max return 400 before the popup even opens — guards
// against an agent asking for "30 days" and the human approving
// without noticing.
const sudoDefaultMaxDuration = 1 * time.Hour

// sudoDefaultDefaultDuration is what an unspecified --duration
// resolves to. The popup payload always shows the resolved expires_at
// so the human sees the actual window.
const sudoDefaultDefaultDuration = 5 * time.Minute

// sudoDefaultPopupTimeout is how long the request blocks waiting for
// the human's decision. Timeout is treated as deny: a doomed agent
// never gets stuck waiting forever.
const sudoDefaultPopupTimeout = 60 * time.Second

// sudoDefaultBlocklist names slugs that can NEVER be sudo-elevated.
// Each listed slug enables permanent privilege escalation: an agent
// holding `permissions.grant` could grant itself anything during the
// sudo window and the grant would outlive the elevation. Block at
// the request-validation layer (no popup) so a misclick or runaway
// loop can't even surface them to the human.
//
// Group ownership (`groups.own`) is intentionally NOT blocklisted —
// it spreads power but the time-bound + popup audit make it
// recoverable. Forbid only the truly recursive escalation.
//
// Config can replace this list via agent.sudo.blocklist. An explicit
// empty list (`"blocklist": []`) is honored — the human opts out of
// the safety net knowingly.
var sudoDefaultBlocklist = []string{
	PermPermissionsGrant,
	PermPermissionsRevoke,
}

// resolvedSudo bundles the active sudo policy for a single caller
// after the global config + per-conv override are layered onto the
// hardcoded defaults. Built fresh per request via resolveSudoConfig
// so a config edit lands without restarting the daemon.
type resolvedSudo struct {
	MaxDuration     time.Duration
	DefaultDuration time.Duration
	PopupTimeout    time.Duration
	Blocklist       []string
}

// resolveSudoConfig builds the effective sudo policy for a caller
// (convID / title). Order of precedence: per-conv override
// (Sudo.Overrides[matching key]) → global Sudo block → hardcoded
// fallbacks. Each layer fills in only the fields it sets — unset
// fields fall through.
//
// Bad duration strings in the config are tolerated: a
// time.ParseDuration error preserves the previous layer's value and
// logs nothing (the human edited the file; surface the error in CI
// or via a dedicated config-lint subcommand later if it becomes a
// support burden).
func resolveSudoConfig(cfg *config.Config, convID, title string) resolvedSudo {
	out := resolvedSudo{
		MaxDuration:     sudoDefaultMaxDuration,
		DefaultDuration: sudoDefaultDefaultDuration,
		PopupTimeout:    sudoDefaultPopupTimeout,
		Blocklist:       append([]string(nil), sudoDefaultBlocklist...),
	}
	if cfg == nil || cfg.Agent == nil || cfg.Agent.Sudo == nil {
		return out
	}
	applySudoLayer(&out, cfg.Agent.Sudo.MaxDuration, cfg.Agent.Sudo.DefaultDuration,
		cfg.Agent.Sudo.PopupTimeout, cfg.Agent.Sudo.Blocklist)
	if ov := cfg.MatchSudoOverride(convID, title); ov != nil {
		applySudoLayer(&out, ov.MaxDuration, ov.DefaultDuration, ov.PopupTimeout, ov.Blocklist)
	}
	return out
}

func applySudoLayer(dst *resolvedSudo, maxStr, defaultStr, popupStr string, blocklist *[]string) {
	if d, ok := parseDurationOpt(maxStr); ok {
		dst.MaxDuration = d
	}
	if d, ok := parseDurationOpt(defaultStr); ok {
		dst.DefaultDuration = d
	}
	if d, ok := parseDurationOpt(popupStr); ok {
		dst.PopupTimeout = d
	}
	if blocklist != nil {
		// Replace, not merge: an empty list means "explicitly no
		// blocklist" (human opts out of the safety net).
		dst.Blocklist = append([]string(nil), (*blocklist)...)
	}
}

func parseDurationOpt(s string) (time.Duration, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, false
	}
	return d, true
}

func handleSudo(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		handleSudoRequest(w, r)
	case http.MethodGet:
		handleSudoList(w, r)
	case http.MethodDelete:
		// DELETE /v1/sudo (no id) — bulk revoke. /v1/sudo/{id} is the
		// per-row revoke; that goes through handleSudoByID below.
		handleSudoRevokeBulk(w, r)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method", "GET / POST / DELETE only")
	}
}

func handleSudoByID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, "method", "DELETE only")
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/v1/sudo/")
	if rest == "" || strings.Contains(rest, "/") {
		writeError(w, http.StatusBadRequest, "invalid_arg", "expected /v1/sudo/{id}")
		return
	}
	id, err := strconv.ParseInt(rest, 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "id must be a positive integer")
		return
	}
	// Revoke is human-only: agents can't take elevations away from
	// each other (would defeat the audit-log promise of "human
	// approved + scoped").
	if !requireHuman(w, r, "revoke sudo grants") {
		return
	}
	n, err := db.RevokeSudoGrant(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	if n == 0 {
		writeError(w, http.StatusNotFound, "not_found",
			fmt.Sprintf("no active grant with id %d", id))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"revoked": n, "id": id})
}

type sudoRequestBody struct {
	Slugs    []string `json:"slugs"`
	Duration string   `json:"duration"`
	Reason   string   `json:"reason"`
	// Target switches the endpoint into proactive-grant mode: the
	// human caller seeds a time-bounded grant for a specific conv
	// without involving the popup. Agents are not allowed to set
	// this — manager-pattern approval ("agent grants sudo to a peer")
	// is intentionally deferred. Empty/unset → original
	// agent-initiated, popup-gated behaviour.
	Target string `json:"target,omitempty"`
}

type sudoGrantJSON struct {
	ID        int64  `json:"id"`
	AgentID   string `json:"agent_id,omitempty"`
	ConvID    string `json:"conv_id"`
	ConvTitle string `json:"conv_title,omitempty"`
	Slug      string `json:"slug"`
	GrantedAt string `json:"granted_at"`
	ExpiresAt string `json:"expires_at"`
	GrantedBy string `json:"granted_by"`
	Reason    string `json:"reason,omitempty"`
	RemainingSeconds int64 `json:"remaining_seconds,omitempty"`
}

func handleSudoRequest(w http.ResponseWriter, r *http.Request) {
	p := peerFromContext(r.Context())
	var body sudoRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	body.Slugs = dedupeNonEmpty(body.Slugs)
	if len(body.Slugs) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_arg",
			"slugs[] is required (at least one slug to elevate)")
		return
	}

	// Two paths through this endpoint:
	//
	//   1. body.target SET — proactive grant. Human caller, no popup
	//      (peer-cred is the consent layer). The CLI's
	//      `tclaude agent sudo request --target <conv> ...` lands here.
	//   2. body.target UNSET — agent-initiated. Popup-gated. The agent's
	//      conv is the grant target.
	//
	// Agents calling with target set is rejected — manager-pattern
	// approval (agent grants sudo to a peer) is intentionally deferred;
	// when the use case shows up it ships behind a `sudo.approve` slug.
	if strings.TrimSpace(body.Target) != "" {
		handleSudoProactiveGrant(w, p, body, "<human-cli>:proactive")
		return
	}

	// Original agent-initiated, popup-gated path — confirmed agent only.
	switch classify(p) {
	case classAgent:
		// proceed below
	case classHuman:
		writeError(w, http.StatusBadRequest, "invalid_arg",
			"sudo is only meaningful for agent callers without --target; humans seed proactive grants by setting body.target = <conv selector>")
		return
	case classUnidentified:
		writeUnidentified(w)
		return
	case classAgentUnknown:
		writeAgentUnknown(w)
		return
	case classUnconfirmed:
		writeUnconfirmed(w)
		return
	}

	// Resolve caller title up-front: needed for both the per-conv
	// override lookup (Sudo.Overrides[<key>]) and the popup payload
	// (a familiar name helps the human recognise who's asking).
	title := ""
	if row := agent.FreshConvRow(p.ConvID); row != nil {
		title = agent.DisplayTitle(row)
	}
	cfg, _ := config.Load()
	policy := resolveSudoConfig(cfg, p.ConvID, title)

	if blocked := blockedSlugs(body.Slugs, policy.Blocklist); len(blocked) > 0 {
		writeError(w, http.StatusForbidden, "blocked",
			fmt.Sprintf("slug(s) blocklisted from sudo (would enable permanent escalation): %s",
				strings.Join(blocked, ", ")))
		return
	}
	dur, ok := resolveSudoDuration(w, body.Duration, policy)
	if !ok {
		return
	}
	reason := strings.TrimSpace(body.Reason)

	// Build the popup payload. The body preview surfaces the slug
	// list + reason + resolved expiry so the human sees exactly what
	// they're approving (vs the per-call escape hatch's request body).
	now := time.Now()
	expires := now.Add(dur)
	preview := buildSudoApprovalPreview(body.Slugs, dur, expires, reason)
	req := &approvalRequest{
		id:          newApprovalID(),
		perm:        "sudo." + strings.Join(body.Slugs, ","),
		convID:      p.ConvID,
		convTitle:   title,
		method:      r.Method,
		path:        r.URL.Path,
		bodyPreview: preview,
		createdAt:   now,
		timeout:     policy.PopupTimeout,
		decision:    make(chan bool, 1),
		extend:      make(chan time.Duration, 1),
	}
	if popupBaseURL == "" {
		writeError(w, http.StatusServiceUnavailable, "no_popup",
			"no popup base URL configured; sudo cannot be approved without a popup")
		return
	}

	approved := requestHumanApproval(req, popupBaseURL)
	if !approved {
		writeError(w, http.StatusForbidden, "denied",
			"sudo request denied by human (or popup timed out)")
		return
	}

	// Re-snapshot the timestamps post-popup so the window is measured
	// from approval time, not request time — fairer to the agent if
	// the human took 30 seconds to click.
	out, status := insertSudoBundle(p.ConvID, title, body.Slugs, dur, reason,
		"human:popup-id="+req.id)
	writeJSON(w, status, out)
}

// handleSudoProactiveGrant runs the human-initiated grant path:
// validate body.Target, apply the same blocklist + duration cap as
// the agent-initiated path, insert grant rows, return the standard
// /v1/sudo response shape. granter labels the audit trail (CLI vs
// dashboard, both proactive).
//
// Caller is required to be human — agents reaching here mean the
// caller set body.Target, which is reserved for humans (see
// handleSudoRequest's dispatch). Returning 403 keeps the
// manager-pattern-approval gap explicit.
func handleSudoProactiveGrant(w http.ResponseWriter, p *peer, body sudoRequestBody, granter string) {
	if classify(p) != classHuman {
		writeError(w, http.StatusForbidden, "auth",
			"only the human operator may proactively grant sudo to other convs (manager-pattern approval is deferred; seed proactive grants from the dashboard, or `tclaude agent sudo request --target <conv>` with the operator token set)")
		return
	}
	res, _, err := agent.ResolveSelector(strings.TrimSpace(body.Target))
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found",
			"resolve target: "+err.Error())
		return
	}
	targetConv := res.ConvID
	title := ""
	if row := agent.FreshConvRowResolved(targetConv); row != nil {
		title = agent.DisplayTitle(row)
	}
	cfg, _ := config.Load()
	policy := resolveSudoConfig(cfg, targetConv, title)

	if blocked := blockedSlugs(body.Slugs, policy.Blocklist); len(blocked) > 0 {
		writeError(w, http.StatusForbidden, "blocked",
			fmt.Sprintf("slug(s) blocklisted from sudo (would enable permanent escalation): %s",
				strings.Join(blocked, ", ")))
		return
	}
	dur, ok := resolveSudoDuration(w, body.Duration, policy)
	if !ok {
		return
	}
	out, status := insertSudoBundle(targetConv, title, body.Slugs, dur, strings.TrimSpace(body.Reason), granter)
	writeJSON(w, status, out)
}

// blockedSlugs returns every entry of want that's also in blocklist,
// preserving order. Used by both the agent-initiated and proactive
// paths so the error message wording stays consistent.
func blockedSlugs(want, blocklist []string) []string {
	out := []string{}
	for _, slug := range want {
		for _, b := range blocklist {
			if slug == b {
				out = append(out, slug)
				break
			}
		}
	}
	return out
}

// resolveSudoDuration parses body.Duration against the policy. On any
// validation failure it writes the error response and returns
// (0, false); callers just return.
func resolveSudoDuration(w http.ResponseWriter, raw string, policy resolvedSudo) (time.Duration, bool) {
	dur := policy.DefaultDuration
	if strings.TrimSpace(raw) != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg",
				"duration: "+err.Error())
			return 0, false
		}
		if d <= 0 {
			writeError(w, http.StatusBadRequest, "invalid_arg",
				"duration must be positive")
			return 0, false
		}
		dur = d
	}
	if dur > policy.MaxDuration {
		writeError(w, http.StatusBadRequest, "invalid_arg",
			fmt.Sprintf("duration %s exceeds the max %s", dur, policy.MaxDuration))
		return 0, false
	}
	return dur, true
}

// sudoBundleResponse is the wire shape returned by both the
// agent-initiated and proactive grant paths. Mirrors what
// `tclaude agent sudo request` already parses on the client side.
type sudoBundleResponse struct {
	Grants    []sudoGrantJSON `json:"grants"`
	ExpiresAt string          `json:"expires_at"`
	ConvID    string          `json:"conv_id"`
}

// insertSudoBundle inserts one row per slug, sharing granted_at /
// expires_at so a single grant decision surfaces as a coherent bundle
// in audit views. Returns the response payload + HTTP status; per-row
// insert failures are surfaced in the response (non-fatal — sibling
// rows that landed are still valid).
func insertSudoBundle(convID, title string, slugs []string, dur time.Duration, reason, granter string) (sudoBundleResponse, int) {
	now := time.Now()
	expires := now.Add(dur)
	out := sudoBundleResponse{
		ExpiresAt: expires.Format(time.RFC3339Nano),
		ConvID:    convID,
	}
	for _, slug := range slugs {
		id, err := db.InsertSudoGrant(&db.SudoGrant{
			ConvID:    convID,
			Slug:      slug,
			GrantedAt: now,
			ExpiresAt: expires,
			GrantedBy: granter,
			Reason:    reason,
		})
		if err != nil {
			out.Grants = append(out.Grants, sudoGrantJSON{
				Slug:   slug,
				Reason: "insert failed: " + err.Error(),
			})
			continue
		}
		out.Grants = append(out.Grants, sudoGrantJSON{
			ID:               id,
			ConvID:           convID,
			ConvTitle:        title,
			Slug:             slug,
			GrantedAt:        now.Format(time.RFC3339Nano),
			ExpiresAt:        expires.Format(time.RFC3339Nano),
			GrantedBy:        granter,
			Reason:           reason,
			RemainingSeconds: int64(dur.Seconds()),
		})
	}
	return out, http.StatusOK
}

func handleSudoList(w http.ResponseWriter, r *http.Request) {
	all := r.URL.Query().Get("all") == "1" || r.URL.Query().Get("all") == "true"
	p := peerFromContext(r.Context())
	now := time.Now()

	if all {
		// Cross-conv listing is human-only: an agent shouldn't see
		// what permissions another agent currently holds.
		if !requireHuman(w, r, "list all sudo grants") {
			return
		}
		rows, err := db.ListAllActiveSudoGrants()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, sudoGrantsToJSON(rows, now))
		return
	}
	// Self-listing — agent path uses the caller's resolved conv-id;
	// human path returns an empty list (humans don't have sudo grants
	// — they hold every permission implicitly).
	switch classify(p) {
	case classHuman:
		writeJSON(w, http.StatusOK, []sudoGrantJSON{})
		return
	case classAgent:
		// proceed to self-listing below
	case classUnidentified:
		writeUnidentified(w)
		return
	case classAgentUnknown:
		writeAgentUnknown(w)
		return
	case classUnconfirmed:
		writeUnconfirmed(w)
		return
	}
	rows, err := db.ListActiveSudoGrants(p.ConvID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, sudoGrantsToJSON(rows, now))
}

func handleSudoRevokeBulk(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "revoke sudo grants") {
		return
	}
	q := r.URL.Query()
	if q.Get("all") == "1" || q.Get("all") == "true" {
		n, err := db.RevokeAllActiveSudoGrants()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"revoked": n, "scope": "all"})
		return
	}
	convSel := strings.TrimSpace(q.Get("conv"))
	if convSel == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg",
			"DELETE /v1/sudo requires ?conv=<selector> or ?all=1")
		return
	}
	if u, err := url.QueryUnescape(convSel); err == nil {
		convSel = u
	}
	res, _, err := agent.ResolveSelector(convSel)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found",
			"resolve conv: "+err.Error())
		return
	}
	n, err := db.RevokeSudoGrantsByConv(res.ConvID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"revoked": n, "conv_id": res.ConvID})
}

func sudoGrantsToJSON(rows []*db.SudoGrant, now time.Time) []sudoGrantJSON {
	out := make([]sudoGrantJSON, 0, len(rows))
	for _, g := range rows {
		title := ""
		if row := agent.FreshConvRowResolved(g.ConvID); row != nil {
			title = agent.DisplayTitle(row)
		}
		entry := sudoGrantJSON{
			ID:        g.ID,
			AgentID:   g.AgentID,
			ConvID:    g.ConvID,
			ConvTitle: title,
			Slug:      g.Slug,
			GrantedAt: g.GrantedAt.Format(time.RFC3339Nano),
			ExpiresAt: g.ExpiresAt.Format(time.RFC3339Nano),
			GrantedBy: g.GrantedBy,
			Reason:    g.Reason,
		}
		if remaining := g.ExpiresAt.Sub(now); remaining > 0 {
			entry.RemainingSeconds = int64(remaining.Seconds())
		}
		out = append(out, entry)
	}
	return out
}

func buildSudoApprovalPreview(slugs []string, dur time.Duration, expires time.Time, reason string) string {
	sort.Strings(slugs)
	var b strings.Builder
	b.WriteString("Sudo request:\n")
	b.WriteString("  slugs:\n")
	for _, s := range slugs {
		b.WriteString("    - ")
		b.WriteString(s)
		b.WriteString("\n")
	}
	b.WriteString("  duration: ")
	b.WriteString(dur.String())
	b.WriteString("\n  expires_at: ")
	b.WriteString(expires.Format(time.RFC3339))
	if reason != "" {
		b.WriteString("\n  reason: ")
		b.WriteString(reason)
	}
	return b.String()
}

func dedupeNonEmpty(xs []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		x = strings.TrimSpace(x)
		if x == "" || seen[x] {
			continue
		}
		seen[x] = true
		out = append(out, x)
	}
	return out
}
