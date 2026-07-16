package agentd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

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

// auditResult carries safe handler-produced attribution back to the audit
// middleware after a command succeeds. It deliberately cannot carry request
// bodies: routes such as process run creation contain runtime params that may
// be secrets and must remain unbuffered by the audit layer.
type auditResult struct {
	targetLabel string
}

type auditResultContextKey struct{}

func setAuditTargetLabel(r *http.Request, label string) {
	if result, ok := r.Context().Value(auditResultContextKey{}).(*auditResult); ok {
		result.targetLabel = auditClip(label, 120)
	}
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
	// Reply to a received message — `tclaude agent reply` / `inbox watch`,
	// which POST /v1/messages/{id}/reply (CLI/TUI only; the dashboard has
	// no reply path). Without this entry the single-segment "messages"
	// route above never matches the three-segment reply path, so replies
	// went unrecorded. The reply's recipient is not in the body (the daemon
	// derives it from the original message), so describeReply resolves it.
	{method: http.MethodPost, segs: []string{"messages", "{id}", "reply"}, verb: "reply", describe: describeReply},
	// Message deletions — we audit sends, so we audit deletions too (else
	// the trail can be erased untracked). DELETE /v1/messages/{id} is the
	// CLI/inbox-watch single-delete; POST /v1/inbox/prune is the CLI bulk
	// prune; the two /api/mailbox/* routes are the dashboard's operator-side
	// delete (a list of ids) and wipe (whole mailboxes by conv-id).
	{method: http.MethodDelete, segs: []string{"messages", "{id}"}, verb: "message.delete", describe: describeMessageDelete},
	{method: http.MethodPost, segs: []string{"inbox", "prune"}, verb: "inbox.prune", describe: describeInboxPrune},
	{method: http.MethodPost, segs: []string{"mailbox", "delete"}, verb: "message.delete", describe: describeMailboxDelete},
	{method: http.MethodPost, segs: []string{"mailbox", "wipe"}, verb: "mailbox.wipe", describe: describeMailboxWipe},

	// Self-lifecycle. The `tclaude agent {reincarnate,clone,rename,compact,
	// remote-control}` verbs hit /v1/whoami/{verb} when acting on SELF and
	// /v1/agent/{conv}/{verb} when acting on a peer (--target). Only the
	// latter is covered by the agent/{conv}/{verb} route below, so a
	// self-reincarnate/clone/rename went unaudited. describeWhoamiVerb gates
	// to the real lifecycle verbs (sibling /v1/whoami/{context,dir} are GET
	// reads, never audited) and the target stays blank — the actor column
	// already names the agent acting on itself.
	{segs: []string{"whoami", "{verb}"}, describe: describeWhoamiVerb},

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
	// Binary attachment bodies must not be buffered by the audit layer. Their
	// message metadata is persisted on the resulting human_messages row.
	{method: http.MethodPost, segs: []string{"notify-human", "attachment"}, verb: "notify-human.attach"},

	// Human clipboard write. The detail records only the byte count, never
	// the copied text — clipboard content is often sensitive (a secret, a
	// password) and must not land in the audit log.
	{method: http.MethodPost, segs: []string{"clipboard"}, verb: "clipboard", describe: describeClipboard},

	// Group templates: instantiating one spawns a whole group of agents —
	// a real coordination action — so it's audited (the CLI and dashboard
	// both POST /…/templates/{name}/instantiate). Template authoring
	// (create/update/delete/from-group) is config-shaped and left out.
	{method: http.MethodPost, segs: []string{"templates", "{name}", "instantiate"}, verb: "template.instantiate", describe: describeTemplateInstantiate},
	// Reinforce (JOH-376) spawns a template's roster INTO an existing group — a
	// real coordination action against an existing team, so it is audited too.
	{method: http.MethodPost, segs: []string{"templates", "{name}", "reinforce"}, verb: "template.reinforce", describe: describeTemplateInstantiate},

	// Process run creation is a durable engine command that may launch
	// performers. Keep the describer nil: the body contains runtime params that
	// may be secrets, and actor/source/status/path are sufficient attribution.
	{method: http.MethodPost, segs: []string{"process", "runs"}, verb: "process.run.create"},
	// Signal satisfaction advances a durable schema-7 wait. Keep the describer
	// nil so the signal body is never copied into the audit detail.
	{method: http.MethodPost, segs: []string{"process", "runs", "{id}", "nodes", "{node}", "signal"}, verb: "process.signal"},

	// Remote-access administration (dashboard, cert-admin gated). Issuing a
	// client cert / adding SAN hosts / (re)running setup are security-
	// relevant admin actions worth a trail. Describers capture only safe
	// fields — passphrases and p12 passwords are NEVER recorded.
	{method: http.MethodPost, segs: []string{"remote-access", "add-client"}, verb: "remote-access.add-client", describe: describeRemoteAccessClient},
	{method: http.MethodPost, segs: []string{"remote-access", "add-hosts"}, verb: "remote-access.add-hosts", describe: describeRemoteAccessHosts},
	{method: http.MethodPost, segs: []string{"remote-access", "setup"}, verb: "remote-access.setup", describe: describeRemoteAccessSetup},

	// Agent power control (dashboard): shutting down / powering on a group
	// or all agents is a fleet-wide state change.
	{method: http.MethodPost, segs: []string{"shutdown"}, verb: "power.shutdown", describe: describePower},
	{method: http.MethodPost, segs: []string{"power-on"}, verb: "power.on", describe: describePower},
}

// auditRequests wraps a mux so every matched command writes an audit
// row. It buffers the request body (restoring it for the handler) only
// for matched routes, runs the handler, then records the row with the
// resulting status — after the response is flushed, so it adds no
// user-visible latency and cannot affect the response.
func auditRequests(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		route, vars, source, ok := matchAuditRoute(r.Method, r.URL.Path)
		var result *auditResult
		if ok {
			source = auditRequestSource(r, source)
			result = &auditResult{}
			r = r.WithContext(context.WithValue(r.Context(), auditResultContextKey{}, result))
		}
		var body []byte
		if ok && route.describe != nil {
			body = bufferAuditBody(r)
		}
		rec := &statusRec{ResponseWriter: w, code: 200}
		h.ServeHTTP(rec, r)
		if ok {
			recordAuditRow(r, route, vars, body, result, rec.code, source)
		}
	})
}

// auditRequestSource distinguishes authenticated browser requests that use
// the Processes tab's shared /v1 surface from Unix-socket CLI requests. The
// dashboard deliberately consumes the public process API instead of exposing
// a duplicate /api contract, so the path prefix alone identifies only the
// route shape. Remote requests carry the authentication boundary's unforgeable
// pre-auth marker; loopback requests re-run the cookie + origin predicate.
// Agent and human CLI peers remain classified through their socket context.
func auditRequestSource(r *http.Request, source string) string {
	if dashboardPreAuthed(r) {
		return db.AuditSourceDashboard
	}
	if source != db.AuditSourceCLI || !strings.HasPrefix(r.URL.Path, "/v1/process/") {
		return source
	}
	if ok, _, _, _ := dashboardAuthResult(r); ok {
		return db.AuditSourceDashboard
	}
	return source
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
	case "operator-message":
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
	if err != nil {
		// On a failed/partial read, do NOT hand the handler a silently
		// truncated body (it could decode to a partial command). Close the
		// original and leave it: the handler hits the same read error and
		// rejects the request. We record no body for audit.
		_ = r.Body.Close()
		return nil
	}
	_ = r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(buf))
	return buf
}

// recordAuditRow assembles and writes one audit_log row. Best-effort: a
// logging failure is warned and swallowed — it must never fail the
// command, which has already completed by the time this runs.
func recordAuditRow(r *http.Request, route *auditRoute, vars map[string]string, body []byte, result *auditResult, status int, source string) {
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
	if status < http.StatusBadRequest && result != nil && result.targetLabel != "" {
		fields.TargetLabel = result.targetLabel
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

// auditActor resolves the request's actor. A dashboard request is the
// human operator IFF it carries a valid dashboard session — either the remote
// boundary's pre-auth marker or a loopback session that passes the auth
// predicate. We never key on the response status, so a post-auth policy refusal
// (e.g. a blocklisted sudo grant the operator did clear the cookie gate for,
// returning 403) stays attributed to the operator, while an unauthenticated /
// cross-origin probe is recorded as "unauthenticated" instead of masquerading
// as the human. CLI requests
// route through the same classify() the permission gates use: human
// (operator token), agent (resolved conv-id + title), or unknown
// (fail-closed callers — still logged so a denied probe leaves a trail).
func auditActor(r *http.Request, source string) (kind, conv, label string) {
	if source == db.AuditSourceDashboard {
		if dashboardPreAuthed(r) {
			return db.AuditActorHuman, "", "operator"
		}
		if ok, _, _, _ := dashboardAuthResult(r); !ok {
			return db.AuditActorUnknown, "", "unauthenticated"
		}
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
		// ResolveSelector already returned the conv_index row — use it
		// directly rather than re-resolving via auditConvLabel.
		if res.Row != nil {
			if t := agent.DisplayTitle(res.Row); t != "" {
				return res.ConvID, t
			}
		}
		return res.ConvID, short8(res.ConvID)
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

// auditDetailMax caps a freeform Detail value at write time — message /
// reply / notify bodies, and the cron / template / sudo text. Generous
// enough that the dashboard's Detail column (which wraps) shows the whole
// thing for almost all rows, while still bounding a pathological body in the
// stored audit row. The short, structured labels (role: / client: / scope:
// / hosts: / → name) keep their own tighter caps — they're bounded by
// nature, so 512 would never apply.
const auditDetailMax = 512

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
	c.fields.Detail = auditClip(detail, auditDetailMax)
}

// describeReply records a reply to a received message. Unlike a fresh
// message, the request body carries only subject/body — the recipient is
// the original message's sender, which the daemon derives from the {id}
// path var. We resolve it here so the trail still shows WHO the reply
// went to, mirroring the handler's own routing: the target is the
// original sender walked forward to its live successor (so a reply that
// lands on a reincarnated agent is attributed to the successor), and the
// routing group stays the original's group (0 = a direct/off-group send).
// A best-effort lookup: if the original row is gone (a 404 reply) the
// target simply stays blank, which still records the attempt.
func describeReply(c *auditCtx) {
	var b struct {
		Subject string `json:"subject"`
		Body    string `json:"body"`
	}
	_ = json.Unmarshal(c.body, &b)

	if id, err := strconv.ParseInt(c.vars["id"], 10, 64); err == nil {
		if orig, err := db.GetAgentMessage(id); err == nil && orig != nil {
			if orig.GroupID != 0 {
				if g, _ := db.GetAgentGroupByID(orig.GroupID); g != nil {
					c.fields.GroupName = g.Name
				}
			}
			target, _ := walkSuccession(orig.FromConv)
			if target == "" {
				target = orig.FromConv
			}
			if target != "" {
				c.fields.TargetConv, c.fields.TargetLabel = resolveAuditTarget(target)
			}
		}
	}

	detail := b.Subject
	if body := strings.TrimSpace(b.Body); body != "" {
		if detail != "" {
			detail += " — " + body
		} else {
			detail = body
		}
	}
	c.fields.Detail = auditClip(detail, auditDetailMax)
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

// auditedWhoamiVerbs is the set of real self-lifecycle verbs the
// whoami/{verb} route audits. Mirrors auditedAgentVerbs but scoped to the
// verbs an agent can run on ITSELF — the others (context, dir) are GET
// reads the mutating-method gate already drops, but the allowlist keeps
// the audited set explicit so a future POST sibling isn't mis-recorded.
var auditedWhoamiVerbs = map[string]bool{
	"reincarnate":    true,
	"clone":          true,
	"rename":         true,
	"compact":        true,
	"remote-control": true,
}

// describeWhoamiVerb classifies a self-lifecycle command. The verb is the
// {verb} path capture; there is no target — the actor (resolved later from
// the caller's peer identity) IS the subject. Drops anything that isn't a
// known self-verb so a POST to a read sibling (/v1/whoami/context) can't
// be mis-recorded.
func describeWhoamiVerb(c *auditCtx) {
	if !auditedWhoamiVerbs[c.fields.Verb] {
		c.fields.Verb = ""
		return
	}
	// A self-rename carries the new title in the body, same as the
	// manager-path rename.
	if c.fields.Verb == "rename" {
		describeNewName(c)
	}
}

// describeMessageDelete records a single-message deletion. We can only key
// off the {id} path capture: the describer runs AFTER the handler, by which
// point the row is gone, so a preview lookup would always miss. The actor
// (a party to the message) is named in the actor column; the detail records
// which message was removed.
func describeMessageDelete(c *auditCtx) {
	if id := strings.TrimSpace(c.vars["id"]); id != "" {
		c.fields.Detail = "message #" + id
	}
}

// describeInboxPrune records a bulk inbox prune. The body carries the
// cutoff window and a read-only flag; we summarise both so the trail shows
// how aggressive the prune was.
func describeInboxPrune(c *auditCtx) {
	var b struct {
		OlderThanSeconds int64 `json:"older_than_seconds"`
		ReadOnly         bool  `json:"read_only"`
	}
	_ = json.Unmarshal(c.body, &b)
	detail := "older than " + auditDuration(b.OlderThanSeconds)
	if b.ReadOnly {
		detail += ", read-only"
	}
	c.fields.Detail = detail
}

// describeMailboxDelete records a dashboard operator-side delete of one or
// more messages by id (Detail = the count).
func describeMailboxDelete(c *auditCtx) {
	var b struct {
		IDs []int64 `json:"ids"`
	}
	_ = json.Unmarshal(c.body, &b)
	c.fields.Detail = auditCount(len(b.IDs), "message", "messages")
}

// describeMailboxWipe records a dashboard wipe of whole mailboxes by
// conv-id. A single-mailbox wipe also names that mailbox as the target.
func describeMailboxWipe(c *auditCtx) {
	var b struct {
		Convs []string `json:"convs"`
	}
	_ = json.Unmarshal(c.body, &b)
	c.fields.Detail = auditCount(len(b.Convs), "mailbox", "mailboxes")
	if len(b.Convs) == 1 {
		c.fields.TargetConv, c.fields.TargetLabel = resolveAuditTarget(b.Convs[0])
	}
}

// describeTemplateInstantiate records a template instantiation — the
// {name} path capture is the source template; the body names the new group
// being spawned (which becomes the row's group + a task preview detail).
func describeTemplateInstantiate(c *auditCtx) {
	var b struct {
		GroupName string `json:"group_name"`
		Task      string `json:"task"`
	}
	_ = json.Unmarshal(c.body, &b)
	if g := strings.TrimSpace(b.GroupName); g != "" {
		c.fields.GroupName = g
		c.fields.TargetLabel = g
	}
	detail := "from template " + c.vars["name"]
	if task := strings.TrimSpace(b.Task); task != "" {
		detail += ": " + task
	}
	c.fields.Detail = auditClip(detail, auditDetailMax)
}

// describeRemoteAccessClient records issuing a client cert. Only the client
// NAME is captured — the p12 password in the body is never recorded.
func describeRemoteAccessClient(c *auditCtx) {
	var b struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(c.body, &b)
	if name := strings.TrimSpace(b.Name); name != "" {
		c.fields.Detail = "client: " + auditClip(name, 80)
	}
}

// describeRemoteAccessHosts records adding SAN hosts to the server cert.
func describeRemoteAccessHosts(c *auditCtx) {
	var b struct {
		Hosts string `json:"hosts"`
	}
	_ = json.Unmarshal(c.body, &b)
	if hosts := strings.TrimSpace(b.Hosts); hosts != "" {
		c.fields.Detail = "hosts: " + auditClip(hosts, 100)
	}
}

// describeRemoteAccessSetup records first-time / regenerate cert setup.
// Captures only the non-secret fields — the passphrase and p12 password in
// the body are deliberately NOT read.
func describeRemoteAccessSetup(c *auditCtx) {
	var b struct {
		Bind       string `json:"bind"`
		ClientName string `json:"client_name"`
		Regenerate bool   `json:"regenerate"`
		Enable     bool   `json:"enable"`
	}
	_ = json.Unmarshal(c.body, &b)
	parts := make([]string, 0, 4)
	if bind := strings.TrimSpace(b.Bind); bind != "" {
		parts = append(parts, "bind "+bind)
	}
	if cn := strings.TrimSpace(b.ClientName); cn != "" {
		parts = append(parts, "client "+cn)
	}
	if b.Regenerate {
		parts = append(parts, "regenerate")
	}
	if b.Enable {
		parts = append(parts, "enable")
	}
	c.fields.Detail = auditClip(strings.Join(parts, ", "), 120)
}

// describePower records an agent power shutdown / power-on. The body's
// scope ("all" / "group") + group name summarise what was targeted.
func describePower(c *auditCtx) {
	var b struct {
		Scope string `json:"scope"`
		Group string `json:"group"`
	}
	_ = json.Unmarshal(c.body, &b)
	if g := strings.TrimSpace(b.Group); g != "" {
		c.fields.GroupName = g
	}
	if scope := strings.TrimSpace(b.Scope); scope != "" {
		c.fields.Detail = "scope: " + auditClip(scope, 60)
	}
}

// auditDuration renders a second count as a Go duration string for the
// prune detail (e.g. 3600 → "1h0m0s"; Go's Duration has no day unit, so a
// multi-day window reads as "Nh0m0s").
func auditDuration(seconds int64) string {
	if seconds <= 0 {
		return "0s"
	}
	d := time.Duration(seconds) * time.Second
	return d.String()
}

// auditCount renders "N singular" / "N plural" for a count detail.
func auditCount(n int, singular, plural string) string {
	if n == 1 {
		return "1 " + singular
	}
	return strconv.Itoa(n) + " " + plural
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
	// The new name lives under different keys per route: a group rename
	// sends new_name (groups_rename.go / dashboard), an agent rename sends
	// title (handleAgentRename). Accept all three.
	var b struct {
		NewName string `json:"new_name"`
		Title   string `json:"title"`
		Name    string `json:"name"`
	}
	_ = json.Unmarshal(c.body, &b)
	name := strings.TrimSpace(b.NewName)
	if name == "" {
		name = strings.TrimSpace(b.Title)
	}
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
		Name    string `json:"name"`
		Subject string `json:"subject"`
		Body    string `json:"body"`
	}
	_ = json.Unmarshal(c.body, &b)
	detail := strings.TrimSpace(b.Name)
	if s := strings.TrimSpace(b.Subject); s != "" {
		detail = strings.TrimSpace(detail + " [" + s + "]")
	}
	if body := strings.TrimSpace(b.Body); body != "" {
		if detail != "" {
			detail += ": " + body
		} else {
			detail = body
		}
	}
	c.fields.Detail = auditClip(detail, auditDetailMax)
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
	// Sort the slug=effect pairs so two identical requests produce the
	// same detail string (map iteration order is otherwise random).
	parts := make([]string, 0, len(b.Overrides))
	for slug, effect := range b.Overrides {
		parts = append(parts, slug+"="+effect)
	}
	sort.Strings(parts)
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
	c.fields.Detail = auditClip(detail, auditDetailMax)
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
	c.fields.Detail = auditClip(detail, auditDetailMax)
}

// describeClipboard records a clipboard write as its byte count only. The
// copied text itself is deliberately never logged — it is frequently
// sensitive (a token, a password, a private snippet), so the audit trail
// captures that a copy happened and how big it was, nothing more.
func describeClipboard(c *auditCtx) {
	var b struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(c.body, &b)
	c.fields.Detail = fmt.Sprintf("%d bytes", len(b.Text))
}
