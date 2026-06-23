package agentd

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// audit.go records the persistent trail of daemon-proxied tclaude
// commands (JOH-268). agentd already funnels every `tclaude agent` (CLI)
// and dashboard action through one of two muxes; the auditRequests
// middleware wraps both and writes a symbolic audit_log row for each
// command — WHO ran WHAT against WHICH target, plus the raw method/path/
// status for the generic case.
//
// Scope (v1): only daemon-proxied commands are captured (the operator
// chose this; direct non-daemon CLI invocations are a later expansion).
// Capture is an allowlist — a request is audited iff its (method, path)
// matches an entry in auditRoutes. That keeps the dashboard's many
// cosmetic/poll endpoints (snapshot, prefs, slop, …) out of the trail
// while still covering every real coordination command.
//
// All outcomes are recorded, not just successes: a 403 (permission
// denied) or a 4xx/5xx error produces a row too, so the trail answers
// "who *tried* to do what", not only "what landed". The UI filters on
// the status field.

// auditFields are the symbolic columns a route's describer fills in. The
// middleware seeds Verb / GroupName / target from the path captures, then
// the describer (if any) refines them from the request body.
type auditFields struct {
	Verb        string
	TargetConv  string
	TargetLabel string
	GroupName   string
	Detail      string
}

// auditCtx is handed to a describer: the path captures, the buffered
// request body, and the fields-in-progress to mutate.
type auditCtx struct {
	vars   map[string]string
	body   []byte
	fields *auditFields
}

// describer refines auditFields from the request body. nil means the
// path-derived defaults (verb + group + target selector) are enough.
type describer func(c *auditCtx)

// auditRoute is one allowlist entry. segs is the canonical path (the
// /v1 or /api prefix already stripped, and the two dashboard spellings
// normalised — see canonicalizeAuditSegs). A token of the form {x}
// captures that segment into vars["x"]; any other token is a literal.
// method "" matches any mutating method.
type auditRoute struct {
	method   string
	segs     []string
	verb     string // fixed verb; "" → use the {verb} capture
	describe describer
}

// auditRoutes is the allowlist of daemon-proxied commands. Canonical
// (prefix-stripped) segments, so one entry serves both the CLI (/v1) and
// dashboard (/api) surfaces. New commands that should appear in the trail
// add an entry here; a command with no entry is simply not audited.
var auditRoutes = []auditRoute{
	// Messaging.
	{method: http.MethodPost, segs: []string{"messages"}, verb: "message", describe: describeMessage},

	// Per-agent lifecycle verbs. The CLI is /v1/agent/{sel}/{verb}; the
	// dashboard is /api/agents/{conv}/{verb} (canonicalised agents→agent).
	{segs: []string{"agent", "{conv}", "{verb}"}, describe: describeAgentVerb},
	// Dashboard agent edit/delete with no verb segment.
	{method: http.MethodDelete, segs: []string{"agent", "{conv}"}, verb: "delete", describe: describeAgentTarget},
	{method: http.MethodPatch, segs: []string{"agent", "{conv}"}, verb: "edit", describe: describeAgentTarget},

	// Spawn.
	{method: http.MethodPost, segs: []string{"groups", "{name}", "spawn"}, verb: "spawn", describe: describeSpawn},

	// Group lifecycle.
	{method: http.MethodPost, segs: []string{"groups"}, verb: "group.create", describe: describeGroupCreate},
	{method: http.MethodPost, segs: []string{"groups", "import"}, verb: "group.import"},
	{method: http.MethodDelete, segs: []string{"groups", "{name}"}, verb: "group.delete"},
	{method: http.MethodPatch, segs: []string{"groups", "{name}"}, verb: "group.update"},
	{method: http.MethodPost, segs: []string{"groups", "{name}", "rename"}, verb: "group.rename", describe: describeNewName},
	{method: http.MethodPost, segs: []string{"groups", "{name}", "clone"}, verb: "group.clone"},
	{method: http.MethodPost, segs: []string{"groups", "{name}", "archive"}, verb: "group.archive"},
	{method: http.MethodPost, segs: []string{"groups", "{name}", "unarchive"}, verb: "group.unarchive"},
	{method: http.MethodPost, segs: []string{"groups", "{name}", "retire"}, verb: "group.retire"},
	{method: http.MethodPost, segs: []string{"groups", "{name}", "stop"}, verb: "group.stop"},
	{method: http.MethodPost, segs: []string{"groups", "{name}", "resume"}, verb: "group.resume"},

	// Membership + ownership.
	{method: http.MethodPost, segs: []string{"groups", "{name}", "members"}, verb: "member.add", describe: describeMemberTarget},
	{method: http.MethodDelete, segs: []string{"groups", "{name}", "members", "{conv}"}, verb: "member.remove", describe: describeAgentTarget},
	{method: http.MethodPatch, segs: []string{"groups", "{name}", "members", "{conv}"}, verb: "member.update", describe: describeAgentTarget},
	{method: http.MethodPost, segs: []string{"groups", "{name}", "owners"}, verb: "owner.add", describe: describeMemberTarget},
	{method: http.MethodDelete, segs: []string{"groups", "{name}", "owners", "{conv}"}, verb: "owner.remove", describe: describeAgentTarget},

	// Inter-group links.
	{method: http.MethodPost, segs: []string{"groups", "{name}", "links"}, verb: "link.add"},
	{method: http.MethodPatch, segs: []string{"groups", "{name}", "links", "{id}"}, verb: "link.update"},
	{method: http.MethodDelete, segs: []string{"groups", "{name}", "links", "{id}"}, verb: "link.delete"},

	// Cron.
	{method: http.MethodPost, segs: []string{"cron"}, verb: "cron.add", describe: describeCron},
	{method: http.MethodPatch, segs: []string{"cron", "{id}"}, verb: "cron.update"},
	{method: http.MethodDelete, segs: []string{"cron", "{id}"}, verb: "cron.delete"},

	// Permissions.
	{method: http.MethodPost, segs: []string{"permissions", "grant"}, verb: "permissions.grant", describe: describePerm},
	{method: http.MethodPost, segs: []string{"permissions", "deny"}, verb: "permissions.deny", describe: describePerm},
	{method: http.MethodPost, segs: []string{"permissions", "revoke"}, verb: "permissions.revoke", describe: describePerm},
	{method: http.MethodPost, segs: []string{"permissions"}, verb: "permissions.set", describe: describePermSet},

	// Sudo (time-bounded elevation).
	{method: http.MethodPost, segs: []string{"sudo"}, verb: "sudo.grant", describe: describeSudo},
	{method: http.MethodDelete, segs: []string{"sudo", "{id}"}, verb: "sudo.revoke"},

	// Human notification channel.
	{method: http.MethodPost, segs: []string{"notify-human"}, verb: "notify-human", describe: describeNotifyHuman},
}

// auditRequests wraps a mux so every matched command writes an audit
// row. It buffers the request body (restoring it for the handler) only
// for matched routes, runs the handler, then records the row with the
// resulting status — after the response is flushed, so it adds no
// user-visible latency and cannot affect the response.
func auditRequests(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		route, vars, source, ok := matchAuditRoute(r.Method, r.URL.Path)
		var body []byte
		if ok {
			body = bufferAuditBody(r)
		}
		rec := &statusRec{ResponseWriter: w, code: 200}
		h.ServeHTTP(rec, r)
		if ok {
			recordAuditRow(r, route, vars, body, rec.code, source)
		}
	})
}

// isMutatingMethod reports whether a method is a state-changing verb we
// audit. Reads (GET/HEAD/OPTIONS) are never audited.
func isMutatingMethod(m string) bool {
	switch m {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

// matchAuditRoute resolves (method, path) to an audit route. It strips
// the surface prefix (/v1 → cli, /api → dashboard), normalises the two
// divergent dashboard spellings, then matches the canonical segments
// against auditRoutes. ok=false means "not audited".
func matchAuditRoute(method, path string) (route *auditRoute, vars map[string]string, source string, ok bool) {
	if !isMutatingMethod(method) {
		return nil, nil, "", false
	}
	segs := splitPathSegments(path)
	if len(segs) < 2 {
		return nil, nil, "", false
	}
	switch segs[0] {
	case "v1":
		source = db.AuditSourceCLI
	case "api":
		source = db.AuditSourceDashboard
	default:
		return nil, nil, "", false
	}
	rest := canonicalizeAuditSegs(segs[1:])
	for i := range auditRoutes {
		rt := &auditRoutes[i]
		if rt.method != "" && rt.method != method {
			continue
		}
		if v, matched := matchSegs(rt.segs, rest); matched {
			return rt, v, source, true
		}
	}
	return nil, nil, "", false
}

// canonicalizeAuditSegs folds the two dashboard route spellings onto
// their CLI equivalents so a single auditRoutes table serves both
// surfaces: /api/agents/… → agent/…, and /api/message → messages.
func canonicalizeAuditSegs(rest []string) []string {
	if len(rest) == 0 {
		return rest
	}
	out := append([]string(nil), rest...)
	switch out[0] {
	case "agents":
		out[0] = "agent"
	case "message":
		out[0] = "messages"
	}
	return out
}

// matchSegs matches a route's segment patterns against the request
// segments, capturing {x} tokens into a vars map. Lengths must be equal.
func matchSegs(pattern, segs []string) (map[string]string, bool) {
	if len(pattern) != len(segs) {
		return nil, false
	}
	var vars map[string]string
	for i, tok := range pattern {
		if len(tok) >= 2 && tok[0] == '{' && tok[len(tok)-1] == '}' {
			if vars == nil {
				vars = map[string]string{}
			}
			key := tok[1 : len(tok)-1]
			val := segs[i]
			if u, err := url.PathUnescape(val); err == nil {
				val = u
			}
			vars[key] = val
			continue
		}
		if tok != segs[i] {
			return nil, false
		}
	}
	return vars, true
}

// splitPathSegments splits a URL path into its non-empty segments.
func splitPathSegments(path string) []string {
	parts := strings.Split(path, "/")
	out := parts[:0]
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// bufferAuditBody reads the request body so a describer can parse it,
// then restores r.Body verbatim for the handler. It reads the body in
// full — the same ReadAll the handler does — and restores exactly those
// bytes, so it can never truncate what the handler sees. Only audited
// command routes are buffered (their bodies are small JSON command
// payloads); the describers cap only the short strings they extract.
func bufferAuditBody(r *http.Request) []byte {
	if r.Body == nil {
		return nil
	}
	buf, err := io.ReadAll(r.Body)
	_ = r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(buf))
	if err != nil {
		return nil
	}
	return buf
}

// recordAuditRow assembles and writes one audit_log row. Best-effort: a
// logging failure is warned and swallowed — it must never fail the
// command, which has already completed by the time this runs.
func recordAuditRow(r *http.Request, route *auditRoute, vars map[string]string, body []byte, status int, source string) {
	fields := auditFields{
		Verb:      route.verb,
		GroupName: vars["name"],
	}
	if fields.Verb == "" {
		fields.Verb = vars["verb"]
	}
	// Default target = the {conv} selector, resolved to a readable label
	// best-effort. A describer may override this.
	if sel := vars["conv"]; sel != "" {
		fields.TargetConv, fields.TargetLabel = resolveAuditTarget(sel)
	}
	if route.describe != nil {
		route.describe(&auditCtx{vars: vars, body: body, fields: &fields})
	}
	if fields.Verb == "" {
		return // unclassifiable — nothing useful to record
	}

	kind, conv, label := auditActor(r, source)
	if _, err := db.InsertAuditLog(db.AuditLogEntry{
		ActorKind:   kind,
		ActorConv:   conv,
		ActorLabel:  label,
		Verb:        fields.Verb,
		TargetConv:  fields.TargetConv,
		TargetLabel: fields.TargetLabel,
		GroupName:   fields.GroupName,
		Detail:      fields.Detail,
		Method:      r.Method,
		Path:        r.URL.Path,
		Status:      status,
		Source:      source,
	}); err != nil {
		slog.Warn("audit: failed to record command", "verb", fields.Verb, "err", err)
	}
}

// auditActor resolves the request's actor. Dashboard requests are always
// the human (cookie-gated loopback). CLI requests route through the same
// classify() the permission gates use: human (operator token), agent
// (resolved conv-id + title), or unknown (fail-closed callers — still
// logged so a denied probe leaves a trail).
func auditActor(r *http.Request, source string) (kind, conv, label string) {
	if source == db.AuditSourceDashboard {
		return db.AuditActorHuman, "", "operator"
	}
	p := peerFromContext(r.Context())
	switch classify(p) {
	case classHuman:
		return db.AuditActorHuman, "", "operator"
	case classAgent:
		return db.AuditActorAgent, p.ConvID, auditConvLabel(p.ConvID)
	default:
		return db.AuditActorUnknown, p.ConvID, "(unknown)"
	}
}

// resolveAuditTarget turns a target selector (title / conv-id / prefix)
// into a (conv-id, label) pair, falling back to the raw selector when it
// can't be resolved — e.g. a just-deleted conv whose index row is gone.
func resolveAuditTarget(selector string) (conv, label string) {
	if res, _, err := agent.ResolveSelector(selector); err == nil && res.ConvID != "" {
		return res.ConvID, auditConvLabel(res.ConvID)
	}
	return selector, selector
}

// auditConvLabel is the readable display title for a conv-id, falling
// back to its short form when no conv_index row resolves.
func auditConvLabel(convID string) string {
	if row := agent.FreshConvRowResolved(convID); row != nil {
		if t := agent.DisplayTitle(row); t != "" {
			return t
		}
	}
	return short8(convID)
}

// auditClip trims and length-caps a free-text value for the Detail
// column / a label, collapsing whitespace so a multi-line message body
// reads as one symbolic line.
func auditClip(s string, max int) string {
	s = strings.TrimSpace(strings.Join(strings.Fields(s), " "))
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}

// --- describers --------------------------------------------------------

func describeMessage(c *auditCtx) {
	var b struct {
		To      string `json:"to"`
		Subject string `json:"subject"`
		Body    string `json:"body"`
	}
	_ = json.Unmarshal(c.body, &b)
	to := strings.TrimSpace(b.To)
	if group, ok := strings.CutPrefix(to, "group:"); ok {
		c.fields.GroupName = group
		c.fields.TargetLabel = "group:" + group
	} else if to != "" {
		c.fields.TargetConv, c.fields.TargetLabel = resolveAuditTarget(to)
	}
	detail := b.Subject
	if body := strings.TrimSpace(b.Body); body != "" {
		if detail != "" {
			detail += " — " + body
		} else {
			detail = body
		}
	}
	c.fields.Detail = auditClip(detail, 120)
}

func describeSpawn(c *auditCtx) {
	var b struct {
		Name string `json:"name"`
		Role string `json:"role"`
	}
	_ = json.Unmarshal(c.body, &b)
	name := strings.TrimSpace(b.Name)
	if name == "" {
		name = "(unnamed)"
	}
	c.fields.TargetLabel = name
	if role := strings.TrimSpace(b.Role); role != "" {
		c.fields.Detail = "role: " + auditClip(role, 60)
	}
}

// auditedAgentVerbs is the set of real per-agent lifecycle verbs the
// agent/{conv}/{verb} route audits. The guard matters: that route's
// {verb} capture is just a path segment, so without it a non-verb route
// that happens to share the shape — e.g. /v1/agent/aliases/{handle}
// (head aliases) — would be recorded with a bogus verb. An unknown verb
// blanks the row out (recordAuditRow drops a verbless row).
var auditedAgentVerbs = map[string]bool{
	"reincarnate":    true,
	"compact":        true,
	"rename":         true,
	"remote-control": true,
	"clone":          true,
	"stop":           true,
	"resume":         true,
	"delete":         true,
	"promote":        true,
	"retire":         true,
	"reinstate":      true,
	"notify":         true,
}

func describeAgentVerb(c *auditCtx) {
	// Verb comes from the path ({verb}); target is already resolved from
	// {conv}. Drop anything that isn't a known lifecycle verb so sibling
	// /v1/agent/... routes (head aliases) aren't mis-recorded.
	if !auditedAgentVerbs[c.fields.Verb] {
		c.fields.Verb = ""
		return
	}
	// A rename additionally carries the new title in the body.
	if c.fields.Verb == "rename" {
		describeNewName(c)
	}
}

// describeAgentTarget is a no-op refinement: the base record path already
// resolved the {conv} selector into the target. It exists so member /
// owner / agent-edit routes have an explicit, documented describer slot.
func describeAgentTarget(c *auditCtx) {}

// describeMemberTarget resolves the added member/owner from the request
// body (the {name} path var is the group, not the member).
func describeMemberTarget(c *auditCtx) {
	var b struct {
		Conv string `json:"conv"`
		To   string `json:"to"`
	}
	_ = json.Unmarshal(c.body, &b)
	sel := strings.TrimSpace(b.Conv)
	if sel == "" {
		sel = strings.TrimSpace(b.To)
	}
	if sel != "" {
		c.fields.TargetConv, c.fields.TargetLabel = resolveAuditTarget(sel)
	}
}

func describeNewName(c *auditCtx) {
	var b struct {
		Name  string `json:"name"`
		Title string `json:"title"`
	}
	_ = json.Unmarshal(c.body, &b)
	name := strings.TrimSpace(b.Title)
	if name == "" {
		name = strings.TrimSpace(b.Name)
	}
	if name != "" {
		c.fields.Detail = "→ " + auditClip(name, 80)
	}
}

func describeGroupCreate(c *auditCtx) {
	var b struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(c.body, &b)
	if name := strings.TrimSpace(b.Name); name != "" {
		c.fields.GroupName = name
	}
}

func describeCron(c *auditCtx) {
	var b struct {
		Name string `json:"name"`
		Body string `json:"body"`
	}
	_ = json.Unmarshal(c.body, &b)
	detail := strings.TrimSpace(b.Name)
	if body := strings.TrimSpace(b.Body); body != "" {
		if detail != "" {
			detail += ": " + body
		} else {
			detail = body
		}
	}
	c.fields.Detail = auditClip(detail, 100)
}

func describePerm(c *auditCtx) {
	var b struct {
		Slug string `json:"slug"`
		Conv string `json:"conv"`
	}
	_ = json.Unmarshal(c.body, &b)
	if slug := strings.TrimSpace(b.Slug); slug != "" {
		c.fields.Detail = slug
	}
	if sel := strings.TrimSpace(b.Conv); sel != "" {
		c.fields.TargetConv, c.fields.TargetLabel = resolveAuditTarget(sel)
	}
}

func describePermSet(c *auditCtx) {
	var b struct {
		Conv      string            `json:"conv"`
		Overrides map[string]string `json:"overrides"`
	}
	_ = json.Unmarshal(c.body, &b)
	if sel := strings.TrimSpace(b.Conv); sel != "" {
		c.fields.TargetConv, c.fields.TargetLabel = resolveAuditTarget(sel)
	}
	var parts []string
	for slug, effect := range b.Overrides {
		parts = append(parts, slug+"="+effect)
	}
	c.fields.Detail = auditClip(strings.Join(parts, ", "), 120)
}

func describeSudo(c *auditCtx) {
	var b struct {
		Slug   string `json:"slug"`
		Conv   string `json:"conv"`
		Reason string `json:"reason"`
	}
	_ = json.Unmarshal(c.body, &b)
	detail := strings.TrimSpace(b.Slug)
	if reason := strings.TrimSpace(b.Reason); reason != "" {
		detail = strings.TrimSpace(detail + " (" + reason + ")")
	}
	c.fields.Detail = auditClip(detail, 120)
	if sel := strings.TrimSpace(b.Conv); sel != "" {
		c.fields.TargetConv, c.fields.TargetLabel = resolveAuditTarget(sel)
	}
}

func describeNotifyHuman(c *auditCtx) {
	var b struct {
		Subject string `json:"subject"`
		Body    string `json:"body"`
	}
	_ = json.Unmarshal(c.body, &b)
	detail := b.Subject
	if body := strings.TrimSpace(b.Body); body != "" {
		if detail != "" {
			detail += " — " + body
		} else {
			detail = body
		}
	}
	c.fields.Detail = auditClip(detail, 120)
}
