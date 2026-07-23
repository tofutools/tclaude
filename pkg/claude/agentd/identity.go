package agentd

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// peerKey is the context key under which we stash the resolved peer
// identity for the lifetime of a request.
type peerKey struct{}

type permissionDefaultsKey struct{}

// withPermissionDefaults snapshots config-backed defaults before a caller
// enters a global mutation lock. DB-backed grants/denies are still evaluated
// at the authorization point; only filesystem config loading is prepaid.
func withPermissionDefaults(r *http.Request, slugs ...string) *http.Request {
	cfg, _ := config.Load()
	defaults := make(map[string]bool, len(slugs))
	for _, slug := range slugs {
		defaults[slug] = cfg.HasDefaultPermission(slug)
	}
	return r.WithContext(context.WithValue(r.Context(), permissionDefaultsKey{}, defaults))
}

// peer is the identity resolved from the connecting socket peer. It is
// raw material: no handler reads these fields directly for an
// authorization decision — every human-vs-agent decision routes through
// classify().
//
//   - PID is the process that opened the socket. 0 if peerPID failed.
//   - ConvID is the current conv-id of the nearest claude/node ancestor,
//     read from ~/.claude/sessions/<pid>.json (or, as a fallback, from
//     the sessions table by host pid). Empty when the caller has no
//     claude ancestor *or* when the ancestor's conv-id couldn't be
//     resolved.
//   - HasClaudeAncestor is true iff a claude/node ancestor was observed
//     anywhere in the pid tree, regardless of conv-id resolvability.
//   - HumanTokenValid is true iff the request carried a valid operator
//     token (see humantoken.go).
//   - DashboardHuman is true only for the synthetic peer stamped by
//     asDashboardHumanPeer — a cookie-authenticated dashboard delegation.
type peer struct {
	PID               int
	ConvID            string
	HasClaudeAncestor bool
	HumanTokenValid   bool
	DashboardHuman    bool
}

// peerFromContext returns the peer attached by the identity middleware.
// Always non-nil for handlers; PID may be 0 if the lookup failed.
func peerFromContext(ctx context.Context) *peer {
	v, _ := ctx.Value(peerKey{}).(*peer)
	if v == nil {
		return &peer{}
	}
	return v
}

// callerClass is the single, centralised verdict on who a request's peer
// is. EVERY human-vs-agent authorization decision in the daemon routes
// through classify() — no handler re-derives identity from the raw peer
// fields, and there is no exception.
type callerClass int

const (
	// classUnidentified: the peer PID could not be read. Fail closed → 401.
	classUnidentified callerClass = iota
	// classAgent: a confirmed Claude Code caller with a resolved conv-id.
	classAgent
	// classAgentUnknown: a Claude Code ancestor is present but its conv-id
	// could not be resolved. Fail closed → 403; never treated as the human.
	classAgentUnknown
	// classHuman: the human operator — either the cookie-authenticated
	// dashboard, or a CLI caller presenting a valid operator token.
	classHuman
	// classUnconfirmed: no Claude Code ancestor and no valid operator
	// token. Fail closed → 403. Before the fail-closed model this case was
	// assumed to be the human (fail-open) — that assumption is now gone.
	classUnconfirmed
)

// classify is THE policy chokepoint: it maps a resolved peer to one
// callerClass. The precedence is deliberate and load-bearing:
//
//   - DashboardHuman first: a cookie-authenticated dashboard delegation
//     (asDashboardHumanPeer) is the human regardless of process tree.
//   - A Claude Code ancestor wins over any operator token. The human
//     exports TCLAUDE_HUMAN_TOKEN into their shell, so a CC session
//     launched from that shell inherits it; if the token could promote a
//     caller, such an agent would escalate to human. An agent-family
//     caller is therefore never offered the token branch.
//   - Only a caller with no CC ancestor is eligible to be the human, and
//     only with a valid token. Anything else is classUnconfirmed → 403.
func classify(p *peer) callerClass {
	if p.DashboardHuman {
		return classHuman
	}
	if p.PID == 0 {
		return classUnidentified
	}
	if p.HasClaudeAncestor {
		if p.ConvID != "" {
			return classAgent
		}
		return classAgentUnknown
	}
	if p.HumanTokenValid {
		return classHuman
	}
	return classUnconfirmed
}

// writeUnconfirmed writes the standard fail-closed 403 for a caller the
// daemon could neither confirm as an agent nor as the human. The body is
// self-explanatory and points the human operator at the fix.
func writeUnconfirmed(w http.ResponseWriter, r *http.Request) {
	if strings.TrimSpace(r.Header.Get(agentipc.AgentHintHeader)) == "1" {
		writeError(w, http.StatusForbidden, "unconfirmed",
			"unconfirmed managed-agent caller: agentd could not resolve this process to a known harness session. "+
				"The agent may be dangling or its session identity may be stale; ask the human operator to inspect or resume it.")
		return
	}
	writeError(w, http.StatusForbidden, "unconfirmed",
		"unconfirmed caller: not a known agent, and no valid operator token. "+
			"If you are the human operator, set TCLAUDE_HUMAN_TOKEN to the "+
			"operator token printed on the agentd startup banner, then retry.")
}

// writeUnidentified writes the fail-closed 401 for a peer whose PID could
// not be read from the socket.
func writeUnidentified(w http.ResponseWriter) {
	writeError(w, http.StatusUnauthorized, "auth",
		"could not determine peer PID; refusing the request")
}

// writeAgentUnknown writes the fail-closed 403 for a caller with a Claude
// Code ancestor whose conv-id could not be resolved.
func writeAgentUnknown(w http.ResponseWriter) {
	writeError(w, http.StatusForbidden, "auth",
		"caller has a Claude Code ancestor but no resolvable conv-id")
}

// authedCaller resolves a request to either the human operator or a
// confirmed agent — the common shape for endpoints that admit both and
// then scope behaviour by conv-id. ok is true for classHuman (convID "",
// isHuman true) and classAgent (convID set, isHuman false). For
// unidentified / unconfirmed / unidentifiable-agent callers it writes the
// fail-closed response and returns ok=false; the caller just returns.
func authedCaller(w http.ResponseWriter, r *http.Request) (convID string, isHuman, ok bool) {
	p := peerFromContext(r.Context())
	switch classify(p) {
	case classHuman:
		return "", true, true
	case classAgent:
		return p.ConvID, false, true
	case classUnidentified:
		writeUnidentified(w)
	case classAgentUnknown:
		writeAgentUnknown(w)
	case classUnconfirmed:
		writeUnconfirmed(w, r)
	}
	return "", false, false
}

// asDashboardHumanPeer stamps a synthetic "human via dashboard cookie"
// peer onto the request, so a /v1 handler delegated-to from a
// cookie-authenticated dashboard endpoint is classified as the human.
// The dashboard cookie + Origin pin in checkDashboardAuth IS the
// human-consent layer here — the dashboard human legitimately holds no
// operator token, so DashboardHuman is set explicitly and classify()
// returns classHuman for it. (Without this the synthetic peer would have
// no CC ancestor and no token → classUnconfirmed → 403.)
//
// Used by handleDashboardCronCreate / dashboardCronPatch when they
// delegate to handleCronCreate / handleCronPatch — same DB writes,
// same validation, without duplicating either.
func asDashboardHumanPeer(r *http.Request) *http.Request {
	p := &peer{PID: 1, DashboardHuman: true}
	return r.WithContext(context.WithValue(r.Context(), peerKey{}, p))
}

// withIdentity is the per-request middleware that resolves the connecting
// peer's PID, walks the process tree to a coding-harness ancestor (claude,
// codex, … or node), reads its per-pid session file or falls back to the
// sessions table for its conv-id, verifies any operator token the request
// carries, and attaches the result to the request context. Handlers turn
// that peer into an authorization decision via classify().
//
// Caller-controlled environment values, including TCLAUDE_SESSION_ID, may
// support client-side session routing but must never establish or override
// the caller identity attached here.
//
// Resolving a non-empty conv-id also opportunistically flushes any
// nudges queued for this conv while it was offline. The flush is
// debounced per-conv and runs on its own goroutine, so chatty agents
// don't pay any latency on the request that triggered it.
func withIdentity(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := &peer{}
		if uconn, ok := r.Context().Value(unixConnKey{}).(*net.UnixConn); ok && uconn != nil {
			if pid, err := peerPID(uconn); err == nil {
				p.PID = pid
				p.ConvID, p.HasClaudeAncestor = convIDForPID(pid)
			}
		}
		p.HumanTokenValid = verifyHumanToken(r)
		if p.ConvID != "" {
			maybeFlushUndelivered(p.ConvID)
			enrollCallerOnce(p.ConvID)
		}
		ctx := context.WithValue(r.Context(), peerKey{}, p)
		h.ServeHTTP(w, r.WithContext(ctx))
	})
}

// enrolledCallers remembers conv-ids already run through EnsureAgentForConv
// this daemon lifetime, so the per-request identity middleware does at
// most one actor-ensure write per conv. EnsureAgentForConv itself is
// idempotent; the cache just spares a chatty agent a DB round-trip on every
// subsequent /v1 call.
var enrolledCallers sync.Map

// enrollCallerOnce registers a conv that is talking to the daemon as an
// agent. Running any `tclaude agent` command is, by that act, agentic
// behaviour — this is the catch-all trigger for agents that were never
// spawned into a group or granted a permission. Ensure-only: it mints / links
// an actor but never reinstates a conv the human deliberately retired.
func enrollCallerOnce(convID string) {
	if convID == "" {
		return
	}
	if _, seen := enrolledCallers.LoadOrStore(convID, true); seen {
		return
	}
	// EnsureAgentForConv mints / links the conv's stable actor identity
	// (JOH-26): a conv that talks to the daemon is, by that act, an agent.
	// Idempotent.
	if _, _, err := db.EnsureAgentForConv(convID, "cli"); err != nil {
		slog.Warn("identity: ensure caller actor failed", "conv", convID, "error", err)
		enrolledCallers.Delete(convID) // let a later request retry
	}
}

// requireAgent enforces that the caller is a confirmed agent (classAgent:
// a resolved Claude Code conv-id). Returns the conv-id and true on
// success, or writes 401 and returns false. The human operator and
// unconfirmed callers are refused — this endpoint has no human path.
func requireAgent(w http.ResponseWriter, r *http.Request) (string, bool) {
	p := peerFromContext(r.Context())
	if classify(p) != classAgent {
		writeError(w, http.StatusUnauthorized, "auth",
			"this endpoint requires an agent identity (a resolved Claude Code conv-id)")
		return "", false
	}
	return p.ConvID, true
}

// Permission slugs are simple dotted strings the daemon accepts in
// `agent.default_permissions` / `agent.permission_overrides`. Keep this
// list in sync with the agent-coord skill / docs.
const (
	PermSelfRename         = "self.rename"
	PermSelfCompact        = "self.compact"
	PermSelfClone          = "self.clone"
	PermSelfRemoteControl  = "self.remote-control"
	PermSelfTask           = "self.task"
	PermSelfPR             = "self.pr"
	PermSelfTags           = "self.tags"
	PermSelfDirRepair      = "self.dir-repair"
	PermAgentReincarnate   = "agent.reincarnate"
	PermAgentCompact       = "agent.compact"
	PermAgentRename        = "agent.rename"
	PermAgentRemoteControl = "agent.remote-control"
	PermAgentClone         = "agent.clone"
	PermAgentContextInfo   = "agent.context-info"
	PermAgentTask          = "agent.task"
	PermAgentPR            = "agent.pr"
	PermAgentTags          = "agent.tags"
	PermGroupsCreate       = "groups.create"
	PermGroupsRm           = "groups.rm"
	PermGroupsStop         = "groups.stop"
	PermGroupsResume       = "groups.resume"
	PermGroupsRetire       = "groups.retire"
	PermGroupsSpawn        = "groups.spawn"
	PermGroupsOwn          = "groups.own"
	PermMemberAdd          = "member.add"
	PermMemberRemove       = "member.remove"
	PermMemberRedesignate  = "member.redesignate"
	PermSelfSchedule       = "self.schedule"
	PermAgentSchedule      = "agent.schedule"
	PermAgentStop          = "agent.stop"
	PermAgentResume        = "agent.resume"
	PermAgentDelete        = "agent.delete"
	PermGroupsArchive      = "groups.archive"
	PermGroupsNest         = "groups.nest"
	PermAgentInboxWatch    = "agent.inbox-watch"
	PermGroupsRename       = "groups.rename"
	PermGroupsClone        = "groups.clone"
	PermGroupsLinkAdd      = "groups.link.add"
	PermGroupsLinkRm       = "groups.link.rm"
	PermAgentPromote       = "agent.promote"
	PermAgentRetire        = "agent.retire"
	PermMessageDirect      = "message.direct"
	PermGroupsExport       = "groups.export"
	PermGroupsImport       = "groups.import"
	PermTemplatesManage    = "templates.manage"
	PermTemplatesUse       = "templates.instantiate"
	PermProfilesManage     = "profiles.manage"
	// Sandbox-profile policy can grant host filesystem access and inject launch
	// environment. Keep it separate from profiles.manage: permission to edit a
	// spawn-dialog preset must not imply permission to widen a sandbox.
	PermSandboxProfilesManage = "sandbox-profiles.manage"
	// Draft is intentionally separate from Manage: a dashboard-summoned scribe
	// may propose a validated profile for human review, but cannot persist it,
	// assign it, or use it to launch an agent.
	PermSandboxProfilesDraft   = "sandbox-profiles.draft"
	PermRolesManage            = "roles.manage"
	PermProcessAdvance         = "process.advance"
	PermProcessTemplatesRead   = "process.templates.read"
	PermProcessTemplatesManage = "process.templates.manage"
	PermProcessRunsRead        = "process.runs.read"
	PermProcessRunsManage      = "process.runs.manage"
	PermHumanNotify            = "human.notify"
	PermHumanClipboard         = "human.clipboard"
	// PermSettingsDefaultModel gates writing the user-level default
	// model into ~/.claude/settings.json — a file in the human's home
	// that also carries hooks and permission config, so not
	// default-granted (effectively human-only).
	PermSettingsDefaultModel = "settings.default-model"
)

// permResolution is the verdict of the non-interactive permission
// sources — everything the daemon consults before the human-approval
// popup. requirePermission and its cross-agent / boolean siblings all
// route through resolvePermission so the precedence is defined once.
type permResolution int

const (
	// permUndecided: no source spoke for this (conv, slug). The caller
	// falls through to its own extra checks (e.g. group-owner bypass)
	// and finally the popup / 403.
	permUndecided permResolution = iota
	// permAllow: an allow-source granted it — an active sudo grant, a
	// per-conv grant override, or the config default-permissions list.
	permAllow
	// permDeny: an explicit per-conv deny override. Authoritative below
	// sudo: it suppresses the config default and any structural bypass.
	// The caller still offers the human-approval popup as the one-off
	// escape hatch.
	permDeny
)

// resolvePermission evaluates the non-interactive permission sources
// for (convID, slug) in precedence order:
//
//  1. Active sudo grant — a fresh, time-bounded, explicit human
//     elevation (`tclaude agent sudo`). Wins over everything,
//     including a permanent deny.
//  2. Per-conv override (agent_permissions.effect, written by the
//     dashboard permanent-permission editor / `permissions` CLI) —
//     grant => allow, deny => authoritative deny.
//  3. Any active group the agent belongs to grants the slug — allow.
//  4. Config default-permissions list (~/.tclaude/config.json) — allow.
//
// Nothing matched => permUndecided.
func resolvePermission(convID, slug string) permResolution {
	resolution, _ := resolvePermissionWithSudoGrantID(convID, slug)
	return resolution
}

// resolvePermissionWithSudoGrantID evaluates the same precedence as
// resolvePermission while carrying the exact sudo row that made the decision.
// Callers writing an audit record can therefore preserve decision-time
// provenance without re-querying a grant that may expire or be replaced.
func resolvePermissionWithSudoGrantID(convID, slug string) (permResolution, int64) {
	cfg, _ := config.Load()
	return resolvePermissionWithDefault(convID, slug, cfg.HasDefaultPermission(slug))
}

func resolvePermissionForRequest(r *http.Request, convID, slug string) permResolution {
	if defaults, ok := r.Context().Value(permissionDefaultsKey{}).(map[string]bool); ok {
		resolution, _ := resolvePermissionWithDefault(convID, slug, defaults[slug])
		return resolution
	}
	return resolvePermission(convID, slug)
}

func resolvePermissionWithDefault(convID, slug string, defaultAllowed bool) (permResolution, int64) {
	if convID == "" {
		return permUndecided, 0
	}
	state, err := db.AgentState(convID)
	if err != nil || state == db.AgentStateRetired {
		return permUndecided, 0
	}
	if grantID, err := db.LookupActiveSudoGrantID(convID, slug); err == nil && grantID != 0 {
		return permAllow, grantID
	}
	if effect, ok, err := db.AgentPermissionOverride(convID, slug); err == nil && ok {
		if effect == db.PermEffectDeny {
			return permDeny, 0
		}
		return permAllow, 0
	}
	if ok, err := db.HasAgentGroupPermission(convID, slug); err == nil && ok {
		return permAllow, 0
	}
	if defaultAllowed {
		return permAllow, 0
	}
	return permUndecided, 0
}

// requirePermission gates an endpoint behind a named agent permission.
//
// The human operator (classHuman) always passes. Agents pass only when
// resolvePermission returns permAllow — an active sudo grant, a
// per-conv grant override, or the config default-permissions list. A
// per-conv deny override (or simply no granting source) leaves the
// caller to the X-Tclaude-Ask-Human popup, then a 403 with the
// permission slug in the message body. Unidentified / unconfirmed /
// unidentifiable-agent callers are refused fail-closed.
//
// Returns (convID, true) on success — convID is "" for the human path,
// the resolved conv-id for an agent. On failure the response is
// already written; the caller just returns.
func requirePermission(w http.ResponseWriter, r *http.Request, perm string) (string, bool) {
	return requirePermissionEx(w, r, perm, nil)
}

// requireGroupPermission gates a GROUP-scoped endpoint behind perm with
// the structural rule that OWNING g confers perm by default. It is
// requirePermission plus an owner-of-this-group bypass: owner-state
// raises the default group-lifecycle slugs (groups.spawn / groups.stop /
// groups.retire / groups.resume) so a lead can run its own team's
// lifecycle without an explicit grant. Consistent with the universal
// precedence — the bypass fills only the permUndecided gap, an explicit
// deny override still suppresses it, and a non-owner still needs the slug.
func requireGroupPermission(w http.ResponseWriter, r *http.Request, perm string, g *db.AgentGroup) (string, bool) {
	return requirePermissionEx(w, r, perm, func(convID string) bool {
		owns, err := db.IsAgentGroupOwner(g.ID, convID)
		return err == nil && owns
	})
}

// requirePermissionEx is the shared core of requirePermission and
// requireGroupPermission. ownerBypass, when non-nil, is consulted with
// the resolved caller conv-id ONLY when the slug is otherwise undecided
// (no grant, no deny) — a structural grant that fills the default-slug
// gap. It is deliberately NOT consulted on permDeny: a deny override is
// always authoritative and suppresses the bypass, the same precedence
// every other gate follows. ownerBypass == nil reproduces plain
// requirePermission behaviour exactly.
func requirePermissionEx(w http.ResponseWriter, r *http.Request, perm string, ownerBypass func(convID string) bool) (string, bool) {
	p := peerFromContext(r.Context())
	switch classify(p) {
	case classUnidentified:
		writeError(w, http.StatusUnauthorized, "auth",
			"could not determine peer PID; refusing to evaluate permission")
		return "", false
	case classHuman:
		// The human operator is implicitly allowed everything.
		return "", true
	case classAgentUnknown:
		writeError(w, http.StatusForbidden, "auth",
			"caller has a Claude Code ancestor but no resolvable conv-id; cannot evaluate permission")
		return "", false
	case classUnconfirmed:
		writeUnconfirmed(w, r)
		return "", false
	case classAgent:
		// Confirmed agent — fall through to the per-conv evaluation below.
	}
	title := ""
	row, _ := db.GetConvIndex(p.ConvID)
	if row != nil {
		title = agent.DisplayTitle(row)
	}
	slog.Debug("requirePermission: resolved caller",
		"conv", p.ConvID, "row_present", row != nil, "title", title, "perm", perm)
	state, err := db.AgentState(p.ConvID)
	if err != nil {
		writeError(w, http.StatusForbidden, "auth", "could not verify caller agent state")
		return "", false
	}
	if state == db.AgentStateRetired {
		writeError(w, http.StatusForbidden, "auth", "caller is a retired agent")
		return "", false
	}
	// Defaults, per-conv grant/deny overrides, and sudo grants all
	// resolve in resolvePermission. A permAllow passes; a permUndecided
	// may still pass via the structural owner bypass; permDeny is
	// authoritative and (like an undecided with no bypass) falls through
	// to the popup-or-403 path below.
	allowed := false
	if hasWriteProofApprovalContinuation(r, p.ConvID, perm, p.ConvID) ||
		hasHumanApprovalContinuation(r, perm, p.ConvID) {
		allowed = true
	} else {
		switch resolvePermissionForRequest(r, p.ConvID, perm) {
		case permAllow:
			allowed = true
		case permUndecided:
			allowed = ownerBypass != nil && ownerBypass(p.ConvID)
		case permDeny:
			// Authoritative deny — suppresses the owner bypass.
		}
	}
	if !allowed {
		// Permission denied. If the caller asked for a human-override
		// popup (via X-Tclaude-Ask-Human: <duration>), open one and
		// block on the decision. Timeout = deny, so a doomed agent can
		// never get stuck waiting forever.
		if timeout := parseAskHumanHeader(r); timeout > 0 && popupBaseURL != "" {
			// Snapshot a safe description now so the popup can show what's
			// being approved. The preview helper replaces r.Body with a
			// fresh reader so the downstream handler still gets the same
			// bytes after approval; sensitive routes provide redacted previews.
			bodyPreview := snapshotApprovalRequestBody(r, perm)
			targetGroup, targetConvID, targetConvTitle := extractApprovalTargets(r, bodyPreview)
			// For a clipboard write, show the human the exact text about to
			// be copied under a clear label — the JSON envelope would render
			// newlines as literal \n and read poorly for a snippet. The raw
			// text is still escaped when the dashboard card renders it
			// (mail.js esc() on r.body — the access-requests folder).
			bodyLabel := ""
			if raw, ok := clipboardApprovalPreview(perm, bodyPreview); ok {
				bodyPreview = raw
				bodyLabel = "Clipboard content"
			}
			observabilityPath, sensitivePath := projectSafeHTTPLogPath(r.URL.Path)
			observabilityQuery := r.URL.RawQuery
			if sensitivePath {
				observabilityQuery = ""
			}
			req := &approvalRequest{
				id:              newApprovalID(),
				perm:            perm,
				convID:          p.ConvID,
				convTitle:       title,
				method:          r.Method,
				path:            observabilityPath,
				rawQuery:        observabilityQuery,
				bodyPreview:     bodyPreview,
				bodyLabel:       bodyLabel,
				targetGroup:     targetGroup,
				targetConvID:    targetConvID,
				targetConvTitle: targetConvTitle,
				autoGrantable:   IsAutoGrantableSlug(perm),
				createdAt:       time.Now(),
				timeout:         timeout,
				decision:        make(chan approvalOutcome, 1),
				extend:          make(chan time.Duration, 1),
			}
			if requestHumanApproval(req, popupBaseURL) {
				markWriteProofHumanApproval(r, perm, p.ConvID)
				return p.ConvID, true
			}
			writeError(w, http.StatusForbidden, "permission",
				fmt.Sprintf("human declined or timed out after %s on permission %q", timeout, perm))
			return "", false
		}
		writeError(w, http.StatusForbidden, "permission",
			fmt.Sprintf("caller is not granted permission %q (grant via agent.default_permissions or agent.permission_overrides in ~/.tclaude/config.json; or call again with X-Tclaude-Ask-Human: <duration> to ask the human via popup)", perm))
		return "", false
	}
	return p.ConvID, true
}

// extractApprovalTargets parses the request URL + JSON body to surface
// the action's target group / target conv-id, so the popup can show
// the human concrete names rather than "Endpoint: PATCH /v1/groups/foo/members/abcd".
//
// Returns (groupName, targetConvID, targetConvTitle). Empty strings
// when there's nothing useful to display (e.g. /v1/whoami/rename has
// no group and no separate target — the requester is the target).
func extractApprovalTargets(r *http.Request, bodyPreview string) (group, targetConvID, targetConvTitle string) {
	const groupsPrefix = "/v1/groups/"
	if strings.HasPrefix(r.URL.Path, groupsPrefix) {
		rest := strings.TrimPrefix(r.URL.Path, groupsPrefix)
		parts := strings.SplitN(rest, "/", 3)
		if len(parts) >= 1 && parts[0] != "" {
			if g, err := url.PathUnescape(parts[0]); err == nil {
				group = g
			} else {
				group = parts[0]
			}
		}
		// /v1/groups/{name}/members/{conv} => target is parts[2].
		if len(parts) >= 3 && parts[1] == "members" && parts[2] != "" {
			selector := parts[2]
			if u, err := url.PathUnescape(selector); err == nil {
				selector = u
			}
			if res, _, err := agent.ResolveSelector(selector); err == nil {
				targetConvID = res.ConvID
				if res.Row != nil {
					targetConvTitle = agent.DisplayTitle(res.Row)
				}
			}
		}
	}
	// POST /v1/groups/{name}/members carries the target conv in the JSON
	// body's "conv" field. Parse the snapshot we already buffered.
	if targetConvID == "" && r.Method == http.MethodPost && bodyPreview != "" {
		var body struct {
			Conv string `json:"conv"`
		}
		if err := json.Unmarshal([]byte(bodyPreview), &body); err == nil && body.Conv != "" {
			if res, _, err := agent.ResolveSelector(body.Conv); err == nil {
				targetConvID = res.ConvID
				if res.Row != nil {
					targetConvTitle = agent.DisplayTitle(res.Row)
				}
			}
		}
	}
	return group, targetConvID, targetConvTitle
}

// clipboardApprovalPreview extracts the raw text a clipboard write is
// about to copy, so the approval popup can show the human the exact
// content instead of the {"text":"…"} JSON envelope (whose escaped
// newlines read poorly for a multi-line snippet). bodyPreview is the
// already-buffered, JSON-prettified request body from snapshotRequestBody.
//
// Returns ok=false for any non-clipboard perm, and also when the body
// can't be parsed as the clipboard envelope — e.g. a payload larger than
// snapshotRequestBody's preview cap, which arrives truncated and no longer
// valid JSON. In that case the caller keeps the generic JSON preview,
// which is still shown and still escaped.
func clipboardApprovalPreview(perm, bodyPreview string) (string, bool) {
	if perm != PermHumanClipboard || bodyPreview == "" {
		return "", false
	}
	var b struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(bodyPreview), &b); err != nil || b.Text == "" {
		return "", false
	}
	return b.Text, true
}

// parseAskHumanHeader reads the X-Tclaude-Ask-Human header. Empty/absent
// => 0 (no popup). Bare integers are seconds; everything else is parsed
// via time.ParseDuration. Hard cap at 300s — popups blocking longer than
// that defeat the "agents don't get stuck" goal of having a timeout in
// the first place.
func parseAskHumanHeader(r *http.Request) time.Duration {
	v := strings.TrimSpace(r.Header.Get("X-Tclaude-Ask-Human"))
	if v == "" {
		return 0
	}
	if d, err := time.ParseDuration(v); err == nil && d > 0 {
		if d > 300*time.Second {
			d = 300 * time.Second
		}
		return d
	}
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		if n > 300 {
			n = 300
		}
		return time.Duration(n) * time.Second
	}
	return 0
}

// procName / procParent are the process-tree walk primitives convIDForPID
// uses, indirected through package vars so a unit test can stand up a
// synthetic ancestor chain (e.g. a codex ancestor over a sessions row)
// without real /proc. Production points them at the session package's
// /proc readers.
var (
	procName   = session.GetProcessName
	procParent = session.GetParentPID
)

// convIDForPID walks up from pid to the nearest coding-harness ancestor —
// any harness runtime (claude, codex, …) or "node" (Claude Code runs as
// node), recognised by session.IsHarnessProcessName, so a Codex agent is
// identified the same way a Claude Code one is (JOH-206).
//
// Returns the ancestor's conv-id plus a flag indicating whether any such
// ancestor was observed at all. The conv-id is resolved, in order, from:
//
//  1. Claude Code's per-pid ~/.claude/sessions/<pid>.json at the ancestor
//     (CC writes it under its own — node — pid; Codex writes no such file).
//  2. agentd's sessions table keyed by the ancestor's own host pid — the
//     case where the pane shell exec'd into the harness, so pane_pid IS the
//     harness pid, plus any hook-corrected row keyed by FindClaudePID().
//  3. agentd's sessions table keyed by the ancestor's PARENT host pid.
//  4. For an OpenCode ancestor only, agentd's opencode_runtimes table keyed
//     by the ancestor's own pid, then its parent's pid.
//
// Step 3 is the load-bearing one for Codex (JOH-206). The spawn row is keyed
// by the tmux pane_pid (ParsePIDFromTmux at `tclaude session new`), and a
// harness launches as `sh -c "export …; <harness> …"` — a compound command
// the shell never exec-optimises — so the pane_pid is that `sh` wrapper and
// the harness runs as its direct child. The walk therefore reaches the
// harness one hop *below* the recorded pid; its parent is the pane shell the
// row is keyed by. (Verified live: a codex process was the direct child of
// the `sh` pane whose pid the session row carried, pid 205165 under 205164.)
//
// Step 4 is the OpenCode server-authoritative counterpart. The pane is an
// `opencode attach` client, while agentd launches the per-session `opencode
// serve` process and records its exact pid in opencode_runtimes. OpenCode runs
// shell tools from that server runtime, so the peer's ancestry is expected to
// reach the recorded serve pid. Probing both the matched OpenCode ancestor and
// its parent also covers an intermediate OpenCode process; the unchanged
// sessions probes above retain the attach-pane ancestry fallback.
//
// Resolution is intentionally bound to host pids the daemon itself recorded
// at spawn — facts a sandboxed caller cannot choose. It must NOT read the
// caller's process environment for a session-id, the way an earlier cut did:
// the walk matches the first harness-NAMED ancestor, and a caller controls
// both a process's name and its environment, so a renamed `codex` process
// carrying a planted TCLAUDE_SESSION_ID would impersonate any agent whose
// session-id it knows. Keying only on recorded host pids closes that.
//
// Callers use hasAncestor to distinguish "really the human" (no ancestor)
// from "agent we can't identify" (ancestor present, conv-id unresolved).
func convIDForPID(pid int) (convID string, hasAncestor bool) {
	cur := pid
	for cur > 1 {
		name := procName(cur)
		parent := procParent(cur)
		if session.IsHarnessProcessName(name) {
			hasAncestor = true
			if id := readSessionFile(cur); id != "" {
				return id, true
			}
			if id := sessionConvByPID(cur); id != "" {
				return id, true
			}
			if id := sessionConvByPID(parent); id != "" {
				return id, true
			}
			// OpenCode is server-authoritative: agentd owns `opencode serve`
			// outside the attach pane and records that process in
			// opencode_runtimes, not sessions.pid. Gate these extra probes on
			// the OpenCode binary name so Claude/Codex resolution above stays
			// byte-for-byte unchanged.
			if name == harness.OpenCodeName {
				if id := openCodeRuntimeConvByPID(cur); id != "" {
					return id, true
				}
				if id := openCodeRuntimeConvByPID(parent); id != "" {
					return id, true
				}
			}
		}
		cur = parent
	}
	return "", hasAncestor
}

// sessionConvByPID returns the conv-id of the most-recently-updated sessions
// row whose recorded host pid is hostPID, or "" when none matches (or the
// match has no conv-id yet). The sessions table is keyed by the tmux pane_pid
// recorded at spawn; convIDForPID probes both the harness ancestor's own pid
// and its parent's because the harness runs one hop below that pane_pid.
func sessionConvByPID(hostPID int) string {
	if row, err := db.FindSessionByPID(hostPID); err == nil && row != nil {
		return row.ConvID
	}
	return ""
}

// openCodeRuntimeConvByPID returns the conv-id of the freshest managed
// OpenCode runtime whose recorded `opencode serve` pid is hostPID. It is
// called only for an OpenCode-named ancestor, keeping the Claude/Codex
// sessions.pid resolution contract unchanged.
//
// The match is confirmed by endpoint ownership before it becomes an identity:
// if an `opencode serve` crashes, its runtime row lingers with a stale pid
// until reconcile/reap clears it, and a same-uid `opencode`-named process that
// inherits that pid in the meantime would otherwise resolve as the victim conv
// (→ classAgent). Requiring that the recorded pid still owns the recorded
// endpoint closes that reuse window: the crashed server freed its port, so the
// impostor cannot own it, while a live managed server always holds its own port
// — the same proof every authenticated request to this runtime already
// requires. (The parallel sessions.pid path in sessionConvByPID has the same
// pre-existing property but no endpoint to prove against; it is intentionally
// left unchanged here — see TCL-678.)
func openCodeRuntimeConvByPID(hostPID int) string {
	if runtime, err := db.FindOpenCodeRuntimeByPID(hostPID); err == nil && runtime != nil &&
		openCodeRuntimeVerified(*runtime) {
		return runtime.ConvID
	}
	return ""
}

// readSessionFile loads ~/.claude/sessions/<pid>.json and returns
// `sessionId`, or "" on any error.
func readSessionFile(pid int) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	path := filepath.Join(home, ".claude", "sessions", fmt.Sprintf("%d.json", pid))
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return ""
	}
	id, _ := m["sessionId"].(string)
	return id
}

// unixConnKey is how we smuggle the connection's *net.UnixConn into per-request
// context, since net/http hides the underlying conn from handlers. The Server's
// ConnContext hook puts it there.
type unixConnKey struct{}
