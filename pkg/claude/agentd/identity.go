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
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
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

// dashboardSpawnOriginKey is an unforgeable in-process marker added only after
// the cookie-authenticated dashboard spawn route has accepted the request. It
// distinguishes that supported UI surface from a raw /v1 request carrying an
// operator token, which also classifies as human but must not author the
// dashboard-only unenforced-sandbox override.
type dashboardSpawnOriginKey struct{}

func asDashboardSpawnPeer(r *http.Request) *http.Request {
	r = asDashboardHumanPeer(r)
	return r.WithContext(context.WithValue(r.Context(), dashboardSpawnOriginKey{}, true))
}

func isDashboardSpawnPeer(r *http.Request) bool {
	allowed, _ := r.Context().Value(dashboardSpawnOriginKey{}).(bool)
	return allowed && classify(peerFromContext(r.Context())) == classHuman
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
	PermSelfRename                        = "self.rename"
	PermSelfCompact                       = "self.compact"
	PermSelfInterrupt                     = "self.interrupt"
	PermSelfClone                         = "self.clone"
	PermSelfRemoteControl                 = "self.remote-control"
	PermSelfTask                          = "self.task"
	PermSelfPR                            = "self.pr"
	PermSelfTags                          = "self.tags"
	PermSelfDirRepair                     = "self.dir-repair"
	PermAgentReincarnate                  = "agent.reincarnate"
	PermAgentCompact                      = "agent.compact"
	PermAgentInterrupt                    = "agent.interrupt"
	PermAgentRename                       = "agent.rename"
	PermAgentRemoteControl                = "agent.remote-control"
	PermAgentClone                        = "agent.clone"
	PermAgentContextInfo                  = "agent.context-info"
	PermAgentTask                         = "agent.task"
	PermAgentPR                           = "agent.pr"
	PermAgentTags                         = "agent.tags"
	PermGroupsAdmin                       = "groups.admin"
	PermGroupsCreate                      = "groups.create"
	PermGroupsDelete                      = "groups.delete"
	PermGroupsMembersStop                 = "groups.members.stop"
	PermGroupsMembersResume               = "groups.members.resume"
	PermGroupsMembersRetire               = "groups.members.retire"
	PermGroupsMembersSpawn                = "groups.members.spawn"
	PermGroupsMembersReincarnate          = "groups.members.reincarnate"
	PermGroupsMembersCompact              = "groups.members.compact"
	PermGroupsMembersInterrupt            = "groups.members.interrupt"
	PermGroupsMembersRename               = "groups.members.rename"
	PermGroupsMembersClone                = "groups.members.clone"
	PermGroupsMembersContextInfo          = "groups.members.context-info"
	PermGroupsMembersTask                 = "groups.members.task"
	PermGroupsMembersPR                   = "groups.members.pr"
	PermGroupsMembersTags                 = "groups.members.tags"
	PermGroupsMembersSchedule             = "groups.members.schedule"
	PermGroupsMembersDelete               = "groups.members.delete"
	PermGroupsMembersPromote              = "groups.members.promote"
	PermGroupsMembersRemoteControl        = "groups.members.remote-control"
	PermGroupsMembersInboxWatch           = "groups.members.inbox-watch"
	PermGroupsOwnersManage                = "groups.owners.manage"
	PermGroupsMembersAdd                  = "groups.members.add"
	PermGroupsMembersRemove               = "groups.members.remove"
	PermGroupsMembersUpdate               = "groups.members.update"
	PermGroupsMessagesSchedule            = "groups.messages.schedule"
	PermSelfSchedule                      = "self.schedule"
	PermAgentSchedule                     = "agent.schedule"
	PermAgentStop                         = "agent.stop"
	PermAgentResume                       = "agent.resume"
	PermAgentDelete                       = "agent.delete"
	PermGroupsArchive                     = "groups.archive"
	PermGroupsNest                        = "groups.nest"
	PermAgentInboxWatch                   = "agent.inbox-watch"
	PermGroupsRename                      = "groups.rename"
	PermGroupsSettingsDescription         = "groups.settings.description"
	PermGroupsSettingsDefaultDir          = "groups.settings.default-dir"
	PermGroupsSettingsDefaultContext      = "groups.settings.default-context"
	PermGroupsSettingsDefaultSpawnTarget  = "groups.settings.default-spawn-target"
	PermGroupsSettingsDefaultProfile      = "groups.settings.default-profile"
	PermGroupsSettingsMaxMembers          = "groups.settings.max-members"
	PermGroupsSettingsNotifications       = "groups.settings.notifications"
	PermGroupsSettingsRemoteControlPolicy = "groups.settings.remote-control-policy"
	PermGroupsSettingsMemberPermissions   = "groups.settings.member-permissions"
	PermGroupsSettingsOwnerScopes         = "groups.settings.owner-scopes"
	PermGroupsAttachment                  = "groups.attachment"
	PermGroupsClone                       = "groups.clone"
	PermGroupsLinkAdd                     = "groups.link.add"
	PermGroupsLinkRemove                  = "groups.link.remove"
	PermAgentPromote                      = "agent.promote"
	PermAgentRetire                       = "agent.retire"
	PermAgentStanddown                    = "agent.standdown"
	PermMessageDirect                     = "message.direct"
	PermGroupsExport                      = "groups.export"
	PermGroupsImport                      = "groups.import"
	PermTemplatesManage                   = "templates.manage"
	PermTemplatesUse                      = "templates.instantiate"
	PermProfilesManage                    = "profiles.manage"
	// Rewriting an EXISTING agent's durable sandbox implementation. It
	// deliberately confers no owner bypass: the assignment can move an agent onto
	// an implementation with no access confinement at all, which is the boundary
	// the sandbox-lineage guard exists to protect, and a group owner is not
	// automatically an operator of the host's sandbox policy. Same reasoning as
	// sandbox-profiles.manage below, which is likewise human-only.
	PermAgentSandboxImplementation = "agent.sandbox-impl"
	// Selecting the harness-config posture for a launch — i.e. asking for the
	// read-only floor over the harness's own settings/hook/skill surface to be
	// lifted. Same reasoning as agent.sandbox-impl above: lifting it lets the
	// launched agent rewrite the policy that confines it and drop code that
	// runs in the human's next unsandboxed session, so group ownership does not
	// confer it and it is not default-granted. It gates SELECTION at spawn
	// time, never enforcement: the floor is a mount frozen at launch, and
	// sandbox lineage still refuses a child wider than its recorded parent.
	PermSandboxHarnessConfig = "sandbox.harness-config"
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
	PermTriggersRead           = "triggers.read"
	PermTriggersManage         = "triggers.manage"
	PermGroupsTriggersRead     = "groups.triggers.read"
	PermGroupsTriggersManage   = "groups.triggers.manage"
	PermHumanNotify            = "human.notify"
	PermHumanClipboard         = "human.clipboard"
	// PermSettingsDefaultModel gates writing the user-level default
	// model into ~/.claude/settings.json — a file in the human's home
	// that also carries hooks and permission config, so not
	// default-granted (effectively human-only).
	PermSettingsDefaultModel = "settings.default-model"
	// Group-route authority is deliberately split: publishing exposes a
	// publisher-owned listener, while consuming opens a lease to a peer route.
	// Neither slug is a global default and neither substitutes for membership.
	PermRoutesPublish = "routes.publish"
	PermRoutesConsume = "routes.consume"
	// The Git-remote / GitHub proxy lends agentd's OWN credentials — an SSH
	// key, a GitHub token — to an agent that has been deliberately sandboxed
	// away from them. That makes these unlike every other slug here: holding
	// one does not merely authorize a coordination action, it spends the
	// operator's identity on a remote host.
	//
	// So they are split read/write per family (a fetch and a push are not the
	// same authority), none is default-granted, none is owner-implied (owning
	// a group must not structurally confer push access to a forge), and none
	// is auto-grantable from the approval popup's "always allow" button.
	//
	// PermGitHubMerge is split off from proxy.github.write for the same reason
	// push is split off from read: opening a pull request proposes a change,
	// while merging one lands it on the base branch. An agent trusted to write
	// its own work up is not thereby trusted to decide it ships.
	PermGitRead     = "proxy.git.read"
	PermGitPush     = "proxy.git.push"
	PermGitHubRead  = "proxy.github.read"
	PermGitHubWrite = "proxy.github.write"
	PermGitHubMerge = "proxy.github.merge"
	PermLinearRead  = "proxy.linear.read"
	PermLinearWrite = "proxy.linear.write"
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
	v := resolveEffectivePermissionVerdict(convID, slug,
		cfg.HasDefaultPermission(slug), cfg.HasDefaultPermission(PermGroupsAdmin))
	return contextFreeResolution(v), v.SudoGrantID
}

// resolvePermissionVerdictForRequest is the request-scoped resolver the two
// gates use. They need the whole verdict, not just the resolution, because a
// scoped allow can only be answered against the request's ActionContext.
func resolvePermissionVerdictForRequest(r *http.Request, convID, slug string) permVerdict {
	if defaults, ok := r.Context().Value(permissionDefaultsKey{}).(map[string]bool); ok {
		return resolveEffectivePermissionVerdict(convID, slug, defaults[slug], defaults[PermGroupsAdmin])
	}
	cfg, _ := config.Load()
	return resolveEffectivePermissionVerdict(convID, slug,
		cfg.HasDefaultPermission(slug), cfg.HasDefaultPermission(PermGroupsAdmin))
}

// resolveEffectivePermissionVerdict adds the groups.admin umbrella to the
// ordinary exact-slug resolver. Exact decisions always go first: in
// particular, a dedicated deny suppresses an umbrella grant. An umbrella deny
// does not suppress a separately granted dedicated capability; it only removes
// the umbrella source.
func resolveEffectivePermissionVerdict(convID, slug string, defaultAllowed, adminDefaultAllowed bool) permVerdict {
	src := loadPermSources(convID)
	return resolveEffectivePermissionVerdictFrom(src, slug, defaultAllowed, adminDefaultAllowed)
}

func resolveEffectivePermissionVerdictFrom(src permSources, slug string, defaultAllowed, adminDefaultAllowed bool) permVerdict {
	if !IsKnownPermSlug(slug) {
		return permVerdict{Resolution: permUndecided, Source: permSourceNone}
	}
	exact := resolvePermissionVerdictFrom(src, slug, defaultAllowed)
	if exact.Resolution == permDeny || !IsGroupsAdminImpliedSlug(slug) {
		return exact
	}
	admin := resolvePermissionVerdictFrom(src, PermGroupsAdmin, adminDefaultAllowed)
	if exact.Resolution == permUndecided && admin.Resolution == permAllow {
		return admin
	}
	// A scoped dedicated allow may not cover this particular action. An
	// unscoped umbrella still does, so prefer it here; otherwise the gate would
	// stop at the unsatisfied dedicated scope and never reach groups.admin.
	if exact.Resolution == permAllow && contextFreeResolution(exact) == permUndecided &&
		admin.Resolution == permAllow && contextFreeResolution(admin) == permAllow {
		return admin
	}
	return exact
}

// contextFreeResolution collapses a verdict for a reader that cannot say what
// the action targets — every boolean sibling of the gate, and the gates
// themselves before an ActionContext exists.
//
// A SCOPED allow is not an allow to such a reader: there is nothing to
// evaluate the scope against, so it degrades to permUndecided. That is the
// same fail-closed answer requirePermissionEx gives when a gate site omits a
// dimension, applied at the one place every context-free reader passes
// through, so a scoped grant can never authorize an action nobody checked.
//
// permUndecided rather than permDeny: an unsatisfied scope means "this grant
// does not speak to this action", not "this action is forbidden". The
// structural owner bypass and the ask-human popup stay reachable exactly as
// they are for an agent holding no grant at all — narrowing the owner bypass
// itself is Phase 6.
func contextFreeResolution(v permVerdict) permResolution {
	if v.Resolution == permAllow && !evalPermissionScope(v, "", ActionContext{}).Unscoped {
		return permUndecided
	}
	return v.Resolution
}

// permSource names which of resolvePermissionVerdict's ordered sources
// actually decided a (conv, slug) question. It exists so a caller that
// must EXPLAIN a decision — the effective-permissions listing — can do so
// without re-deriving the precedence itself and drifting from the gate.
type permSource string

const (
	// permSourceNone: nothing spoke (permUndecided).
	permSourceNone permSource = ""
	// permSourceSudo: an active, time-bounded human elevation.
	permSourceSudo permSource = "sudo"
	// permSourceOverride: a per-conv grant or deny row.
	permSourceOverride permSource = "override"
	// permSourceGroup: an active group the agent belongs to grants it.
	permSourceGroup permSource = "group"
	// permSourceDefault: the config default-permissions list.
	permSourceDefault permSource = "default"
	// permSourceOwner: not decided by resolvePermissionVerdict at all —
	// the structural group-owner bypass that call sites (and the listing)
	// apply to fill the permUndecided gap.
	permSourceOwner permSource = "owner"
	// permSourceMember is a registry-declared capability conferred by active
	// membership in the action's target group.
	permSourceMember permSource = "member"
)

// permVerdict is a resolution plus the provenance behind it.
type permVerdict struct {
	Resolution  permResolution
	Source      permSource
	SudoGrantID int64
	// ScopeJSON carries the winning tier's persisted scope rows — one entry
	// per source row in that tier, "" for an unscoped row. It is what
	// evalPermissionScope evaluates against an ActionContext at the gate and
	// what permissionProvenance renders for the listing, so enforcement and
	// display read the same value.
	ScopeJSON []string
}

type sudoPermSource struct {
	ID        int64
	ScopeJSON string
}

type overridePermSource struct {
	Effect    string
	ScopeJSON string
}

// permSources is one agent's standing permission state — every source
// resolvePermissionVerdictFrom consults, read once. Gates need a single
// slug and listings need dozens, and both must reach the same precedence,
// so the sources are gathered here and the precedence is applied over
// them separately. Reading all of an agent's rows costs the same round
// trips as probing one slug, which is what lets the roster evaluate the
// whole registry per agent without a query per slug.
//
// A zero permSources (resolvable=false) answers permUndecided for every
// slug — the fail-closed shape for an empty conv-id, an unknown or
// retired agent, or an unreadable DB.
type permSources struct {
	resolvable bool
	// sudo maps slug → active grant id (soonest-expiring wins, matching
	// db.LookupActiveSudoGrantID's ORDER BY).
	sudo map[string]sudoPermSource
	// override maps slug → "grant" | "deny" (the per-conv override rows).
	override map[string]overridePermSource
	// group holds the slugs granted by the agent's active groups.
	group map[string][]string
	// groupRows retains each grant's group identity for consumers whose
	// authorization is structurally tied to one target group (routes). The
	// ordinary resolver still unions group scopes through group above.
	groupRows map[string][]db.AgentGroupPermission
}

// loadPermSources reads every standing source for convID. Read errors
// degrade to "this source said nothing", exactly as the per-slug queries
// did — except an unresolvable agent, which stays fail-closed.
func loadPermSources(convID string) permSources {
	src, _ := loadPermSourcesWithReadPolicy(convID, false)
	return src
}

// loadPermSourcesWithReadPolicy carries the route gate's stricter historical
// read-error contract without duplicating permission precedence. Central
// callers preserve the ordinary best-effort source reads; a group-bound route
// gate asks to fail closed if the override or group tier cannot be read, since
// treating either as absent could expose a lower group/default allow.
func loadPermSourcesWithReadPolicy(convID string, failOnTierReadError bool) (permSources, error) {
	if convID == "" {
		return permSources{}, nil
	}
	state, err := db.AgentState(convID)
	if err != nil {
		if failOnTierReadError {
			return permSources{}, fmt.Errorf("read agent state: %w", err)
		}
		return permSources{}, nil
	}
	if state == db.AgentStateRetired {
		return permSources{}, nil
	}
	out := permSources{
		resolvable: true,
		sudo:       map[string]sudoPermSource{},
		override:   map[string]overridePermSource{},
		group:      map[string][]string{},
		groupRows:  map[string][]db.AgentGroupPermission{},
	}
	// ListActiveSudoGrants orders by expires_at ascending, so the first
	// row for a slug is the one LookupActiveSudoGrantID would have picked.
	if grants, err := db.ListActiveSudoGrants(convID); err == nil {
		for _, g := range grants {
			if g == nil {
				continue
			}
			if _, seen := out.sudo[g.Slug]; !seen {
				out.sudo[g.Slug] = sudoPermSource{ID: g.ID, ScopeJSON: g.ScopeJSON}
			}
		}
	}
	if overrides, err := db.ListAgentPermissionOverrideRowsForConv(convID); err == nil {
		for _, row := range overrides {
			out.override[row.Slug] = overridePermSource{Effect: row.Effect, ScopeJSON: row.ScopeJSON}
		}
	} else if failOnTierReadError {
		return permSources{}, fmt.Errorf("read permission overrides: %w", err)
	}
	if grants, err := db.ListAgentGroupPermissionRowsForConv(convID); err == nil {
		for _, grant := range grants {
			out.group[grant.Slug] = append(out.group[grant.Slug], grant.ScopeJSON)
			out.groupRows[grant.Slug] = append(out.groupRows[grant.Slug], grant)
		}
	} else if failOnTierReadError {
		return permSources{}, fmt.Errorf("read group permission grants: %w", err)
	}
	return out, nil
}

// resolveGroupBoundPermissionVerdictForRequest resolves a permission through
// the same precedence as every central gate, while admitting group-tier rows
// only from targetGroupID. Routes are structurally group-bound: membership in
// and a grant from alpha must never authorize the same verb in beta. Sudo,
// per-agent overrides and defaults remain actor-wide and are unchanged.
func resolveGroupBoundPermissionVerdictForRequest(r *http.Request, convID, slug string, targetGroupID int64) (permVerdict, error) {
	src, err := loadPermSourcesWithReadPolicy(convID, true)
	if err != nil {
		return permVerdict{}, err
	}
	var targetScopes []string
	for _, grant := range src.groupRows[slug] {
		if grant.GroupID == targetGroupID {
			targetScopes = append(targetScopes, grant.ScopeJSON)
		}
	}
	if len(targetScopes) == 0 {
		delete(src.group, slug)
	} else {
		src.group[slug] = targetScopes
	}
	defaultAllowed := false
	if defaults, ok := r.Context().Value(permissionDefaultsKey{}).(map[string]bool); ok {
		defaultAllowed = defaults[slug]
	} else if cfg, _ := config.Load(); cfg != nil {
		defaultAllowed = cfg.HasDefaultPermission(slug)
	}
	return resolvePermissionVerdictFrom(src, slug, defaultAllowed), nil
}

// resolvePermissionVerdict is THE non-interactive permission resolver:
// every gate reaches it through requirePermission/requirePermissionEx,
// and the effective-permissions listing reaches it through
// effectivePermsFor. Both therefore see one implementation of the
// precedence documented on resolvePermission, and a new source added
// here shows up in the listing for free rather than silently making the
// listing wrong (an agent held human.notify via a group grant while
// `permissions ls` reported it absent).
// resolvePermissionVerdictFrom applies the precedence to already-read
// sources. This is the one place the ordering lives; a caller resolving
// many slugs for one agent loads the sources once and calls this per
// slug, paying no more queries than a single gate check.
func resolvePermissionVerdictFrom(src permSources, slug string, defaultAllowed bool) permVerdict {
	if !src.resolvable {
		return permVerdict{Resolution: permUndecided, Source: permSourceNone}
	}
	if grant, ok := src.sudo[slug]; ok && grant.ID != 0 {
		return permVerdict{Resolution: permAllow, Source: permSourceSudo, SudoGrantID: grant.ID, ScopeJSON: []string{grant.ScopeJSON}}
	}
	if override, ok := src.override[slug]; ok {
		if override.Effect == db.PermEffectDeny {
			return permVerdict{Resolution: permDeny, Source: permSourceOverride}
		}
		return permVerdict{Resolution: permAllow, Source: permSourceOverride, ScopeJSON: []string{override.ScopeJSON}}
	}
	if scopes := src.group[slug]; len(scopes) != 0 {
		return permVerdict{Resolution: permAllow, Source: permSourceGroup, ScopeJSON: scopes}
	}
	if defaultAllowed {
		return permVerdict{Resolution: permAllow, Source: permSourceDefault}
	}
	return permVerdict{Resolution: permUndecided, Source: permSourceNone}
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
//
// actx optionally describes what the request targets, for slugs whose grants
// may be scoped (§4). Omitting it is the norm and costs nothing for an
// unscoped grant; see requirePermissionEx for what a scoped grant does then.
func requirePermission(w http.ResponseWriter, r *http.Request, perm string, actx ...ActionContext) (string, bool) {
	return requirePermissionEx(w, r, perm, actx...)
}

// requireGroupPermission gates a group-scoped endpoint behind perm. Ownership
// contributes ordinary OwnerImplied grants scoped to each owned group; this
// gate identifies which group is the ownership subject. Exact denies still
// suppress the derived grant, and non-owners need another positive source.
//
// It is also the one gate that fills in an ActionContext by itself: the
// target group is its whole reason for existing, so a grant scoped to
// {"group": [...]} is evaluated here rather than waiting for a per-handler
// migration. Any caller-supplied context wins, so a handler that also knows
// the profile or template can pass a fuller one.
// Owner-grant constraints are read from g itself. An owner of a constrained
// g1 and an unconstrained g2 acting on g2 is therefore unaffected.
func requireGroupPermission(w http.ResponseWriter, r *http.Request, perm string, g *db.AgentGroup, actx ...ActionContext) (string, bool) {
	ctx := actionContextOf(actx)
	if ctx.Group == "" {
		ctx.Group = g.Name
	}
	ctx.structuralGroup = g.Name
	if isBulkGroupMemberPermission(perm) {
		ctx.bulkGroupMemberCoverage = true
	}
	return requirePermissionEx(w, r, perm, ctx)
}

func isBulkGroupMemberPermission(slug string) bool {
	switch slug {
	case PermGroupsMembersStop, PermGroupsMembersResume, PermGroupsMembersRetire:
		return true
	default:
		return false
	}
}

// permissionAllowsAction evaluates all standing sources for one slug and one
// concrete action. A scoped positive source that misses the action does not
// revoke lower positive authority: the owner/member tier may still cover it.
// An explicit deny remains authoritative and suppresses structural grants.
func permissionAllowsAction(r *http.Request, convID, perm string, actx ActionContext) (bool, string, error) {
	v := resolvePermissionVerdictForRequest(r, convID, perm)
	if !actx.bulkGroupMemberCoverage {
		allowed, matched := permissionVerdictAllowsAction(v, convID, perm, actx)
		return allowed, matched, nil
	}
	coverageSlug := perm
	if actx.alternatePermission != "" {
		base := actx
		base.bulkGroupMemberCoverage = false
		if allowed, matched := permissionVerdictAllowsAction(v, convID, perm, base); allowed {
			return true, matched, nil
		}
		coverageSlug = actx.alternatePermission
		v = resolvePermissionVerdictForRequest(r, convID, coverageSlug)
	}
	return permissionVerdictAllowsBulkGroupAction(v, convID, coverageSlug, actx)
}

func permissionVerdictAllowsBulkGroupAction(v permVerdict, convID, perm string, actx ActionContext) (bool, string, error) {
	targets := actx.affectedConvs
	if targets == nil {
		g, err := db.GetAgentGroupByName(actx.structuralGroup)
		if err != nil {
			return false, "", fmt.Errorf("resolve target group: %w", err)
		}
		if g == nil {
			return false, "", fmt.Errorf("resolve target group: group %q not found", actx.structuralGroup)
		}
		members, err := db.ListAgentGroupMembers(g.ID)
		if err != nil {
			return false, "", fmt.Errorf("resolve affected members: %w", err)
		}
		for _, member := range members {
			if perm == PermGroupsMembersRetire && sameActor(convID, member.ConvID) {
				continue
			}
			targets = append(targets, member.ConvID)
		}
	}
	if len(targets) == 0 {
		base := actx
		base.bulkGroupMemberCoverage = false
		allowed, matched := permissionVerdictAllowsAction(v, convID, perm, base)
		return allowed, matched, nil
	}
	seenGroups := map[string]bool{}
	for _, target := range targets {
		groups, err := activeGroupNamesForConvs(target)
		if err != nil {
			return false, "", fmt.Errorf("resolve affected groups: %w", err)
		}
		if len(groups) == 0 {
			return false, "", nil
		}
		targetAgent, err := db.AgentIDForConv(target)
		if err != nil {
			return false, "", fmt.Errorf("resolve affected agent: %w", err)
		}
		for _, group := range groups {
			candidate := actx
			candidate.bulkGroupMemberCoverage = false
			candidate.Group = group
			candidate.structuralGroup = group
			candidate.TargetAgent = targetAgent
			candidate.targetConv = target
			if allowed, _ := permissionVerdictAllowsAction(v, convID, perm, candidate); !allowed {
				return false, "", nil
			}
			seenGroups[group] = true
		}
	}
	groups := make([]string, 0, len(seenGroups))
	for group := range seenGroups {
		groups = append(groups, group)
	}
	sort.Strings(groups)
	return true, "group=" + strings.Join(groups, ","), nil
}

func permissionVerdictAllowsAction(v permVerdict, convID, perm string, actx ActionContext) (bool, string) {
	switch v.Resolution {
	case permAllow:
		eval := evalPermissionScope(v, convID, actx)
		if eval.Satisfied {
			return true, eval.Matched
		}
		return structuralPermissionMatch(convID, perm, actx)
	case permUndecided:
		return structuralPermissionMatch(convID, perm, actx)
	case permDeny:
		return false, ""
	default:
		return false, ""
	}
}

// loadBearingSudoGrantID returns the durable grant responsible for an action,
// but only when the same action is not already covered by standing sources
// (including owner/member-derived grants). This decision-time check retains
// scoped sudo provenance that a context-free audit re-resolution cannot see.
func loadBearingSudoGrantID(r *http.Request, convID, perm string, actx ActionContext) int64 {
	cfg, _ := config.Load()
	src := loadPermSources(convID)
	withoutSudo := src
	withoutSudo.sudo = map[string]sudoPermSource{}
	standing := resolveEffectivePermissionVerdictFrom(withoutSudo, perm,
		cfg.HasDefaultPermission(perm), cfg.HasDefaultPermission(PermGroupsAdmin))
	if allowed, _ := permissionVerdictAllowsAction(standing, convID, perm, actx); allowed {
		return 0
	}
	effective := resolvePermissionVerdictForRequest(r, convID, perm)
	if allowed, _ := permissionVerdictAllowsAction(effective, convID, perm, actx); allowed && effective.SudoGrantID > 0 {
		return effective.SudoGrantID
	}
	return 0
}

// requirePermissionEx is the shared core of requirePermission and
// requireGroupPermission. Group ownership and group membership are consulted
// from the registry with the resolved caller conv-id ONLY when the slug is
// otherwise undecided (no grant, no deny) — derived positive sources that fill
// the default-slug gap. Neither is consulted on permDeny: a deny override is
// always authoritative and suppresses them, the same precedence every other
// gate follows. Slugs not marked OwnerImplied receive no ownership source; a slug that is not
// MemberImplied, or a call with no structuralGroup, receives no member source.
//
// actx (at most one) describes what the request targets, and is evaluated
// against the winning grant's scope (§4/§6):
//
//   - Unscoped winning allow → pass, exactly as before scopes existed. This
//     is every grant that exists today, so the sites passing no context are
//     unaffected.
//   - Scoped allow the context satisfies → pass, and the matched scope is
//     recorded on the request's audit row.
//   - Scoped allow the context does not satisfy, or a scoped dimension the
//     site did not describe → that source does not decide this action. An
//     owner/member-derived grant may still cover it, then the ask-human popup,
//     then 403. Never a silent allow.
func requirePermissionEx(w http.ResponseWriter, r *http.Request, perm string, actx ...ActionContext) (string, bool) {
	// Authorization code may only name capabilities in the central registry.
	// Fail before identity resolution (including the implicit-human path), so a
	// typo can never appear as a valid denial or reach one-time approval.
	if !IsKnownPermSlug(perm) {
		slog.Error("authorization gate references unregistered permission", "permission", perm, "path", r.URL.Path)
		writeError(w, http.StatusInternalServerError, "unregistered_permission",
			fmt.Sprintf("authorization gate references unregistered permission %q", perm))
		return "", false
	}
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
	// may still pass via an owner/member-derived grant; permDeny is
	// authoritative and (like an undecided with no derived grant) falls through
	// to the popup-or-403 path below.
	allowed := false
	if hasWriteProofApprovalContinuation(r, p.ConvID, perm, p.ConvID) ||
		hasHumanApprovalContinuation(r, perm, p.ConvID) {
		// A human already approved this exact operation; the standing
		// grants (and their scopes) are not consulted at all.
		allowed = true
	} else {
		actionCtx := actionContextOf(actx)
		var matched string
		var evalErr error
		allowed, matched, evalErr = permissionAllowsAction(r, p.ConvID, perm, actionCtx)
		if evalErr != nil {
			writeError(w, http.StatusInternalServerError, "io", evalErr.Error())
			return "", false
		}
		recordAuditPermissionScope(r, perm, matched)
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
			// The scope the popup's scoped "always allow" may persist is
			// derived from the SAME context this gate evaluates grants
			// against — never re-parsed from the URL or body — so a scope the
			// human accepts is guaranteed to satisfy the next check rather
			// than leaving them clicking the same popup forever. A site that
			// describes nothing offers only the blanket button.
			scopeJSON, scopeDisplay := approvalScopeForSlug(perm, actionContextOf(actx))
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
				scopeJSON:       scopeJSON,
				scopeDisplay:    scopeDisplay,
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
	procName    = session.GetProcessName
	procParent  = session.GetParentPID
	procExeName = session.GetProcessExeName
)

// harnessNameAt returns the harness runtime name of the process at pid — the
// name the walks below key their harness-specific branches on — or "" when
// the process is not a harness runtime. name is the value the caller already
// read from procName.
//
// The executable is consulted when the name misses because a process's NAME
// is not proof of what it runs: on Linux procName reads /proc/<pid>/comm,
// the main thread's name, which the program itself may overwrite. Copilot's
// Node SEA does exactly that — a running `copilot` reports comm "MainThread"
// and its npm loader reports "node-MainThread" — so a Copilot pane had NO
// recognisable harness ancestor and every one of its callers classified as
// classUnconfirmed: no whoami, no inbox, no sends (TCL-1049). macOS never hit
// it because there the same reader falls back to `ps -o comm=`, which prints
// the executable path.
//
// This only widens WHICH ancestors are considered; it does not widen what may
// establish a conv-id. Resolution below still keys exclusively on host pids
// the daemon itself recorded at spawn. And the added evidence is a fact of the
// kernel — /proc/<pid>/exe is the binary being executed, not a settable
// string. argv[0] is deliberately NOT consulted: a process controls its own
// argv, so admitting it would let any process present itself as a harness.
func harnessNameAt(pid int, name string) string {
	if session.IsHarnessProcessName(name) {
		return name
	}
	exe := procExeName(pid)
	if exe != "" && exe != name && session.IsHarnessProcessName(exe) {
		return exe
	}
	return ""
}

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
//  4. For a recorded tclaude-layer launch, the ancestor chain above the
//     harness's parent. The bubblewrap + inner shell wrappers add hops between
//     the harness and the tmux pane PID recorded in sessions.
//  5. agentd's opencode_runtimes table keyed by each walked ancestor pid.
//     OpenCode's packaged runtime may report its process name as `bun`, so
//     the daemon-recorded pid and live endpoint proof are the authority, not
//     the executable's current display name.
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
	// Packaged OpenCode builds may expose their underlying `bun` process name
	// on macOS. Cross that name-independent ancestry only for a runtime whose
	// recorded contract is tclaude-layer; the helper retains the same bounded
	// wrapper walk and live endpoint-ownership proof as the named path below.
	if id := openCodeRuntimeConvByAncestor(pid); id != "" {
		return id, true
	}
	cur := pid
	for cur > 1 {
		name := procName(cur)
		parent := procParent(cur)
		if hname := harnessNameAt(cur, name); hname != "" {
			hasAncestor = true
			name = hname
			if id := readSessionFile(cur); id != "" {
				return id, true
			}
			if id := sessionConvByPID(cur); id != "" {
				return id, true
			}
			if id := sessionConvByPID(parent); id != "" {
				return id, true
			}
			if id := tclaudeLayerSessionConvByAncestor(parent); id != "" {
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
				if id := openCodeRuntimeConvByAncestor(parent); id != "" {
					return id, true
				}
			}
		}
		cur = parent
	}
	return "", hasAncestor
}

// tclaudeLayerSessionConvByAncestor crosses only wrapper processes between a
// harness and a sessions row that explicitly records tclaude-layer. The
// implementation check is the trust boundary: harness-builtin launches retain
// their exact/one-parent identity rule, while the outer layer can account for
// its known bwrap -> sh ancestry without trusting caller-controlled env.
func tclaudeLayerSessionConvByAncestor(pid int) string {
	const maxWrapperHops = 16
	// Same live-row preference as the row-returning twin below: a wrapped
	// agent whose pid is shadowed by a dead row would otherwise be given
	// the corpse's conv-id here too (TCL-761). The extra conv-id condition
	// stays part of the candidate test, so a row that has not established
	// one yet is skipped rather than returned as "".
	accept := func(row *db.SessionRow) bool { return row.ConvID != "" && isTclaudeLayerRow(row) }
	for range maxWrapperHops {
		if pid <= 1 {
			return ""
		}
		if row := preferLiveRowAtPID(pid, accept); row != nil {
			return row.ConvID
		}
		pid = procParent(pid)
	}
	return ""
}

// hookSessionRowForPID resolves the sessions row a brokered hook callback
// from pid belongs to, plus the harness pid the walk crossed to reach it.
//
// It is convIDForPID's walk with the row kept instead of thrown away.
// TCL-754 needs the row itself, not just its conv-id: the hook callback is
// keyed by sessions.id, and the whole point of brokering is that the
// caller's own TCLAUDE_SESSION_ID is caller-controlled compatibility state
// rather than proof of anything. Resolving the row from recorded host pids
// — facts the daemon wrote at spawn and a sandboxed caller cannot choose —
// makes the brokered path strictly stronger than the direct one.
//
// Two deliberate differences from convIDForPID:
//
//   - The ~/.claude/sessions/<pid>.json probe is skipped. It yields a
//     conv-id, not a row, and inside the layer the harness writes it into
//     a tmpfs the daemon cannot see anyway.
//   - A row with no conv-id yet still matches. A brokered SessionStart is
//     frequently the event that ESTABLISHES the conv-id, so requiring one
//     would lock out exactly the first hook of every agent. The row is
//     still identified by recorded pid, and the layer walk still requires
//     the recorded tclaude-layer implementation, so nothing about the
//     trust boundary relaxes with it.
//
// The harness pid is returned so the brokered ambient context can carry
// the same pid correction FindClaudePID performs on the direct path.
func hookSessionRowForPID(pid int) (*db.SessionRow, int) {
	cur := pid
	for cur > 1 {
		name := procName(cur)
		parent := procParent(cur)
		if harnessNameAt(cur, name) != "" {
			if row := sessionRowByPID(cur); row != nil {
				return row, cur
			}
			if row := sessionRowByPID(parent); row != nil {
				return row, cur
			}
			if row := tclaudeLayerSessionRowByAncestor(parent); row != nil {
				return row, cur
			}
		}
		cur = parent
	}
	return nil, 0
}

// claimedLivePaneSessionRow repairs broker identity when a reused pid makes
// hookSessionRowForPID return an older row from the caller's ancestry. This
// starts in the launch-only gap between tmux creating a pane and session new
// persisting that pane's pid, but it can outlive the gap: the new row records
// the pane pid while the stale row may match the nearer harness pid, causing
// the ancestry walk to return early on every later callback.
//
// The claimed id remains a cross-check, not authority. It may select a row
// only after tmux reports that row's live pane pid AND launch generation, the
// generation matches the row's daemon-owned launch identity, and the process
// walk proves that exact pid is an ancestor of the socket caller. A caller
// cannot manufacture any of those facts. The generation check is essential:
// tmux names are reusable, so pid ancestry alone could make a stale pid=0 row
// appear to own a later pane that reused its name.
//
// Restricting this to tclaude-layer rows keeps the proof on the brokered launch
// shape. The row's recorded pid is deliberately not authority here: it may be
// zero during startup, the pane pid after the parent finishes launch, or the
// harness pid after a successful hook correction. The generation-bound live
// pane and caller ancestry are the stable facts across all three states.
func claimedLivePaneSessionRow(callerPID int, claimedID string) (*db.SessionRow, int) {
	claimedID = strings.TrimSpace(claimedID)
	if callerPID <= 1 || claimedID == "" {
		return nil, 0
	}
	row, err := db.LoadSession(claimedID)
	if err != nil || row == nil || !isTclaudeLayerRow(row) || row.TmuxSession == "" {
		return nil, 0
	}
	identity, err := db.GetSessionExitLaunchIdentity(row.ID)
	if err != nil || identity.Generation == "" || identity.TmuxSession != row.TmuxSession {
		return nil, 0
	}
	pane, err := brokerLivePaneProbe(row.TmuxSession)
	if err != nil || pane.state != paneProbeLive || pane.panePID <= 1 ||
		pane.generation == "" || pane.generation != identity.Generation {
		return nil, 0
	}
	panePID := pane.panePID

	const maxAncestorHops = 256
	harnessPID := 0
	cur := callerPID
	for range maxAncestorHops {
		if cur <= 1 {
			return nil, 0
		}
		if cur == panePID {
			return row, harnessPID
		}
		if harnessPID == 0 && harnessNameAt(cur, procName(cur)) != "" {
			harnessPID = cur
		}
		cur = procParent(cur)
	}
	return nil, 0
}

// Indirected so the startup-race regression can pin the kernel/tmux proof
// without launching a real tmux server. Production uses the same bounded,
// generation-bearing probe as lifecycle mutations.
var brokerLivePaneProbe = probeLifecyclePane

// sessionRowByPID resolves the session row recorded against a host pid,
// preferring a candidate whose tmux session is still alive.
//
// The preference exists because a pid is not unique over a machine's
// lifetime: the OS reuses it, rows record the pid their pane had at
// spawn, and so a long-dead session can share a number with a live one.
// The plain "most recently updated wins" rule then answers a question
// nobody asked. For the brokered hook/statusline path (TCL-754) picking
// the dead row means a LIVE agent is refused — and the failure sustains
// itself, because that agent's row is updated mainly by the hooks now
// being refused, so its updated_at never catches up and the stale row
// keeps winning. TCL-761.
//
// Liveness is a REPAIR OF A DEMONSTRABLY DEAD WINNER, never a filter and
// never a re-ranking. The incumbent — the first candidate, exactly what
// FindSessionByPID would have returned — is displaced only when tmux
// positively reports its session gone AND some other candidate's session
// is positively reported alive. Everything else keeps the incumbent:
//
//   - the incumbent's tmux session is alive → keep it;
//   - the incumbent has no recorded tmux session at all → keep it. Absence
//     of a name is not evidence of death, and a row auto-registered
//     outside tmux legitimately has none. Ranking it below an older row
//     that merely HAS a live name would break the invariant below in the
//     one case where nothing is actually known;
//   - no other candidate is alive → keep it;
//   - the liveness probe fails, or the cache is cold and tmux is
//     unreachable → keep it, because an empty alive set proves nothing
//     about anybody.
//
// So this may resolve better than before; it can never resolve nothing
// where the old code resolved something, and it never moves off a row
// without positive evidence in both directions. Nothing here touches the
// refusal semantics at the call sites: a caller that cannot be placed is
// still refused.
func sessionRowByPID(hostPID int) *db.SessionRow {
	return preferLiveRowAtPID(hostPID, nil)
}

// preferLiveRowAtPID is the shared body of the pid → row lookup: read
// every row recorded against hostPID in FindSessionByPID's order, keep the
// ones `accept` allows, and repair the incumbent per the rule above.
//
// A nil accept keeps every non-empty row. The layer walk passes one so the
// tclaude-layer implementation check happens BEFORE the liveness
// preference: those are different questions, and applying them in the
// other order could prefer a live row the trust boundary excludes.
func preferLiveRowAtPID(hostPID int, accept func(*db.SessionRow) bool) *db.SessionRow {
	return repairedRowAtPID(hostPID, accept, nil)
}

// repairedRowAtPID is preferLiveRowAtPID with one extra knob: `replaces`
// vetoes an otherwise-eligible live replacement, deciding — against the
// incumbent it would displace — whether the swap is one this caller can
// afford. It is NOT a candidate filter: the incumbent is still whichever row
// FindSessionByPID would have returned, so a veto keeps that row rather than
// promoting anyone else. A nil `replaces` allows every live replacement,
// which is the row-returning callers' behaviour.
//
// sessionConvByPID needs it because its answer is the row's CONV-ID, not the
// row: a repair that swaps a conv-id-bearing row for one that has none (or
// the reverse) would change whether the caller resolves at all, in a
// direction liveness says nothing about.
func repairedRowAtPID(
	hostPID int,
	accept func(*db.SessionRow) bool,
	replaces func(incumbent, candidate *db.SessionRow) bool,
) *db.SessionRow {
	rows, err := db.FindSessionsByPID(hostPID)
	if err != nil || len(rows) == 0 {
		return nil
	}

	candidates := rows[:0:0]
	for _, row := range rows {
		if row == nil || row.ID == "" {
			continue
		}
		if accept != nil && !accept(row) {
			continue
		}
		candidates = append(candidates, row)
	}
	if len(candidates) == 0 {
		return nil
	}
	incumbent := candidates[0]
	if len(candidates) == 1 || incumbent.TmuxSession == "" {
		// One row means no ambiguity; a nameless incumbent means no
		// evidence. Either way there is nothing to ask tmux.
		return incumbent
	}

	// Only now is liveness worth a probe. The error is deliberately
	// discarded rather than propagated — every other consumer of this
	// cache treats a failed probe as an empty alive set, and here that
	// degrades precisely to the old behaviour.
	alive, _ := cachedLiveTmuxSessions()
	if _, ok := alive[incumbent.TmuxSession]; ok {
		return incumbent
	}
	for _, row := range candidates[1:] {
		if row.TmuxSession == "" {
			continue
		}
		if _, ok := alive[row.TmuxSession]; !ok {
			continue
		}
		if replaces != nil && !replaces(incumbent, row) {
			// Alive, but not a swap this caller can make. Keep the
			// incumbent rather than looking further down: the rest are
			// older still, and nothing about them is better evidence.
			return incumbent
		}
		return row
	}
	return incumbent
}

func isTclaudeLayerRow(row *db.SessionRow) bool {
	return row != nil && row.SandboxImplementation == string(sandboxpolicy.ImplementationTclaudeLayer)
}

// tclaudeLayerSessionRowByAncestor is tclaudeLayerSessionConvByAncestor
// returning the row. The recorded-implementation check is the same trust
// boundary: only a launch the daemon itself recorded as tclaude-layer may
// have its bwrap -> sh wrapper hops crossed.
//
// This is the walk that actually carries a WRAPPED agent's brokered hooks,
// so it needs the same live-row preference sessionRowByPID applies — a
// tclaude-layer agent is precisely the one that cannot recover on its own
// when a dead row shadows its pid (TCL-761).
func tclaudeLayerSessionRowByAncestor(pid int) *db.SessionRow {
	const maxWrapperHops = 16
	for range maxWrapperHops {
		if pid <= 1 {
			return nil
		}
		if row := preferLiveRowAtPID(pid, isTclaudeLayerRow); row != nil {
			return row
		}
		pid = procParent(pid)
	}
	return nil
}

// sessionConvByPID returns the conv-id of the sessions row recorded against
// hostPID, or "" when none matches (or the match has no conv-id yet). The
// sessions table is keyed by the tmux pane_pid recorded at spawn; convIDForPID
// probes both the harness ancestor's own pid and its parent's because the
// harness runs one hop below that pane_pid.
//
// This is the general pid -> conv-id lookup behind DIRECT CLI identity, and it
// takes the same dead-incumbent repair as sessionRowByPID (TCL-771). It was
// left on the plain most-recently-updated query by TCL-761, whose blast radius
// was telemetry; here the answer becomes peer.ConvID, which classify() turns
// into classAgent, so every authorization decision for that caller keys on it.
// A pid is not unique over a machine's lifetime, and session rows are not
// pruned, so a long-dead row can shadow a live agent's pane pid and hand the
// caller a stranger's identity.
//
// The repair semantics are sessionRowByPID's: a demonstrably dead incumbent
// is displaced only by a demonstrably live sibling, never filtered and never
// re-ranked. Ambiguity — one row, a nameless incumbent, nothing else alive, an
// unreachable tmux — keeps the incumbent.
//
// One condition is added on top, because this caller's answer is the row's
// CONV-ID rather than the row: a live sibling may displace the incumbent only
// when the two agree about HAVING a conv-id. A spawn row is written with an
// empty conv-id and stays that way until the first hook establishes one, so
// without that condition a repair could swap a conv-id-bearing dead row for a
// live row that has none — refusing a caller the old code placed — or the
// reverse, resolving one it previously declined and short-circuiting the
// stronger probes convIDForPID would otherwise have reached (the layer walk's
// recorded-implementation check, OpenCode's endpoint-ownership proof).
// Liveness says nothing about that question, so it may not decide it. With the
// condition, this resolves exactly when the old code did, and only ever
// improves WHICH conversation it names.
//
// Failing CLOSED on ambiguity was considered and rejected: multiple rows per
// pid is the NORMAL case (rows are never pruned and record the pane pid they
// had at spawn), so refusing whenever liveness is merely inconclusive — a cold
// cache, a transiently unreachable tmux, a row auto-registered outside tmux
// with no session name — would refuse legitimate live callers as a matter of
// routine. Pid reuse is also an accident rather than an attacker-choosable
// primitive: a caller cannot pick the pid the OS hands it, and the binding to
// host pids the daemon itself recorded is what a sandboxed caller cannot forge
// either way. Residual limitation: a dead incumbent with no provably live
// sibling still resolves as before.
func sessionConvByPID(hostPID int) string {
	sameAnswerability := func(incumbent, candidate *db.SessionRow) bool {
		return (incumbent.ConvID == "") == (candidate.ConvID == "")
	}
	if row := repairedRowAtPID(hostPID, nil, sameAnswerability); row != nil {
		return row.ConvID
	}
	return ""
}

// openCodeRuntimeConvByPID returns the conv-id of the freshest managed
// OpenCode runtime whose recorded server pid is hostPID. The exact
// daemon-recorded pid is deliberately checked independently of the process
// name: packaged OpenCode builds can expose their underlying `bun` runtime on
// macOS. Claude/Codex sessions.pid resolution remains unchanged.
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
	if hostPID == os.Getpid() {
		// The managed serve is a direct child of agentd, so convIDForPID's
		// parent probe can pass agentd's own pid here. agentd is never a managed
		// serve, so any runtime row matching our pid is stale/reused — and
		// subtree endpoint ownership would still match (managed serves are our
		// children), so the ownership gate alone would not reject it. Fail closed
		// rather than resolve a victim conv-id from a reused self-pid row.
		return ""
	}
	if runtime, err := db.FindOpenCodeRuntimeByPID(hostPID); err == nil && runtime != nil &&
		openCodeRuntimeVerified(*runtime) {
		return runtime.ConvID
	}
	return ""
}

// openCodeRuntimeConvByAncestor tolerates only the bounded wrapper ancestry of
// a runtime explicitly recorded as tclaude-layer. Legacy harness-builtin
// runtimes retain their exact pid/one-parent rule above. Every candidate still
// passes the endpoint-ownership proof before becoming caller identity.
func openCodeRuntimeConvByAncestor(pid int) string {
	const maxWrapperHops = 16
	for range maxWrapperHops {
		if pid <= 1 || pid == os.Getpid() {
			return ""
		}
		runtime, err := db.FindOpenCodeRuntimeByPID(pid)
		if err == nil && runtime != nil &&
			runtime.SandboxImplementation == string(sandboxpolicy.ImplementationTclaudeLayer) &&
			openCodeRuntimeVerified(*runtime) {
			return runtime.ConvID
		}
		pid = procParent(pid)
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
