package agentd

import (
	"encoding/json"
	"fmt"
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
type PermSlug struct {
	Slug        string `json:"slug"`
	Description string `json:"description"`
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
		Slug:        PermSelfReincarnate,
		Description: "Replace this agent with a fresh successor that inherits its identity (tclaude agent reincarnate)",
	},
	{
		Slug:        PermSelfClone,
		Description: "Fork this agent into a sibling that inherits its identity; the original keeps running (tclaude agent clone)",
	},
	{
		Slug:        PermAgentReincarnate,
		Description: "Reincarnate ANOTHER agent (tclaude agent reincarnate --target). Group owners can reincarnate members of groups they own without this slug.",
	},
	{
		Slug:        PermAgentCompact,
		Description: "Compact ANOTHER agent's context window (tclaude agent compact --target). Group owners can compact members of groups they own without this slug.",
	},
	{
		Slug:        PermAgentRename,
		Description: "Rename ANOTHER agent (tclaude agent rename --target). Group owners can rename members of groups they own without this slug.",
	},
	{
		Slug:        PermAgentClone,
		Description: "Clone ANOTHER agent into a sibling that inherits its identity (tclaude agent clone --target). Group owners can clone members of groups they own without this slug.",
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
		Slug:        PermGroupsStop,
		Description: "Stop a group's running members (tclaude agent groups stop)",
	},
	{
		Slug:        PermGroupsResume,
		Description: "Resume a group's offline members (tclaude agent groups resume)",
	},
	{
		Slug:        PermGroupsSpawn,
		Description: "Spawn a fresh CC session and add it to a group (tclaude agent spawn)",
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
		Description: "Edit alias/role/descr on existing members (tclaude agent groups update-member)",
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
		Slug:        PermAgentSchedule,
		Description: "Manage ANOTHER agent's scheduled cron jobs (tclaude agent cron --target). Group owners can manage cron jobs on members of groups they own without this slug.",
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

// permissionsState mirrors the data behind the GET /v1/permissions
// view. Defaults come from config.json (granted to all agents); grants
// come from SQLite (table agent_permissions), keyed by full conv-id,
// and ADD to defaults rather than replace them.
type permissionsState struct {
	Defaults []string            `json:"defaults"`
	Grants   map[string][]string `json:"grants"`
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
	rt := &resolvedTarget{Key: res.ConvID}
	if res.Row != nil {
		rt.ConvTitle = agent.DisplayTitle(res.Row)
	}
	return rt, nil
}

// snapshotPermissions returns the current persisted state, joining
// config defaults with the SQLite overrides table.
func snapshotPermissions() (permissionsState, error) {
	cfg, _ := config.Load()
	out := permissionsState{Grants: map[string][]string{}}
	if cfg != nil && cfg.Agent != nil {
		out.Defaults = append(out.Defaults, cfg.Agent.DefaultPermissions...)
	}
	grants, err := db.ListAllAgentPermissions()
	if err != nil {
		return out, err
	}
	out.Grants = grants
	if out.Grants == nil {
		out.Grants = map[string][]string{}
	}
	return out, nil
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
func handlePermissions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method", "GET only")
		return
	}
	state, err := snapshotPermissions()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, state)
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
	Title     string   `json:"title,omitempty"`      // display title of the resolved conv, when known
	Slug      string   `json:"slug"`
	Effective []string `json:"effective"` // post-mutation slug list for that target
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
	resp := permissionsMutateResp{Target: body.Target, Slug: body.Slug}
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
		resp.Title = target.ConvTitle
		slugs, _ := db.ListAgentPermissionsForConv(target.Key)
		resp.Effective = slugs
	}
	writeJSON(w, http.StatusOK, resp)
}

// handlePermissionsRevoke removes slug from either DefaultPermissions
// (config.json) or agent_permissions for the resolved conv. Idempotent.
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
	resp := permissionsMutateResp{Target: body.Target, Slug: body.Slug}
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
