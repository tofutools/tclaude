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
// Defaults are hardcoded for v1 (config-driven overrides ladder up
// later — `agent.sudo.{max_duration, default_duration, blocklist}`
// in `~/.tclaude/config.json`). Tests can override the popup decision
// via the existing RequestHumanApprovalImpl indirection.

// sudoMaxDuration is the upper bound on a single sudo grant. Requests
// exceeding it return 400 before the popup even opens — guards against
// an agent asking for "30 days" and the human approving without
// noticing.
const sudoMaxDuration = 1 * time.Hour

// sudoDefaultDuration is what the CLI emits when --duration is
// omitted. The popup payload always shows the resolved expires_at so
// the human sees the actual window.
const sudoDefaultDuration = 5 * time.Minute

// sudoPopupTimeout is how long the request blocks waiting for the
// human's decision. Timeout is treated as deny: a doomed agent never
// gets stuck waiting forever.
const sudoPopupTimeout = 60 * time.Second

// sudoBlocklist names slugs that can NEVER be sudo-elevated. Each
// listed slug enables permanent privilege escalation: an agent
// holding `permissions.grant` could grant itself anything during the
// sudo window and the grant would outlive the elevation. Block at
// the request-validation layer (no popup) so a misclick or runaway
// loop can't even surface them to the human.
//
// Group ownership (`groups.own`) is intentionally NOT blocklisted —
// it spreads power but the time-bound + popup audit make it
// recoverable. Forbid only the truly recursive escalation.
var sudoBlocklist = []string{
	PermPermissionsGrant,
	PermPermissionsRevoke,
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
	// approved + scoped"). Mirror requireHuman from the head-alias
	// surface.
	p := peerFromContext(r.Context())
	if p.HasClaudeAncestor {
		writeError(w, http.StatusForbidden, "auth",
			"only humans may revoke sudo grants (no agent path)")
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
}

type sudoGrantJSON struct {
	ID        int64  `json:"id"`
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
	// Caller MUST be an agent. Humans bypass requirePermission already;
	// they don't need sudo. Reject the human path explicitly so a
	// stray CLI call doesn't insert ghost rows.
	p := peerFromContext(r.Context())
	if !p.HasClaudeAncestor {
		writeError(w, http.StatusBadRequest, "invalid_arg",
			"sudo is only meaningful for agent callers; humans hold every permission implicitly")
		return
	}
	if p.ConvID == "" {
		writeError(w, http.StatusForbidden, "auth",
			"caller has a Claude Code ancestor but no resolvable conv-id; cannot evaluate sudo request")
		return
	}

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
	// Blocklist check: refuse permanent-escalation slugs without
	// popping the popup, so a runaway loop can't even surface them
	// to the human. Reports every blocklisted slug at once so the
	// caller can fix its bundle in a single retry.
	blocked := []string{}
	for _, slug := range body.Slugs {
		for _, b := range sudoBlocklist {
			if slug == b {
				blocked = append(blocked, slug)
				break
			}
		}
	}
	if len(blocked) > 0 {
		writeError(w, http.StatusForbidden, "blocked",
			fmt.Sprintf("slug(s) blocklisted from sudo (would enable permanent escalation): %s",
				strings.Join(blocked, ", ")))
		return
	}
	// Duration: parse + cap. Empty defaults to sudoDefaultDuration.
	dur := sudoDefaultDuration
	if strings.TrimSpace(body.Duration) != "" {
		d, err := time.ParseDuration(body.Duration)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg",
				"duration: "+err.Error())
			return
		}
		if d <= 0 {
			writeError(w, http.StatusBadRequest, "invalid_arg",
				"duration must be positive")
			return
		}
		dur = d
	}
	if dur > sudoMaxDuration {
		writeError(w, http.StatusBadRequest, "invalid_arg",
			fmt.Sprintf("duration %s exceeds the max %s", dur, sudoMaxDuration))
		return
	}
	reason := strings.TrimSpace(body.Reason)

	// Resolve caller title for the popup payload — a familiar name
	// helps the human recognise who's asking.
	title := ""
	if row := agent.FreshConvRow(p.ConvID); row != nil {
		title = agent.DisplayTitle(row)
	}

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
		timeout:     sudoPopupTimeout,
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

	// Insert one row per slug, sharing granted_at / expires_at so a
	// single popup approval surfaces as a coherent bundle in the
	// audit view. Re-snapshot the timestamps post-popup so the window
	// is measured from approval time, not request time — fairer to
	// the agent if the human took 30 seconds to click.
	postNow := time.Now()
	postExpires := postNow.Add(dur)
	granter := "human:popup-id=" + req.id
	out := struct {
		Grants    []sudoGrantJSON `json:"grants"`
		ExpiresAt string          `json:"expires_at"`
		ConvID    string          `json:"conv_id"`
	}{
		ExpiresAt: postExpires.Format(time.RFC3339Nano),
		ConvID:    p.ConvID,
	}
	for _, slug := range body.Slugs {
		id, err := db.InsertSudoGrant(&db.SudoGrant{
			ConvID:    p.ConvID,
			Slug:      slug,
			GrantedAt: postNow,
			ExpiresAt: postExpires,
			GrantedBy: granter,
			Reason:    reason,
		})
		if err != nil {
			// Best-effort per slug. The popup approval is single-shot;
			// if a row insert fails, log + omit it from the response so
			// the agent can see exactly what landed. Don't 500: the
			// other slugs already inserted are still valid.
			out.Grants = append(out.Grants, sudoGrantJSON{
				Slug:   slug,
				Reason: "insert failed: " + err.Error(),
			})
			continue
		}
		out.Grants = append(out.Grants, sudoGrantJSON{
			ID:               id,
			ConvID:           p.ConvID,
			ConvTitle:        title,
			Slug:             slug,
			GrantedAt:        postNow.Format(time.RFC3339Nano),
			ExpiresAt:        postExpires.Format(time.RFC3339Nano),
			GrantedBy:        granter,
			Reason:           reason,
			RemainingSeconds: int64(dur.Seconds()),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func handleSudoList(w http.ResponseWriter, r *http.Request) {
	all := r.URL.Query().Get("all") == "1" || r.URL.Query().Get("all") == "true"
	p := peerFromContext(r.Context())
	now := time.Now()

	if all {
		// Cross-conv listing is human-only: an agent shouldn't see
		// what permissions another agent currently holds.
		if p.HasClaudeAncestor {
			writeError(w, http.StatusForbidden, "auth",
				"sudo ls --all is human-only")
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
	if !p.HasClaudeAncestor {
		writeJSON(w, http.StatusOK, []sudoGrantJSON{})
		return
	}
	if p.ConvID == "" {
		writeError(w, http.StatusForbidden, "auth",
			"caller has a Claude Code ancestor but no resolvable conv-id")
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
	p := peerFromContext(r.Context())
	if p.HasClaudeAncestor {
		writeError(w, http.StatusForbidden, "auth",
			"only humans may revoke sudo grants (no agent path)")
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
