package agentd

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// PermSlug describes one agent permission. Slugs are dotted strings that
// identify capabilities the daemon evaluates via requirePermission.
//
// Defaults — granted to every agent — live in ~/.tclaude/config.json
// under agent.default_permissions. Per-conv grants live in SQLite (table
// agent_permissions) and are written through the grant/revoke endpoints.
//
// OwnerImplied marks a slug that group ownership confers structurally:
// when an agent owns a group, the daemon's owner-bypass (the permUndecided
// gap-filler in requireGroupPermission / requireCrossAgentPermission /
// requireNotifyHumanPermission) lets it exercise the capability against
// that group or its members WITHOUT the slug being granted — unless an
// explicit per-conv deny override suppresses it. These structural grants
// are otherwise invisible in the permission editor, so the dashboard reads
// this flag to show owner-conferred slugs as effectively held for owners.
// The set is kept in lockstep with the bypassing call sites by
// TestPermissionRegistry_OwnerImpliedSet.
// AutoGrantable marks a slug the approval popup may persist from its
// "Always allow for this agent" button — a one-click write of an allow
// override alongside approving the pending request, so future calls skip
// the popup. It is a deliberately SMALL allowlist: only low-blast-radius,
// human-machine-surface slugs qualify (the clipboard / notify channels).
// Destructive or fleet-affecting slugs (agent.delete, groups.rm, the
// permissions.* meta-slugs) are NOT auto-grantable — persisting those from
// a single popup click is too sharp an edge; the human sets them
// deliberately via the permission editor / config. Rendered button
// visibility AND the popup's server-side persist both gate on this flag,
// so a scraped popup URL can't self-grant an ineligible slug. Kept in
// lockstep with the popup by TestPermissionRegistry_AutoGrantableSet.
type PermSlug struct {
	Slug          string `json:"slug"`
	Description   string `json:"description"`
	OwnerImplied  bool   `json:"owner_implied,omitempty"`
	AutoGrantable bool   `json:"auto_grantable,omitempty"`
}

// permissionRegistry is the single source of truth for known slugs. It's
// what `permissions slugs` returns and what the validators consult when
// they want to refuse an unknown slug. Forward-compat: the daemon stores
// any string the human writes (so a future build that wires up a new
// slug picks up grants written before that build shipped), but the CLI's
// `grant` command refuses unknown slugs to catch typos.
var permissionRegistry = []PermSlug{
	{
		Slug:        PermSelfRename,
		Description: "Rename own conversation via /rename (tclaude agent rename)",
	},
	{
		Slug:        PermSelfCompact,
		Description: "Compact own conversation via /compact (tclaude agent compact)",
	},
	{
		Slug:        PermSelfClone,
		Description: "Fork this agent into a sibling that inherits its identity; the original keeps running (tclaude agent clone)",
	},
	{
		Slug:        PermSelfTask,
		Description: "Set/clear own task-reference link — the Task column's Linear/GitHub/ticket URL (tclaude agent task set/clear). Default-granted, mirroring the self-lifecycle slugs.",
	},
	{
		Slug:        PermSelfPR,
		Description: "Present own pull request to the operator dashboard (tclaude agent present-pr). Default-granted, mirroring the self-lifecycle slugs.",
	},
	{
		Slug:        PermSelfTags,
		Description: "Set own agent tags — the short labels rendered as chips in the Description column (tclaude agent tags set/add/rm). Default-granted, mirroring the self-lifecycle slugs.",
	},
	{
		Slug:        PermSelfDirRepair,
		Description: "Recreate own recorded startup directory when it has been deleted (tclaude agent dir --repair). The path is daemon-selected and cannot be overridden. Default-granted.",
	},
	{
		Slug:         PermAgentReincarnate,
		OwnerImplied: true,
		Description:  "Reincarnate ANOTHER agent (tclaude agent reincarnate --target). Group owners can reincarnate members of groups they own without this slug.",
	},
	{
		Slug:         PermAgentCompact,
		OwnerImplied: true,
		Description:  "Compact ANOTHER agent's context window (tclaude agent compact --target). Group owners can compact members of groups they own without this slug.",
	},
	{
		Slug:         PermAgentRename,
		OwnerImplied: true,
		Description:  "Rename ANOTHER agent (tclaude agent rename --target). Group owners can rename members of groups they own without this slug.",
	},
	{
		Slug:         PermAgentClone,
		OwnerImplied: true,
		Description:  "Clone ANOTHER agent into a sibling that inherits its identity (tclaude agent clone --target). Group owners can clone members of groups they own without this slug.",
	},
	{
		Slug:         PermAgentContextInfo,
		OwnerImplied: true,
		Description:  "Read ANOTHER agent's context-window state (tclaude agent context-info --target / --group). Read-only. Group owners can read context for members of groups they own without this slug.",
	},
	{
		Slug:         PermAgentTask,
		OwnerImplied: true,
		Description:  "Set/clear ANOTHER agent's task-reference link (tclaude agent task set/clear --target). Group owners can set the task link on members of groups they own without this slug.",
	},
	{
		Slug:         PermAgentPR,
		OwnerImplied: true,
		Description:  "Present or handle ANOTHER agent's pull request (tclaude agent present-pr --target). Group owners can present PRs for members of groups they own without this slug.",
	},
	{
		Slug:         PermAgentTags,
		OwnerImplied: true,
		Description:  "Set ANOTHER agent's tags — the Description-column chip labels (tclaude agent tags set/add/rm --target). Group owners can set tags on members of groups they own without this slug.",
	},
	{
		Slug:        PermGroupsCreate,
		Description: "Create new agent groups (tclaude agent groups create)",
	},
	{
		Slug:        PermGroupsRm,
		Description: "Delete agent groups (tclaude agent groups rm)",
	},
	{
		Slug:         PermGroupsStop,
		OwnerImplied: true,
		Description:  "Stop a group's running members (tclaude agent groups stop). Group owners can stop members of groups they own without this slug.",
	},
	{
		Slug:         PermGroupsResume,
		OwnerImplied: true,
		Description:  "Resume a group's offline members (tclaude agent groups resume). Group owners can resume members of groups they own without this slug.",
	},
	{
		Slug:         PermGroupsRetire,
		OwnerImplied: true,
		Description:  "Retire (soft-delete) every other member of a group in one shot — the bulk parallel of agent.retire (tclaude agent groups retire). Demotes each member to a plain conversation: drops its group memberships and revokes its permission/sudo grants, leaving the conversation intact and reinstatable. The caller's own conv is always skipped. Group owners can retire members of groups they own without this slug; it is not in the global defaults otherwise (retiring agents an owner doesn't manage is a sensitive cleanup the human drives).",
	},
	{
		Slug:         PermGroupsSpawn,
		OwnerImplied: true,
		Description:  "Spawn a fresh CC session and add it to a group (tclaude agent spawn). Group owners can spawn into groups they own without this slug (the spawn guardrails — member cap, rate limit — still apply).",
	},
	{
		Slug:        PermGroupsOwn,
		Description: "Grant or revoke group ownership (tclaude agent groups grant-owner / revoke-owner)",
	},
	{
		Slug:        PermMemberAdd,
		Description: "Add members to a group (tclaude agent groups add)",
	},
	{
		Slug:        PermMemberRemove,
		Description: "Remove members from a group (tclaude agent groups remove)",
	},
	{
		Slug:        PermMemberRedesignate,
		Description: "Edit role/descr on existing members (tclaude agent groups update-member)",
	},
	{
		Slug:        PermPermissionsGrant,
		Description: "Grant agent permissions (tclaude agent permissions grant)",
	},
	{
		Slug:        PermPermissionsRevoke,
		Description: "Revoke agent permissions (tclaude agent permissions revoke)",
	},
	{
		Slug:        PermSelfSchedule,
		Description: "Manage own scheduled cron jobs — list / add / remove (tclaude agent cron). Default-granted, mirroring the self-lifecycle slugs.",
	},
	{
		Slug:         PermAgentSchedule,
		OwnerImplied: true,
		Description:  "Manage ANOTHER agent's scheduled cron jobs (tclaude agent cron --target). Group owners can manage cron jobs on members of groups they own without this slug.",
	},
	{
		Slug:         PermAgentStop,
		OwnerImplied: true,
		Description:  "Stop ANOTHER agent's tmux session (tclaude agent stop). Single-conv variant of groups.stop. Group owners can stop members of groups they own without this slug.",
	},
	{
		Slug:         PermAgentResume,
		OwnerImplied: true,
		Description:  "Resume ANOTHER agent into a fresh tmux session (tclaude agent resume). Single-conv variant of groups.resume. Group owners can resume members of groups they own without this slug.",
	},
	{
		Slug:        PermGroupsArchive,
		Description: "Archive (soft-delete) a group: freezes membership + ownership and hides the group from default listings, while preserving message history (tclaude agent groups archive / unarchive)",
	},
	{
		Slug:         PermGroupsNest,
		OwnerImplied: true,
		Description:  "Nest a group under another as a subgroup, or clear its parent (tclaude agent groups nest <child> --under <parent> | --none). Board-organisation only — it shapes the dashboard tree, not messaging or permissions. Group owners can re-parent groups they own without this slug.",
	},
	{
		Slug:         PermAgentDelete,
		OwnerImplied: true,
		Description:  "Permanently delete ANOTHER agent (tclaude agent delete): purges its rows in every agent / conv / session table and deletes its .jsonl. NOT default-granted; this is destructive and not undoable. Group owners can delete members of groups they own without this slug.",
	},
	{
		Slug:         PermGroupsLinkAdd,
		OwnerImplied: true,
		Description:  "Create an inter-group link enabling messages from one group to another (tclaude agent groups link add). Group owners can add outbound links FROM groups they own without this slug.",
	},
	{
		Slug:         PermGroupsLinkRm,
		OwnerImplied: true,
		Description:  "Remove an inter-group link (tclaude agent groups link rm). Group owners can remove outbound links FROM groups they own without this slug.",
	},
	{
		Slug:         PermAgentPromote,
		OwnerImplied: true,
		Description:  "Promote a plain conversation into an agent, or reinstate a retired one (tclaude agent promote / reinstate). Group owners can act on members of groups they own without this slug.",
	},
	{
		Slug:         PermAgentRetire,
		OwnerImplied: true,
		Description:  "Retire (soft-delete) an agent: revokes its group memberships and permission grants so it stops being an agent, while leaving its conversation intact and reinstatable (tclaude agent retire). Group owners can retire members of groups they own without this slug.",
	},
	{
		Slug:        PermMessageDirect,
		Description: "Send a 1:1 message to ANY agent regardless of shared-group membership — the off-group escape hatch (tclaude agent message). Intra-group messaging, owner-of-group, and via-link reach need no slug; this covers everything else. Not default-granted.",
	},
	{
		Slug:        PermGroupsExport,
		Description: "Export a whole group to a portable .zip archive — DB rows plus every member's conversation .jsonl (tclaude agent groups export). The archive holds full conversation content; not default-granted (effectively human-only).",
	},
	{
		Slug:        PermGroupsImport,
		Description: "Import a group from a .zip archive, recreating the group, its agents, permissions and conversations on this machine (tclaude agent groups import). Not default-granted (effectively human-only).",
	},
	{
		Slug:        PermTemplatesManage,
		Description: "Create, edit, delete group templates and snapshot a live group into a template (dashboard Templates tab). A template is a reusable blueprint, not a conversation snapshot. Not default-granted (effectively human-only).",
	},
	{
		Slug:        PermTemplatesUse,
		Description: "Instantiate a working group from a template — creates the group and spawns its whole agent team in one shot. Strictly more powerful than groups.spawn (a whole team at once), so not default-granted (effectively human-only).",
	},
	{
		Slug:        PermProfilesManage,
		Description: "Create, edit and delete reusable spawn profiles — named, saved bundles of the spawn-agent dialog (harness/model/effort/role/… ) that pre-fill spawns and back a group's default spawn settings (JOH-210). Reads are open; writes rewrite shared spawn config, so not default-granted (effectively human-only).",
	},
	{
		Slug:        PermSandboxProfilesManage,
		Description: "Read, create, edit, delete and assign sandbox profiles — operator policy that can add host filesystem access and launch environment to agents. This is intentionally separate from profiles.manage and is not default-granted (effectively human-only).",
	},
	{
		Slug:        PermSandboxProfilesDraft,
		Description: "Submit a server-validated sandbox-profile draft for explicit human preview and save. Cannot persist or assign profiles and cannot launch agents. Intended for dashboard-summoned sandbox scribes.",
	},
	{
		Slug:        PermRolesManage,
		Description: "Create, edit and delete roles in the role library — named, reusable bundles of a canonical role-brief, a default launch shape and a default permission set that a template roster agent references and inherits from (JOH-240). Reads are open; writes rewrite shared role defaults, so not default-granted (effectively human-only).",
	},
	{
		Slug:         PermProcessAdvance,
		OwnerImplied: true,
		Description:  "Advance a group's advisory process to the next (or a named) phase — records the transition and nudges the entering roles (JOH-242). The process is advisory (nothing is enforced). Group owners can advance their own group's process without this slug; other agents need it. Reads (the current phase) are open.",
	},
	{
		Slug:        PermProcessTemplatesRead,
		Description: "List, show, and validate process templates through tclaude agent process-templates. Read-only and installed as a default alongside the bundled process-template scribe skill; an explicit per-agent deny still wins.",
	},
	{
		Slug:        PermProcessTemplatesManage,
		Description: "Create and edit process templates through tclaude agent process-templates save. Does not execute or instantiate a process. Not default-granted; requires an explicit grant or one-shot human approval.",
	},
	{
		Slug:          PermHumanNotify,
		OwnerImplied:  true,
		AutoGrantable: true,
		Description:   "Send the human a notification via `tclaude agent notify-human` — it lands in the dashboard Messages tab. Lets a coordinating agent (the PO) reach the human outside the terminal. Group owners get this by default (a trusted coordinating role), suppressible by a deny override; otherwise not in the global defaults, so plain workers cannot spam the channel without an explicit grant.",
	},
	{
		Slug:          PermHumanClipboard,
		AutoGrantable: true,
		Description:   "Copy text to the human's system clipboard via `tclaude agent clipboard` — the daemon runs the platform copy tool (wl-copy/xclip/xsel, pbcopy, clip.exe). An agent→human-machine surface like human.notify, but NOT default-granted and NOT owner-implied: it writes to the operator's real clipboard, so it needs an explicit grant or a per-call --ask-human popup approval.",
	},
	{
		Slug:        PermSettingsDefaultModel,
		Description: "Set or clear the user-level default Claude model — the \"model\" key in ~/.claude/settings.json, which every claude launched without --model falls back to. Rewrites a config file in the human's home, so not default-granted (effectively human-only).",
	},
}

// Permission slugs for the permissions-management endpoints themselves.
// Recursive: an agent that holds permissions.grant can hand out more
// permissions to itself or others. By default no agent holds these, so
// they're effectively human-only.
const (
	PermPermissionsGrant  = "permissions.grant"
	PermPermissionsRevoke = "permissions.revoke"
)

// IsKnownPermSlug reports whether slug is registered in
// permissionRegistry. Used by the grant validator to reject typos.
func IsKnownPermSlug(slug string) bool {
	for _, p := range permissionRegistry {
		if p.Slug == slug {
			return true
		}
	}
	return false
}

// IsOwnerImpliedSlug reports whether group ownership structurally confers
// slug (see PermSlug.OwnerImplied). Used by the CLI permission listing to
// surface owner-conferred capabilities for an owner agent.
func IsOwnerImpliedSlug(slug string) bool {
	for _, p := range permissionRegistry {
		if p.Slug == slug {
			return p.OwnerImplied
		}
	}
	return false
}

// OwnerImpliedSlugs returns the sorted set of slugs group ownership
// confers structurally (PermSlug.OwnerImplied). Stable order so callers
// (CLI, tests) get deterministic output.
func OwnerImpliedSlugs() []string {
	var out []string
	for _, p := range permissionRegistry {
		if p.OwnerImplied {
			out = append(out, p.Slug)
		}
	}
	sort.Strings(out)
	return out
}

// IsAutoGrantableSlug reports whether the approval popup may persist slug
// from its "Always allow for this agent" button (see PermSlug.AutoGrantable).
// The popup gates BOTH the button's visibility and its server-side persist
// on this, so an unknown or ineligible slug can never be self-granted from
// a scraped popup URL.
func IsAutoGrantableSlug(slug string) bool {
	for _, p := range permissionRegistry {
		if p.Slug == slug {
			return p.AutoGrantable
		}
	}
	return false
}

// AutoGrantableSlugs returns the sorted set of slugs eligible for the
// popup's "always allow" persist (PermSlug.AutoGrantable). Stable order so
// callers (tests) get deterministic output.
func AutoGrantableSlugs() []string {
	var out []string
	for _, p := range permissionRegistry {
		if p.AutoGrantable {
			out = append(out, p.Slug)
		}
	}
	sort.Strings(out)
	return out
}

// permissionsState mirrors the data behind the GET /v1/permissions
// view. Defaults come from config.json (granted to all agents); grants
// come from SQLite (table agent_permissions), keyed by full conv-id,
// and ADD to defaults rather than replace them.
//
// Overrides is the full tri-state per-conv view — conv-id → slug →
// "grant" | "deny". Grants (above) is the grant-only projection of the
// same table, kept for back-compat with readers that predate deny.
//
// AgentIDs projects the stable agent_id behind each conv key in
// Grants/Overrides (conv-id → agent_id), so the CLI roster can LEAD with
// the rotation-immune id (`name (agt_xxxxxxxx)`) while the maps stay
// conv-keyed on the wire (JOH-325). Absent for a conv that doesn't (yet)
// resolve to an actor; readers fall back to the conv prefix then.
//
// Titles is the display-name projection of the same keys (conv-id →
// display title). It exists so an agent-side CLI can render the roster
// without reading ~/.tclaude/data itself — a sandboxed agent is denied
// that directory by design, so decoration has to arrive over the wire
// (TCL-611). Absent for a conv with no index row; readers render blank.
type permissionsState struct {
	Defaults  []string                     `json:"defaults"`
	Grants    map[string][]string          `json:"grants"`
	Overrides map[string]map[string]string `json:"overrides"`
	AgentIDs  map[string]string            `json:"agent_ids"`
	Titles    map[string]string            `json:"titles"`
}

// permissionsEffectiveResp is the daemon-resolved answer to
// `GET /v1/permissions?target=<selector>` — everything
// `tclaude agent permissions ls <target>` renders, with selector
// resolution and the effective/owner-implied calculation done here
// rather than in the CLI (TCL-611).
type permissionsEffectiveResp struct {
	// Resolved is the explicit contract discriminator, always true here.
	// A pre-TCL-611 daemon ignores `?target` and answers the same GET with
	// the ordinary roster (HTTP 200), which decodes into this struct as
	// all-zero — an empty effective set that would materially misstate the
	// target's authority. Clients MUST refuse a response without this flag
	// and tell the operator to restart agentd.
	Resolved bool `json:"resolved"`
	// Target echoes the selector as typed.
	Target string `json:"target"`
	// TargetKey is the resolved conv-id; empty for the "default" sentinel.
	TargetKey string `json:"target_key,omitempty"`
	// AgentID is the stable actor key behind TargetKey, for leading the
	// rendered "who" (JOH-325); empty when the conv is not an agent.
	AgentID string `json:"agent_id,omitempty"`
	// Title is the resolved conv's display title, when known.
	Title string `json:"title,omitempty"`
	// Effective is ((defaults ∪ grants ∪ owner-implied) − denies), sorted.
	Effective []string `json:"effective"`
	// Source names the matched inputs, e.g. "defaults+grants:<conv> +owner".
	Source string `json:"source"`
	// OwnerImplied is the subset of Effective contributed SOLELY by group
	// ownership, so the CLI can annotate those rows "(via ownership)".
	OwnerImplied []string `json:"owner_implied"`
}

// targetSentinelDefault is the magic target string that means "modify
// the DefaultPermissions list" rather than a per-conv override. Chosen
// so it can't collide with a real conv-id (no UUID is "default") and
// reads well in CLI invocations.
const targetSentinelDefault = "default"

// resolvedTarget is the result of normalising a permissions target into
// a storage handle. For sentinel "default" the kind is sentinel and key
// is "". For a conv selector, key is the full conv-id.
type resolvedTarget struct {
	Sentinel  bool
	Key       string // full conv-id when !Sentinel
	AgentID   string // stable agent_id behind Key, for leading the response "who" (JOH-325); "" when not an actor
	ConvTitle string // best-effort display title for the resolved conv (echoed in responses)
}

func resolveTarget(target string) (*resolvedTarget, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return nil, fmt.Errorf("target is required (use %q for the default-permissions list, or a conv selector)", targetSentinelDefault)
	}
	if target == targetSentinelDefault {
		return &resolvedTarget{Sentinel: true}, nil
	}
	res, _, err := agent.ResolveSelector(target)
	if err != nil {
		return nil, err
	}
	rt := &resolvedTarget{Key: res.ConvID, AgentID: res.AgentID}
	if res.Row != nil {
		rt.ConvTitle = agent.DisplayTitle(res.Row)
	}
	return rt, nil
}

// snapshotPermissions returns the current persisted state, joining
// config defaults with the SQLite overrides table.
func snapshotPermissions() (permissionsState, error) {
	cfg, _ := config.Load()
	out := permissionsState{
		Grants:    map[string][]string{},
		Overrides: map[string]map[string]string{},
		AgentIDs:  map[string]string{},
		Titles:    map[string]string{},
	}
	if cfg != nil && cfg.Agent != nil {
		out.Defaults = append(out.Defaults, cfg.Agent.DefaultPermissions...)
	}
	grants, err := db.ListAllAgentPermissions()
	if err != nil {
		return out, err
	}
	if grants != nil {
		out.Grants = grants
	}
	overrides, err := db.ListAllAgentPermissionOverrides()
	if err != nil {
		return out, err
	}
	if overrides != nil {
		out.Overrides = overrides
	}
	// Project the stable agent_id behind every conv key so the CLI roster
	// can lead with it (display-only — the maps stay conv-keyed). Resolve
	// each conv once; a key that doesn't map to an actor is simply absent.
	for conv := range out.Grants {
		addAgentIDProjection(out.AgentIDs, conv)
		addTitleProjection(out.Titles, conv)
	}
	for conv := range out.Overrides {
		addAgentIDProjection(out.AgentIDs, conv)
		addTitleProjection(out.Titles, conv)
	}
	return out, nil
}

// addTitleProjection records conv → display title in dst so the CLI can
// decorate the roster without its own conv_index read (TCL-611 — a
// sandboxed agent cannot open the private DB). Accepts full conv-ids and
// the prefixes that occasionally show up as grant keys, mirroring what
// the CLI used to try locally. Best-effort: an unresolvable key is simply
// left out and renders blank.
func addTitleProjection(dst map[string]string, conv string) {
	if len(conv) < 8 {
		return
	}
	if _, ok := dst[conv]; ok {
		return
	}
	if row, err := db.GetConvIndex(conv); err == nil && row != nil {
		if title := agent.DisplayTitle(row); title != "" {
			dst[conv] = title
		}
		return
	}
	if row, err := db.FindConvIndexByPrefix(conv); err == nil && row != nil {
		if title := agent.DisplayTitle(row); title != "" {
			dst[conv] = title
		}
	}
}

// addAgentIDProjection records conv → agent_id in dst, skipping convs
// already resolved or with no actor behind them. Best-effort: a lookup
// error leaves the conv out, and the reader falls back to the conv prefix.
func addAgentIDProjection(dst map[string]string, conv string) {
	if conv == "" {
		return
	}
	if _, ok := dst[conv]; ok {
		return
	}
	if agentID, err := db.AgentIDForConv(conv); err == nil && agentID != "" {
		dst[conv] = agentID
	}
}

// addDefaultPermission inserts slug into config.Agent.DefaultPermissions
// (creating the section if missing). Idempotent — slug already present
// is a no-op.
func addDefaultPermission(slug string) error {
	cfg, _ := config.Load()
	if cfg == nil {
		cfg = config.DefaultConfig()
	}
	if cfg.Agent == nil {
		cfg.Agent = &config.AgentConfig{}
	}
	for _, s := range cfg.Agent.DefaultPermissions {
		if s == slug {
			return nil
		}
	}
	cfg.Agent.DefaultPermissions = append(cfg.Agent.DefaultPermissions, slug)
	sort.Strings(cfg.Agent.DefaultPermissions)
	return config.Save(cfg)
}

// removeDefaultPermission removes slug from DefaultPermissions. No-op
// if absent. Empty list is preserved (we don't delete the agent
// section just because the list emptied — that would make subsequent
// adds noisier in the diff).
func removeDefaultPermission(slug string) error {
	cfg, _ := config.Load()
	if cfg == nil || cfg.Agent == nil {
		return nil
	}
	out := cfg.Agent.DefaultPermissions[:0]
	for _, s := range cfg.Agent.DefaultPermissions {
		if s != slug {
			out = append(out, s)
		}
	}
	cfg.Agent.DefaultPermissions = out
	return config.Save(cfg)
}

// handlePermissions dispatches GET /v1/permissions. Read-only: anyone
// (including agents with no granted slugs) can introspect the current
// state. Writing happens at /v1/permissions/grant and .../revoke.
//
// With `?target=<selector>` the daemon additionally resolves the selector
// and returns the effective view for that one target
// (permissionsEffectiveResp). That branch exists so the CLI never has to
// touch ~/.tclaude/data itself: a sandboxed agent is denied that
// directory, and the client-side fallback used to degrade into a
// conversation-index rescan, a warning storm and a raw filesystem path in
// the error (TCL-611).
func handlePermissions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method", "GET only")
		return
	}
	state, err := snapshotPermissions()
	if err != nil {
		// The underlying error can carry private DB paths; keep those
		// server-side and hand the caller a generic typed failure.
		slog.Error("permissions: snapshot failed", "error", err)
		writeError(w, http.StatusInternalServerError, "server", "permission state unavailable")
		return
	}
	if target := strings.TrimSpace(r.URL.Query().Get("target")); target != "" {
		writeEffectivePermissions(w, r, state, target)
		return
	}
	writeJSON(w, http.StatusOK, state)
}

// writeEffectivePermissions resolves target and writes the effective
// permission view for it. Unknown selectors get a concise typed
// `not_found`; ambiguous ones a typed `ambiguous` with candidates, the
// same envelope /v1/lookup and /v1/messages already use.
func writeEffectivePermissions(w http.ResponseWriter, r *http.Request, state permissionsState, target string) {
	if target == targetSentinelDefault {
		defs := append([]string{}, state.Defaults...)
		sort.Strings(defs)
		writeJSON(w, http.StatusOK, permissionsEffectiveResp{
			Resolved:     true,
			Target:       targetSentinelDefault,
			Effective:    defs,
			Source:       "defaults",
			OwnerImplied: []string{},
		})
		return
	}
	// `.` / `-` mean "the conversation invoking this command". The CLI used
	// to expand them from its own process before resolving; now that the
	// selector travels to the daemon, they must be resolved from the
	// AUTHENTICATED PEER instead — letting agent.ResolveSelector see them
	// here would inspect the daemon's own process identity and answer for
	// the wrong conversation (or nothing at all).
	selector := target
	if selector == "." || selector == "-" {
		callerConv, isHuman, ok := authedCaller(w, r)
		if !ok {
			return // authedCaller already wrote the refusal
		}
		if isHuman || callerConv == "" {
			writeError(w, http.StatusNotFound, "not_found",
				fmt.Sprintf("%q means the calling conversation; this invocation has none — pass a conv selector, or %q for the defaults list",
					target, targetSentinelDefault))
			return
		}
		selector = callerConv
	}
	res, matches, err := agent.ResolveSelector(selector)
	if errors.Is(err, agent.ErrAmbiguous) {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":      fmt.Sprintf("selector %q matches multiple conversations", target),
			"code":       "ambiguous",
			"candidates": peerEntriesFromResolved(matches),
		})
		return
	}
	if err != nil {
		// err may wrap an internal DB/filesystem error; log it with context
		// and answer with the domain fact the caller is entitled to.
		slog.Warn("permissions: selector did not resolve", "target", target, "error", err)
		writeError(w, http.StatusNotFound, "not_found",
			fmt.Sprintf("no conversation or agent matches %q", target))
		return
	}
	resp := permissionsEffectiveResp{
		Resolved:  true,
		Target:    target,
		TargetKey: res.ConvID,
		AgentID:   res.AgentID,
	}
	if res.Row != nil {
		resp.Title = agent.DisplayTitle(res.Row)
	}
	effective, ownerAdded, source := effectivePermsFor(state, res.ConvID, ownerImpliedSlugsFor(res.ConvID))
	sort.Strings(effective)
	sort.Strings(ownerAdded)
	resp.Effective = effective
	resp.OwnerImplied = ownerAdded
	resp.Source = source
	if resp.Effective == nil {
		resp.Effective = []string{}
	}
	if resp.OwnerImplied == nil {
		resp.OwnerImplied = []string{}
	}
	writeJSON(w, http.StatusOK, resp)
}

// ownerImpliedSlugsFor returns the owner-conferred slug set for convID —
// non-empty only when the conv owns at least one group. Ownership is a
// structural bypass (PermSlug.OwnerImplied), so an owner effectively holds
// these without an explicit grant. A DB error degrades to "not an owner":
// owner perms go un-annotated rather than failing the whole listing.
func ownerImpliedSlugsFor(convID string) []string {
	owned, err := db.ListGroupsOwnedBy(convID)
	if err != nil {
		slog.Warn("permissions: owned-group lookup failed", "conv", convID, "error", err)
		return nil
	}
	if len(owned) == 0 {
		return nil
	}
	return OwnerImpliedSlugs()
}

// effectivePermsFor returns the slug list the daemon would consult for
// this agent. Per-conv overrides live in SQLite keyed by full conv-id:
// a grant override ADDS a slug on top of the global defaults, a deny
// override SUBTRACTS one. Group ownership ADDS the owner-conferred slugs
// (ownerImplied, empty for a non-owner) — the structural owner-bypass,
// which a deny still suppresses. So the effective set is
// ((defaults ∪ grants ∪ owner-implied) − denies).
//
// ownerAdded reports the subset contributed SOLELY by ownership (not
// already held via defaults/grants and not denied), so the caller can
// annotate those rows "(via ownership)".
//
// The returned label names the matched sources ("defaults",
// "defaults+grants:<conv>", "+owner", with " −denies" appended when any
// deny override applies).
func effectivePermsFor(state permissionsState, convID string, ownerImplied []string) (effective, ownerAdded []string, source string) {
	effective = append([]string{}, state.Defaults...)
	source = "defaults"
	if grants, ok := state.Grants[convID]; ok && len(grants) > 0 {
		effective = mergeUniqueSlugs(effective, grants)
		source = "defaults+grants:" + convID
	}
	if len(ownerImplied) > 0 {
		held := map[string]bool{}
		for _, s := range effective {
			held[s] = true
		}
		for _, s := range ownerImplied {
			if !held[s] {
				ownerAdded = append(ownerAdded, s)
			}
		}
		if len(ownerAdded) > 0 {
			effective = mergeUniqueSlugs(effective, ownerImplied)
			source += "+owner"
		}
	}
	denied := map[string]bool{}
	for slug, effect := range state.Overrides[convID] {
		if effect == db.PermEffectDeny {
			denied[slug] = true
		}
	}
	if len(denied) > 0 {
		effective = dropDeniedSlugs(effective, denied)
		ownerAdded = dropDeniedSlugs(ownerAdded, denied)
		source += " −denies"
	}
	return effective, ownerAdded, source
}

// dropDeniedSlugs returns slugs with every denied entry removed,
// preserving order. Shared by the effective set and its owner-conferred
// projection so a deny override suppresses a slug in both — deny is
// authoritative over the owner-bypass, mirroring resolvePermission.
func dropDeniedSlugs(slugs []string, denied map[string]bool) []string {
	kept := make([]string, 0, len(slugs))
	for _, s := range slugs {
		if !denied[s] {
			kept = append(kept, s)
		}
	}
	return kept
}

// mergeUniqueSlugs appends b to a, skipping duplicates and preserving
// first-seen order.
func mergeUniqueSlugs(a, b []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, v := range a {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	for _, v := range b {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// handlePermissionsSlugs returns the registry of known slugs +
// descriptions. Open to anyone — same shape as the agent-coord skill's
// docs, just queryable.
func handlePermissionsSlugs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method", "GET only")
		return
	}
	out := make([]PermSlug, len(permissionRegistry))
	copy(out, permissionRegistry)
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	writeJSON(w, http.StatusOK, out)
}

type permissionsMutateReq struct {
	Target string `json:"target"`
	Slug   string `json:"slug"`
}

type permissionsMutateResp struct {
	Target    string   `json:"target"`
	TargetKey string   `json:"target_key,omitempty"` // resolved conv-id when target != "default"
	AgentID   string   `json:"agent_id,omitempty"`   // stable agent_id behind TargetKey, for leading the "who" (JOH-325)
	Title     string   `json:"title,omitempty"`      // display title of the resolved conv, when known
	Slug      string   `json:"slug"`
	Effect    string   `json:"effect,omitempty"` // post-mutation override effect: "grant", "deny", or "default" (cleared)
	Effective []string `json:"effective"`        // post-mutation GRANTED slug list for that target
}

func decodeMutateReq(w http.ResponseWriter, r *http.Request) (*permissionsMutateReq, bool) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
		return nil, false
	}
	var body permissionsMutateReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return nil, false
	}
	body.Target = strings.TrimSpace(body.Target)
	body.Slug = strings.TrimSpace(body.Slug)
	if body.Target == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg",
			fmt.Sprintf("target is required (use %q for the default-permissions list, or a conv selector)", targetSentinelDefault))
		return nil, false
	}
	if body.Slug == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg", "slug is required")
		return nil, false
	}
	return &body, true
}

// granterLabel describes who is granting / revoking. For humans we
// record "<human>"; for agents we use their conv-id. Logged into
// agent_permissions.granted_by for audit.
func granterLabel(granterConvID string) string {
	if granterConvID == "" {
		return "<human>"
	}
	return granterConvID
}

// auditedCaller composes the granted_by audit string for a permission-
// gated mutate, annotating sudo-elevated calls so forensic queries
// can tell normal ops apart from elevated ones.
//
// Returns:
//   - "" when callerConvID is empty (human path; sites that label
//     humans differently — e.g. granterLabel — keep doing so).
//   - "<conv>:via-sudo:grant-id=<n>" when the call only passed because
//     of an active sudo grant for perm. The grant-id ties the audit
//     string to a specific row in agent_sudo_grants, so a
//     post-incident query like "what did agent X do during grant
//     42's window?" is a single LIKE.
//   - "<conv>" otherwise — the agent had a non-sudo source for the
//     permission (a per-conv grant override or the default-permissions
//     list). Annotating those with via-sudo would be misleading.
//
// A per-conv deny override is treated like the no-non-sudo-source case:
// if the call passed at all, sudo is the only thing that could have
// allowed it, so the via-sudo annotation applies.
//
// Only used at the audit-write layer, not in the hot read path —
// re-checking config + DB here is fine.
func auditedCaller(callerConvID, perm string) string {
	if callerConvID == "" {
		return ""
	}
	effect, hasOverride, _ := db.AgentPermissionOverride(callerConvID, perm)
	if hasOverride && effect == db.PermEffectGrant {
		return callerConvID
	}
	if !hasOverride {
		cfg, _ := config.Load()
		if cfg.HasDefaultPermission(perm) {
			return callerConvID
		}
	}
	// Either an explicit deny override, or no non-sudo source at all —
	// the call could only have passed via an active sudo grant.
	grantID, err := db.LookupActiveSudoGrantID(callerConvID, perm)
	if err != nil || grantID == 0 {
		return callerConvID
	}
	return fmt.Sprintf("%s:via-sudo:grant-id=%d", callerConvID, grantID)
}

// handlePermissionsGrant adds slug to either the DefaultPermissions list
// (target=="default", in config.json) or to agent_permissions(conv_id,
// slug) in SQLite. Idempotent.
//
// Refuses unknown slugs with a 400 listing the registered ones. This
// catches typos at the CLI; if the human really wants to grant a slug
// a future build will pick up, they can hand-edit config.json (the
// daemon honours unknown slugs at evaluation time too — we just refuse
// new CLI-driven grants of them).
func handlePermissionsGrant(w http.ResponseWriter, r *http.Request) {
	granter, ok := requirePermission(w, r, PermPermissionsGrant)
	if !ok {
		return
	}
	body, ok := decodeMutateReq(w, r)
	if !ok {
		return
	}
	if !IsKnownPermSlug(body.Slug) {
		writeError(w, http.StatusBadRequest, "unknown_slug",
			fmt.Sprintf("unknown permission slug %q. Known slugs: %s. "+
				"To grant a slug a future build wires up, edit %s by hand.",
				body.Slug, strings.Join(knownSlugs(), ", "), config.ConfigPath()))
		return
	}
	target, err := resolveTarget(body.Target)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	resp := permissionsMutateResp{Target: body.Target, Slug: body.Slug, Effect: db.PermEffectGrant}
	if target.Sentinel {
		if err := addDefaultPermission(body.Slug); err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		state, _ := snapshotPermissions()
		resp.Effective = append(resp.Effective, state.Defaults...)
	} else {
		if err := db.GrantAgentPermission(target.Key, body.Slug, granterLabel(granter)); err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		resp.TargetKey = target.Key
		resp.AgentID = target.AgentID
		resp.Title = target.ConvTitle
		slugs, _ := db.ListAgentPermissionsForConv(target.Key)
		resp.Effective = slugs
	}
	writeJSON(w, http.StatusOK, resp)
}

// handlePermissionsDeny writes a per-conv DENY override into
// agent_permissions(conv_id, slug, effect='deny'). A deny suppresses an
// otherwise default-granted slug for one specific agent — the
// subtractive half of the override model that the additive grant path
// alone could not express.
//
// Unlike grant, deny rejects the "default" sentinel target: there is
// nothing below the defaults list to deny. To remove a slug for every
// agent, revoke it from the defaults list instead.
//
// Gated on permissions.grant — writing a permission override (grant or
// deny) is the same management capability; permissions.revoke only
// covers clearing an override back to the inherited default. Humans
// (and the cookie-authed dashboard) bypass.
func handlePermissionsDeny(w http.ResponseWriter, r *http.Request) {
	granter, ok := requirePermission(w, r, PermPermissionsGrant)
	if !ok {
		return
	}
	body, ok := decodeMutateReq(w, r)
	if !ok {
		return
	}
	if !IsKnownPermSlug(body.Slug) {
		writeError(w, http.StatusBadRequest, "unknown_slug",
			fmt.Sprintf("unknown permission slug %q. Known slugs: %s.",
				body.Slug, strings.Join(knownSlugs(), ", ")))
		return
	}
	if body.Target == targetSentinelDefault {
		writeError(w, http.StatusBadRequest, "invalid_arg",
			"cannot deny on the \"default\" target — deny is a per-conv override. "+
				"To remove a slug for every agent, revoke it from the defaults list instead.")
		return
	}
	target, err := resolveTarget(body.Target)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	if err := db.SetAgentPermissionOverride(target.Key, body.Slug, db.PermEffectDeny, granterLabel(granter)); err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	slugs, _ := db.ListAgentPermissionsForConv(target.Key)
	writeJSON(w, http.StatusOK, permissionsMutateResp{
		Target:    body.Target,
		TargetKey: target.Key,
		AgentID:   target.AgentID,
		Title:     target.ConvTitle,
		Slug:      body.Slug,
		Effect:    db.PermEffectDeny,
		Effective: slugs,
	})
}

// handlePermissionsRevoke removes slug from either DefaultPermissions
// (config.json) or agent_permissions for the resolved conv. For a
// per-conv target it clears whichever override (grant or deny) is
// present, returning the slug to its inherited default. Idempotent.
func handlePermissionsRevoke(w http.ResponseWriter, r *http.Request) {
	if _, ok := requirePermission(w, r, PermPermissionsRevoke); !ok {
		return
	}
	body, ok := decodeMutateReq(w, r)
	if !ok {
		return
	}
	target, err := resolveTarget(body.Target)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	resp := permissionsMutateResp{Target: body.Target, Slug: body.Slug, Effect: "default"}
	if target.Sentinel {
		if err := removeDefaultPermission(body.Slug); err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		state, _ := snapshotPermissions()
		resp.Effective = append(resp.Effective, state.Defaults...)
	} else {
		if _, err := db.RevokeAgentPermission(target.Key, body.Slug); err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		resp.TargetKey = target.Key
		resp.AgentID = target.AgentID
		resp.Title = target.ConvTitle
		slugs, _ := db.ListAgentPermissionsForConv(target.Key)
		resp.Effective = slugs
	}
	writeJSON(w, http.StatusOK, resp)
}

func knownSlugs() []string {
	out := make([]string, len(permissionRegistry))
	for i, p := range permissionRegistry {
		out[i] = p.Slug
	}
	sort.Strings(out)
	return out
}
