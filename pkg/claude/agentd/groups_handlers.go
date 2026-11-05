package agentd

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

func groupByID(id int64) (*db.AgentGroup, error) {
	groups, err := db.ListAgentGroups()
	if err != nil {
		return nil, err
	}
	for _, g := range groups {
		if g.ID == id {
			return g, nil
		}
	}
	return nil, nil
}

// --- /v1/groups (GET = anyone, POST = human only) ---

type groupSummary struct {
	Name                    string `json:"name"`
	Descr                   string `json:"descr,omitempty"`
	AttachmentURL           string `json:"attachment_url,omitempty"`
	AttachmentLabel         string `json:"attachment_label,omitempty"`
	AttachmentLabelOverride string `json:"attachment_label_override,omitempty"`
	Members                 int    `json:"members"`
	Online                  int    `json:"online"`
	// MaxMembers is the group's hard member cap (agent_groups.max_members);
	// 0 = unlimited. A spawn that would exceed it is refused.
	MaxMembers int `json:"max_members,omitempty"`
	// DefaultProfile is the name of the spawn profile whose launch fields fill
	// blank spawn fields for this group's agents (JOH-210); "" = none. It is
	// the spawn default's single source — the vestigial per-group
	// default_model was dropped (JOH-220).
	DefaultProfile string `json:"default_profile,omitempty"`
	// DefaultPrompts names the saved prompts composed into a spawn's initial
	// message when neither the spawn nor its named profile names any.
	DefaultPrompts []string `json:"default_prompts,omitempty"`
	// DefaultCwd is the working directory blank spawns into this group fall
	// back to (agent_groups.default_cwd); "" = none. It is a path on the
	// operator's own filesystem and this listing is readable by every local
	// caller, so it is served only to a human one — see handleGroups.
	DefaultCwd  string                           `json:"default_cwd,omitempty"`
	Environment []sandboxpolicy.EnvironmentEntry `json:"environment,omitempty"`
	// DefaultSpawnGroup is the operator-selected tie-breaker used only when
	// directory auto-join matches more than one active group.
	DefaultSpawnGroup bool     `json:"default_spawn_group,omitempty"`
	Permissions       []string `json:"permissions,omitempty"`
	Archived          bool     `json:"archived,omitempty"`
	// NotifyMuted flags a group whose OS notifications are switched
	// off (agent_groups.notify_enabled = false). omitempty: only the
	// exceptional muted state is serialized.
	NotifyMuted bool `json:"notify_muted,omitempty"`
	// RemoteControlPolicy is the group's remote-control policy (JOH-262):
	// "inherit" (defer to the spawn profile), "optin" (force Remote Access on)
	// or "deny" (force it off). Always serialized (the canonical token) so a
	// consumer never has to guess between "absent" and "inherit".
	RemoteControlPolicy string `json:"remote_control_policy"`
	// ReinjectAfterCompact is what tclaude re-injects into member agents after
	// a compaction or /clear: "contexts" (the default), "identity" or "off".
	// Always serialized as the effective token.
	ReinjectAfterCompact string `json:"reinject_after_compact"`
	// Mission and SourceTemplate are the deploy provenance (JOH-245) carried
	// on the group row. They are what tells a plain group apart from a deployed
	// task force: `tclaude agent task-force ls` (JOH-346) filters on a
	// non-empty SourceTemplate and shows the truncated mission. omitempty — a
	// hand-built group carries neither, and plain `groups ls` ignores them.
	Mission        string `json:"mission,omitempty"`
	SourceTemplate string `json:"source_template,omitempty"`
	// OwnerScopes narrows the group's STRUCTURAL owner-implied permission
	// bypass (TCL-1071): slug → the scope an owner's bypass is confined to
	// here. omitempty — the overwhelming majority of groups impose no
	// narrowing, and an absent field reads as the historical unrestricted
	// bypass exactly as an older daemon's response did.
	OwnerScopes map[string]PermissionScope `json:"owner_scopes,omitempty"`
}

// isConvOnline reports whether any tmux session registered for this conv-id
// is currently alive. Same alive-check the queue delivery path uses.
//
// Single-target callers (delivery, lifecycle, reaper) use this — one
// conv at a time, one `tmux has-session` subprocess at most. For
// snapshot-shaped callers iterating many convs, use isConvOnlineIn
// with a pre-fetched alive set: O(N) subprocesses collapses to one.
func isConvOnline(convID string) bool {
	candidates, err := db.FindSessionsByConvID(convID)
	if err != nil {
		return false
	}
	for _, c := range candidates {
		if c.TmuxSession != "" && session.IsTmuxSessionAlive(c.TmuxSession) {
			return true
		}
	}
	return false
}

// isConvOnlineIn is the snapshot-shaped variant of isConvOnline. It
// takes a pre-fetched alive set (e.g. from clcommon.Default.ListSessions
// at the top of an HTTP handler) and does the per-row check via map
// lookup instead of per-row subprocess. Callers MUST fetch the set
// once per request and pass the SAME map to every call — passing nil
// (or fetching afresh per call) defeats the purpose and is the worst
// of both worlds.
func isConvOnlineIn(convID string, alive map[string]struct{}) bool {
	candidates, err := db.FindSessionsByConvID(convID)
	if err != nil {
		return false
	}
	return isConvOnlineInSessions(candidates, alive)
}

// isConvOnlineInSessions is isConvOnlineIn over an already-fetched session
// slice (most-recent-first, as FindSessionsByConvID returns). The dashboard
// snapshot's per-request batch loader (TCL-368) resolves liveness through it so
// a conv's rows are read once for the whole poll instead of per surface.
func isConvOnlineInSessions(candidates []*db.SessionRow, alive map[string]struct{}) bool {
	for _, c := range candidates {
		if c.TmuxSession == "" {
			continue
		}
		if _, ok := alive[c.TmuxSession]; ok {
			return true
		}
	}
	return false
}

func handleGroups(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		// Anyone (token or not) can list groups. By default archived
		// groups are filtered out — they're soft-deleted and rarely
		// belong in a default listing. Pass `?archived=1` (any non-empty
		// truthy value) to include them; the CLI surfaces this via
		// `groups ls --archived`.
		groups, err := db.ListAgentGroups()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		showArchived := isTruthy(r.URL.Query().Get("archived"))
		// A group's default_cwd is a directory on the human's own filesystem,
		// outside any agent's sandbox, and this listing is the one /v1 read
		// that asks nothing of its caller. Serving the path to an agent would
		// hand it filesystem layout it has no way to guess (the terminal
		// console gates its Tab-completion for exactly this reason), so it
		// goes to a human caller only — who configured it and reads it back
		// in the dashboard and `groups ls` anyway. The directory also goes to a
		// fleet reader, whose setup help starts from it; the environment, which
		// can carry private values, stays with the human.
		caller := peerFromContext(r.Context())
		human := classify(caller) == classHuman
		showDirs := human || (caller != nil && readsWholeFleet(r, caller.ConvID))
		// One tmux ls for the listing — per-member online checks below
		// are map lookups against this snapshot.
		aliveSessions, _ := cachedLiveTmuxSessions()
		out := make([]groupSummary, 0, len(groups))
		for _, g := range groups {
			if !showArchived && g.IsArchived() {
				continue
			}
			members, _ := db.ListAgentGroupMembers(g.ID)
			groupPermissions, _ := db.ListAgentGroupPermissions(g.ID)
			online := 0
			for _, m := range members {
				if isConvOnlineIn(m.ConvID, aliveSessions) {
					online++
				}
			}
			attachment := groupAttachmentViewFor(g)
			defaultCwd := ""
			var environment []sandboxpolicy.EnvironmentEntry
			if showDirs {
				defaultCwd = g.DefaultCwd
			}
			if human {
				environment = append([]sandboxpolicy.EnvironmentEntry(nil), g.Environment...)
			}
			out = append(out, groupSummary{
				Name:                    g.Name,
				Descr:                   g.Descr,
				AttachmentURL:           attachment.URL,
				AttachmentLabel:         attachment.Label,
				AttachmentLabelOverride: attachment.LabelOverride,
				Members:                 len(members),
				Online:                  online,
				MaxMembers:              g.MaxMembers,
				DefaultProfile:          g.DefaultProfile,
				DefaultPrompts:          g.DefaultPrompts,
				DefaultCwd:              defaultCwd,
				Environment:             environment,
				DefaultSpawnGroup:       g.DefaultSpawnGroup,
				Permissions:             groupPermissions,
				Archived:                g.IsArchived(),
				NotifyMuted:             !g.NotifyEnabled,
				RemoteControlPolicy:     remoteControlPolicyToWire(g.RemoteControl),
				ReinjectAfterCompact:    g.EffectiveReinjectAfterCompact(),
				OwnerScopes:             ownerScopesWireOmitEmpty(g.OwnerScopesJSON),
				Mission:                 g.Mission,
				SourceTemplate:          g.SourceTemplate,
			})
		}
		writeJSON(w, http.StatusOK, out)
	case http.MethodPost:
		creator, ok := requirePermission(w, r, PermGroupsCreate)
		if !ok {
			return
		}
		var body struct {
			Name            string                            `json:"name"`
			Parent          string                            `json:"parent,omitempty"`
			Descr           string                            `json:"descr,omitempty"`
			DefaultCwd      string                            `json:"default_cwd,omitempty"`
			DefaultContext  string                            `json:"default_context,omitempty"`
			Environment     *[]sandboxpolicy.EnvironmentEntry `json:"environment,omitempty"`
			AttachmentURL   string                            `json:"attachment_url,omitempty"`
			AttachmentLabel string                            `json:"attachment_label,omitempty"`
			// DefaultProfile names the spawn profile (JOH-210) whose launch
			// fields fill blank spawn fields for this group's agents. "" = none.
			DefaultProfile string `json:"default_profile,omitempty"`
			// MaxMembers is the group's hard member cap; 0 = unlimited.
			// A negative value is clamped to 0 by db.SetAgentGroupMaxMembers.
			MaxMembers int `json:"max_members,omitempty"`
			// RemoteControlPolicy is the group's remote-control policy that
			// overrides a spawn profile's remote-control default (JOH-262):
			// "" / "inherit" (defer to the profile), "optin" (force on), "deny"
			// (force off).
			RemoteControlPolicy string `json:"remote_control_policy,omitempty"`
			// RepositoryClone is a dashboard-human-only convenience: clone a
			// GitHub repository before creating the group, use that checkout as
			// default_cwd, and optionally expose its web URL as the group attachment.
			RepositoryClone *groupRepositoryClone `json:"repository_clone,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
			return
		}
		// validateGroupName is the same guard rename and clone already
		// apply — it rejects empty names, embedded slashes (which would
		// break the /v1/groups/{name}/... and /api/groups/{name}/...
		// route dispatch), control characters, and stray whitespace.
		// Create historically skipped it, which let a slash-named group
		// through and made every later sub-route on that group
		// unroutable.
		if err := validateGroupName(body.Name); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
			return
		}
		if body.RepositoryClone != nil && creator != "" {
			writeError(w, http.StatusForbidden, "human_required", "repository cloning during group creation is available only to the dashboard human")
			return
		}
		repositoryPlan, err := prepareGroupRepositoryClone(body.RepositoryClone)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_repository", err.Error())
			return
		}
		// Validate the optional default cwd + startup context up front,
		// before any DB write, so a bad value fails cleanly without
		// leaving a half-configured group behind.
		groupCwd, err := resolveGroupDefaultCwd(body.DefaultCwd)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_cwd", err.Error())
			return
		}
		if repositoryPlan != nil {
			if groupCwd != "" && groupCwd != repositoryPlan.Destination {
				writeError(w, http.StatusBadRequest, "invalid_cwd", "default_cwd must match the repository clone destination")
				return
			}
			groupCwd = repositoryPlan.Destination
		}
		// A rename could give a group made this way a pinned group's name.
		if db.IsExternalStewardConv(creator) && (groupCwd != "" || body.Environment != nil) {
			writeError(w, http.StatusForbidden, externalStewardCeilingCode,
				"an external steward can't set a group's directory or environment; those decide how its workers start")
			return
		}
		groupContext, err := normalizeGroupContext(body.DefaultContext)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
			return
		}
		var groupEnvironment []sandboxpolicy.EnvironmentEntry
		if body.Environment != nil {
			groupEnvironment, err = sandboxpolicy.NormalizeEnvironment(*body.Environment)
			if err != nil {
				writeError(w, http.StatusBadRequest, "invalid_environment", err.Error())
				return
			}
		}
		attachmentURL, attachmentLabel, err := normalizeGroupAttachment(body.AttachmentURL, body.AttachmentLabel)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_attachment", err.Error())
			return
		}
		// The default profile (JOH-210) replaces the Claude-only default_model
		// gate: validate only that the referenced profile exists — its launch
		// fields were already validated against their own harness at save, so a
		// group can default its team onto a Codex profile without the
		// Claude-only model validator rejecting it (#343). "" = no default.
		groupProfile, err := resolveGroupDefaultProfileName(body.DefaultProfile)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_profile", err.Error())
			return
		}
		if fail := defaultProfileConferralFailure(r, creator, body.Name, groupProfile); fail != nil {
			writeError(w, fail.Status, fail.Kind, fail.Msg)
			return
		}
		// Validate the optional remote-control policy up front (JOH-262); a bad
		// token fails cleanly before any DB write. "" / "inherit" → nil (no
		// override, the column default).
		groupRemoteControl, err := parseRemoteControlPolicy(body.RemoteControlPolicy)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_remote_control_policy", err.Error())
			return
		}
		// Fold newlines out of the description on the create path too,
		// not just update — the one-line header invariant must hold
		// however the descr first arrives (see normalizeGroupDescr).
		groupDescr := normalizeGroupDescr(body.Descr)
		if existing, _ := db.GetAgentGroupByName(body.Name); existing != nil {
			writeError(w, http.StatusConflict, "exists", "group already exists")
			return
		}
		id, err := db.CreateAgentGroupWithParentAndAttachment(
			body.Name, groupDescr, body.Parent, attachmentURL, attachmentLabel)
		if errors.Is(err, db.ErrGroupParentNotFound) {
			writeError(w, http.StatusNotFound, "not_found", err.Error())
			return
		}
		if errors.Is(err, db.ErrGroupParentCycle) {
			writeError(w, http.StatusConflict, "cycle", err.Error())
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		// The group row reserves its unique name and validates its parent before
		// host-side cloning begins. It is still empty here, so a clone failure can
		// safely roll it back and leave the dialog retryable without an orphaned
		// group or a successful checkout detached from a failed DB insert.
		if err := cloneGroupRepository(repositoryPlan); err != nil {
			if deleteErr := db.DeleteAgentGroup(body.Name); deleteErr != nil {
				slog.Error("groups create: clone failed and empty group rollback failed",
					"group", body.Name, "error", deleteErr)
			}
			writeError(w, http.StatusBadGateway, "clone_failed", err.Error())
			return
		}
		// Apply the default cwd + startup context as post-create updates
		// — keeps CreateAgentGroup's signature untouched (it's shared with
		// the clone path and flow-test helpers). A failure here is logged
		// but doesn't unwind the create; the human can set it later via
		// `groups set-context` or the dashboard.
		if groupCwd != "" {
			if _, err := db.SetAgentGroupDefaultCwd(body.Name, groupCwd); err != nil {
				slog.Warn("groups create: failed to set default cwd",
					"group", body.Name, "error", err)
			}
		}
		if repositoryPlan != nil && body.RepositoryClone.Attach {
			if _, err := db.SetAgentGroupAttachment(body.Name, repositoryPlan.WebURL, repositoryPlan.Label); err != nil {
				slog.Warn("groups create: failed to set repository attachment",
					"group", body.Name, "error", err)
			}
		}
		if groupContext != "" {
			if _, err := db.SetAgentGroupDefaultContext(body.Name, groupContext); err != nil {
				slog.Warn("groups create: failed to set default context",
					"group", body.Name, "error", err)
			}
		}
		if body.Environment != nil {
			if _, err := db.SetAgentGroupEnvironment(body.Name, groupEnvironment); err != nil {
				slog.Warn("groups create: failed to set environment",
					"group", body.Name, "error", err)
			}
		}
		if groupProfile != "" {
			if _, err := db.SetAgentGroupDefaultProfile(body.Name, groupProfile); err != nil {
				slog.Warn("groups create: failed to set default profile",
					"group", body.Name, "error", err)
			}
		}
		if body.MaxMembers != 0 {
			if _, err := db.SetAgentGroupMaxMembers(body.Name, body.MaxMembers); err != nil {
				slog.Warn("groups create: failed to set max members",
					"group", body.Name, "error", err)
			}
		}
		// Only write a non-inherit policy; "inherit" (nil) is the column default,
		// so a create that didn't ask for a policy leaves the group deferring to
		// the profile (JOH-262).
		if groupRemoteControl != nil {
			if _, err := db.SetAgentGroupRemoteControl(body.Name, groupRemoteControl); err != nil {
				slog.Warn("groups create: failed to set remote-control policy",
					"group", body.Name, "error", err)
			}
		}
		// Auto-grant ownership to the creator. Skipped for the human
		// path (creator == "") since humans don't have a conv-id; the
		// human is implicitly above the permission system anyway.
		// Failure here is logged but doesn't unwind the create — the
		// human can grant ownership manually if needed.
		if creator != "" {
			if err := db.AddAgentGroupOwner(id, creator, auditedCaller(creator, PermGroupsCreate)); err != nil {
				slog.Warn("groups create: auto-grant owner failed",
					"group", body.Name, "creator", creator, "error", err)
			}
		}
		writeJSON(w, http.StatusCreated, map[string]any{
			"id": id, "name": body.Name, "parent": strings.TrimSpace(body.Parent),
		})
	default:
		writeError(w, http.StatusMethodNotAllowed, "method", "GET or POST")
	}
}

// --- /v1/groups/{name}* routes ---

// registerV1GroupRoutes wires the SO_PEERCRED-authed /v1/groups/{name}
// endpoints onto the daemon mux as Go 1.22 method+pattern routes:
//
//	POST   /v1/groups/{name}/stop            → stop every member
//	POST   /v1/groups/{name}/resume          → resume every member
//	POST   /v1/groups/{name}/retire          → retire every other member
//	POST   /v1/groups/{name}/spawn           → spawn a session into the group
//	POST   /v1/groups/{name}/archive         → soft-delete (archive)
//	POST   /v1/groups/{name}/unarchive       → restore an archived group
//	PUT    /v1/groups/{name}/parent          → nest under another group (body: {parent}); "" clears
//	POST   /v1/groups/{name}/rename          → rename (body: {new_name})
//	POST   /v1/groups/{name}/clone           → clone the group
//	GET    /v1/groups/{name}/owners          → list owners
//	POST   /v1/groups/{name}/owners          → grant owner
//	DELETE /v1/groups/{name}/owners/{conv}   → revoke owner
//	GET    /v1/groups/{name}/context         → list members' context-window state
//	GET    /v1/groups/{name}/members         → list members
//	POST   /v1/groups/{name}/members         → add member
//	PATCH  /v1/groups/{name}/members/{conv}  → update role/descr
//	DELETE /v1/groups/{name}/members/{conv}  → remove member
//	POST   /v1/groups/{name}/members/{conv}/move → move member to another group
//	       /v1/groups/{name}/links[/{id}]    → link CRUD (own method dispatch)
//	GET/POST /v1/groups/{name}/attachment    → persistent group reference
//	PATCH  /v1/groups/{name}                 → update settings
//	DELETE /v1/groups/{name}                 → delete group
//
// The {name} / {conv} / {id} wildcards are matched and percent-decoded
// by the mux (read via r.PathValue), replacing the old hand-rolled
// TrimPrefix + SplitN dispatch. That manual parse split on r.URL.Path
// — already percent-decoded — so a group name with an embedded slash
// was re-split into bogus path segments and the route was lost. A
// {name} wildcard matches one segment of the *escaped* path, so the
// slash survives intact.
func registerV1GroupRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/groups/{name}/stop", v1GroupRoute(handleGroupStop))
	mux.HandleFunc("POST /v1/groups/{name}/resume", v1GroupRoute(handleGroupResume))
	mux.HandleFunc("POST /v1/groups/{name}/retire", v1GroupRoute(handleGroupRetire))
	mux.HandleFunc("POST /v1/groups/{name}/spawn", v1GroupRoute(handleGroupSpawnAPI))
	mux.HandleFunc("GET /v1/spawn-queue", handleSpawnQueueList)
	mux.HandleFunc("DELETE /v1/spawn-queue/{id}", handleSpawnQueueCancel)
	mux.HandleFunc("POST /v1/groups/{name}/archive", v1GroupRoute(handleGroupArchive))
	mux.HandleFunc("POST /v1/groups/{name}/unarchive", v1GroupRoute(handleGroupUnarchive))
	mux.HandleFunc("PUT /v1/groups/{name}/parent", v1GroupRoute(handleGroupParent))
	mux.HandleFunc("POST /v1/groups/{name}/rename", v1GroupRoute(handleGroupRename))
	mux.HandleFunc("POST /v1/groups/{name}/clone", v1GroupRoute(handleGroupClone))
	mux.HandleFunc("GET /v1/groups/{name}/export", v1GroupRoute(handleGroupExport))

	mux.HandleFunc("GET /v1/groups/{name}/owners", v1GroupRoute(handleGroupOwnersList))
	mux.HandleFunc("POST /v1/groups/{name}/owners", v1GroupRoute(handleGroupOwnersAdd))
	mux.HandleFunc("DELETE /v1/groups/{name}/owners/{conv}", v1GroupRoute(func(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
		handleGroupOwnersRemove(w, r, g, r.PathValue("conv"))
	}))

	mux.HandleFunc("GET /v1/groups/{name}/context", v1GroupRoute(handleGroupContext))

	// Advisory process runtime (JOH-242): read the current phase (open),
	// advance to the next / a named phase (gated on process.advance, owner-pass).
	mux.HandleFunc("GET /v1/groups/{name}/process", v1GroupRoute(handleGroupProcessGet))
	mux.HandleFunc("POST /v1/groups/{name}/process/advance", v1GroupRoute(handleGroupProcessAdvance))

	// Staged-spawn choreography status (JOH-244): read the group's pending
	// waves (open, read-only). 404 when the deploy has no pending waves.
	mux.HandleFunc("GET /v1/groups/{name}/waves", v1GroupRoute(handleGroupWavesGet))

	// Re-brief a deployed force (JOH-247): re-deliver the source template's
	// work pattern to the live roster with the group's mission interpolated.
	// Gated on templates.instantiate + owner-pass (see handleGroupRebrief).
	mux.HandleFunc("POST /v1/groups/{name}/rebrief", v1GroupRoute(handleGroupRebrief))

	// Stand down a task force (JOH-345): retire the roster + sweep the
	// deploy-seeded rhythms and pending waves, keeping the group as a dormant
	// record. The mirror of `task-force deploy`. Gated on groups.members.retire +
	// owner-pass (see handleGroupStandDown).
	mux.HandleFunc("POST /v1/groups/{name}/stand-down", v1GroupRoute(handleGroupStandDown))

	mux.HandleFunc("GET /v1/groups/{name}/members", v1GroupRoute(handleGroupMembersList))
	mux.HandleFunc("POST /v1/groups/{name}/members", v1GroupRoute(handleGroupMembersAdd))
	mux.HandleFunc("PATCH /v1/groups/{name}/members/{conv}", v1GroupRoute(func(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
		handleGroupMembersUpdate(w, r, g, r.PathValue("conv"))
	}))
	mux.HandleFunc("DELETE /v1/groups/{name}/members/{conv}", v1GroupRoute(func(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
		handleGroupMembersRemove(w, r, g, r.PathValue("conv"))
	}))
	mux.HandleFunc("POST /v1/groups/{name}/members/{conv}/move", v1GroupRoute(func(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
		handleGroupMembersMove(w, r, g, r.PathValue("conv"))
	}))
	mux.HandleFunc("GET /v1/groups/{name}/kept-permissions", v1GroupRoute(handleGroupKeptPermissionsList))
	mux.HandleFunc("DELETE /v1/groups/{name}/kept-permissions/{conv}", v1GroupRoute(func(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
		handleGroupKeptPermissionsDrop(w, r, g, r.PathValue("conv"))
	}))

	// handleGroupLinks dispatches its own methods, so the two link
	// patterns are registered without a method in the pattern.
	mux.HandleFunc("/v1/groups/{name}/links", v1GroupRoute(func(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
		handleGroupLinks(w, r, g, nil)
	}))
	mux.HandleFunc("/v1/groups/{name}/links/{id}", v1GroupRoute(func(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
		handleGroupLinks(w, r, g, []string{r.PathValue("id")})
	}))
	mux.HandleFunc("/v1/groups/{name}/attachment", v1GroupRoute(handleGroupAttachment))

	mux.HandleFunc("PATCH /v1/groups/{name}", v1GroupRoute(handleGroupUpdate))
	mux.HandleFunc("DELETE /v1/groups/{name}", v1GroupRoute(handleGroupDelete))
}

// v1GroupRoute adapts a group-scoped /v1 handler into an
// http.HandlerFunc. It resolves the {name} path wildcard to a group row
// and replies with a 404 error envelope when the group is missing; the
// per-handler permission gate (SO_PEERCRED identity → requirePermission)
// still runs inside fn, exactly as before.
func v1GroupRoute(fn func(http.ResponseWriter, *http.Request, *db.AgentGroup)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if name == "" {
			writeError(w, http.StatusBadRequest, "invalid_arg", "missing group name")
			return
		}
		g, err := db.GetAgentGroupByName(name)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		if g == nil {
			writeError(w, http.StatusNotFound, "not_found", "no such group")
			return
		}
		fn(w, r, g)
	}
}

// handleGroupDelete deletes a group. Permission slug: groups.delete
// (default human-only). db.DeleteAgentGroup fails with a constraint
// error if the group still has blocking references.
func handleGroupDelete(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
	if _, ok := requirePermission(w, r, PermGroupsDelete); !ok {
		return
	}
	hookHarnesses := standingOrderHookHarnessesForGroupBestEffort(g.ID)
	if err := db.DeleteAgentGroup(g.Name); err != nil {
		writeError(w, http.StatusConflict, "constraint", err.Error())
		return
	}
	closeGroupPages(g.ID)
	if warning := reconcileStandingOrderHookHarnesses(hookHarnesses); warning != "" {
		w.Header().Set("X-Tclaude-Hook-Warning", warning)
		slog.Warn("group delete: standing-order hook reconciliation failed",
			"group", g.Name, "warning", warning)
	}
	w.WriteHeader(http.StatusNoContent)
}

// maxGroupContextBytes caps a group's startup context. The context is
// folded into the startup briefing delivered to each spawned agent's
// inbox; 16 KiB is comfortably more than any reasonable block of
// startup guidance while keeping the briefing message bounded.
const maxGroupContextBytes = 16 * 1024

// normalizeGroupContext prepares a group startup context for storage:
// CRLF / lone-CR line endings are folded to LF so the briefing
// renders consistently regardless of where the human typed it, and
// the result is rejected when it exceeds maxGroupContextBytes.
func normalizeGroupContext(s string) (string, error) {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	if len(s) > maxGroupContextBytes {
		return "", fmt.Errorf("group context too long: %d bytes (max %d)", len(s), maxGroupContextBytes)
	}
	return s, nil
}

// resolveGroupDefaultProfileName validates a requested group default-profile
// reference (JOH-210): it trims the name and, when non-empty, requires a spawn
// profile by that name to exist — there is no DB-level foreign key, so the
// referential check lives here. Empty is valid and clears the group's default.
// Returns the trimmed name to store.
func resolveGroupDefaultProfileName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", nil
	}
	p, err := db.ResolveSpawnProfile(name)
	if err != nil {
		return "", err
	}
	if p == nil {
		return "", fmt.Errorf("no spawn profile named %q", name)
	}
	return p.Name, nil
}

// normalizeGroupDescr prepares a group description for storage. The
// descr is a one-line label rendered inline in the dashboard's group
// header, so any embedded CR / LF is folded to a single space and the
// result is trimmed — a raw API caller can no longer wedge a newline
// into a header that has nowhere to put it. Empty stays empty.
//
// Applied on every write path — create, update, and clone — so the
// one-line invariant holds regardless of how the descr was set, not
// just when it was last edited.
func normalizeGroupDescr(s string) string {
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.TrimSpace(s)
}

// groupUpdateBody is a group settings update; each field left out stays
// unchanged.
type groupUpdateBody struct {
	Descr             *string                           `json:"descr,omitempty"`
	DefaultCwd        *string                           `json:"default_cwd,omitempty"`
	DefaultContext    *string                           `json:"default_context,omitempty"`
	Environment       *[]sandboxpolicy.EnvironmentEntry `json:"environment,omitempty"`
	DefaultSpawnGroup *bool                             `json:"default_spawn_group,omitempty"`
	// DefaultProfile names the spawn profile (JOH-210) whose launch fields
	// fill blank spawn fields for this group's agents; "" clears it. *string
	// so a caller can clear it by sending "" — distinct from omitting it.
	DefaultProfile *string `json:"default_profile,omitempty"`
	// DefaultPrompts is a complete replacement of the group's saved-prompt
	// set; [] clears it.
	DefaultPrompts *[]string `json:"default_prompts,omitempty"`
	// MaxMembers is the group's hard member cap; 0 = unlimited. A
	// negative value is clamped to 0 by db.SetAgentGroupMaxMembers.
	MaxMembers *int `json:"max_members,omitempty"`
	// NotifyEnabled is the group's OS-notification switch; false
	// mutes state-transition notifications for every member agent
	// (a per-agent 'on' pref still overrides).
	NotifyEnabled *bool `json:"notify_enabled,omitempty"`
	// RemoteControlPolicy is the group's remote-control policy that overrides
	// a spawn profile's remote-control default (JOH-262): "inherit" (defer to
	// the profile), "optin" (force on) or "deny" (force off). *string so a
	// caller can change it without touching the other fields; omitting it
	// leaves the policy unchanged.
	RemoteControlPolicy *string `json:"remote_control_policy,omitempty"`
	// ReinjectAfterCompact sets what tclaude re-injects into member agents
	// after a compaction or /clear: "contexts", "identity" or "off".
	ReinjectAfterCompact *string `json:"reinject_after_compact,omitempty"`
	// Permissions is a complete replacement allowlist. Pointer distinguishes
	// omitted (unchanged) from [] (clear every group grant).
	Permissions *[]db.PermissionGrant `json:"permissions,omitempty"`
	// OwnerScopes is a complete replacement owner-scope map for this group
	// (TCL-1071): slug → the scope that slug's owner-implied bypass is
	// confined to here. Raw so {} and null both reach the parser and both
	// mean "no narrowing"; the pointer distinguishes omitted (unchanged)
	// from an explicit clear.
	OwnerScopes *json.RawMessage `json:"owner_scopes,omitempty"`
}

// handleGroupUpdate patches mutable group-level settings:
//
//   - descr — the group's own one-line description, shown next to the
//     group name on the dashboard. Distinct from a per-member descr.
//   - default_cwd — the working directory pre-filled into the spawn
//     form (and substituted server-side by handleGroupSpawn when a
//     spawn request leaves cwd blank).
//   - default_context — a block of shared startup guidance delivered
//     to the inbox of agents spawned into the group (see
//     handleGroupSpawn).
//   - default_profile — the spawn profile (JOH-210) whose launch fields
//     fill blank spawn fields server-side (see executeSpawn /
//     applyDefaultProfile), so a group can default its whole team onto a
//     given harness+model+… without the Claude-only default_model gate.
//     Validated only for existence; the profile carries its own validated
//     launch fields. Replaces the retired default_model setting (#343).
//   - max_members — the group's hard member cap (0 = unlimited); a
//     spawn that would exceed it is refused by the spawn-guardrail
//     layer. See checkSpawnGuardrails.
//   - permissions — live additive grants held by every current member. These
//     are membership policy, not spawn defaults, and are never copied into
//     agent_permissions.
//   - owner_scopes — the narrowing applied to this group's STRUCTURAL
//     owner-implied permission bypass (TCL-1071), as a slug → scope map.
//     {} / null clears it, restoring the unrestricted bypass. It never
//     touches an explicit grant an owner holds.
//
// Partial-update contract, matching handleGroupMembersUpdate: only
// fields present (non-nil) in the body are touched. descr / default_cwd
// / default_context / default_profile are *string so a caller can clear
// any of them by sending "" — distinct from omitting it; max_members is
// *int and clears to "unlimited" with 0. An empty body (no field) is a
// 400.
//
// Each independently meaningful setting has its own permission. This keeps a
// default-directory grant from also authorizing a rename, context injection,
// or a member-cap change. groups.admin is accepted centrally as an umbrella.
//
// permissions AND owner_scopes are permission administration and additionally
// require permissions.grant + permissions.revoke — the same pair, because both
// fields can widen (clearing a narrowing) and narrow (adding one) in a single
// write.
func handleGroupUpdate(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
	u := &groupUpdate{r: r, g: g}
	defer u.close()
	if !u.decode(w) {
		return
	}
	if !u.authorize(w) {
		return
	}
	if !u.normalize(w) {
		return
	}
	if !u.applyLaunchDefaults(w) {
		return
	}
	u.applyMembershipPolicy(w)
}

// groupUpdate is one group settings update's state, built up step by step by
// handleGroupUpdate. Each step writes its own response when it stops the
// update and returns false.
type groupUpdate struct {
	r                     *http.Request
	g                     *db.AgentGroup
	body                  groupUpdateBody
	caller                string
	defaultPrompts        []string
	ownerScopes           string
	normalizedEnvironment []sandboxpolicy.EnvironmentEntry
	reinjectMode          string
	normalizedPermissions []db.PermissionGrant
	cwd                   string
	ctx                   string
	profile               string
	policy                *bool
	resp                  map[string]any
	// cleanups are the handler's deferred closes, run in reverse by close.
	cleanups []func()
}

// close runs the registered cleanups, last registered first, as the
// handler's defers did.
func (u *groupUpdate) close() {
	// Deferred one by one, so a cleanup that panics still lets the earlier
	// ones run, exactly as separate defers would.
	for _, cleanup := range u.cleanups {
		defer cleanup()
	}
}

// decode reads the update and refuses one that names no field.
func (u *groupUpdate) decode(w http.ResponseWriter) bool {
	if err := json.NewDecoder(u.r.Body).Decode(&u.body); err != nil {
		writeError(w, http.StatusBadRequest, "json", err.Error())
		return false
	}
	if u.body.Descr == nil && u.body.DefaultCwd == nil && u.body.DefaultContext == nil && u.body.Environment == nil && u.body.DefaultSpawnGroup == nil && u.body.DefaultProfile == nil && u.body.DefaultPrompts == nil && u.body.MaxMembers == nil && u.body.NotifyEnabled == nil && u.body.RemoteControlPolicy == nil && u.body.ReinjectAfterCompact == nil && u.body.Permissions == nil && u.body.OwnerScopes == nil {
		writeError(w, http.StatusBadRequest, "invalid_arg",
			"nothing to update (expected descr, default_cwd, default_context, environment, default_spawn_group, default_profile, default_prompts, max_members, notify_enabled, remote_control_policy, reinject_after_compact, permissions and/or owner_scopes)")
		return false
	}
	return true
}

// authorize checks the settings permission of every field the update names,
// that the group is active, and a peer's limits on the default profile and
// prompts.
func (u *groupUpdate) authorize(w http.ResponseWriter) bool {
	required := make([]string, 0, 10)
	if u.body.Descr != nil {
		required = append(required, PermGroupsSettingsDescription)
	}
	if u.body.DefaultCwd != nil {
		required = append(required, PermGroupsSettingsDefaultDir)
	}
	// The re-injection mode governs what happens to the startup context after
	// a compaction, so it shares the startup-context slug, as do the default
	// prompts: both are text every fresh member receives at spawn.
	if u.body.DefaultContext != nil || u.body.ReinjectAfterCompact != nil || u.body.DefaultPrompts != nil {
		required = append(required, PermGroupsSettingsDefaultContext)
	}
	if u.body.Environment != nil {
		required = append(required, PermGroupsSettingsEnvironment)
	}
	if u.body.DefaultSpawnGroup != nil {
		required = append(required, PermGroupsSettingsDefaultSpawnTarget)
	}
	if u.body.DefaultProfile != nil {
		required = append(required, PermGroupsSettingsDefaultProfile)
	}
	if u.body.MaxMembers != nil {
		required = append(required, PermGroupsSettingsMaxMembers)
	}
	if u.body.NotifyEnabled != nil {
		required = append(required, PermGroupsSettingsNotifications)
	}
	if u.body.RemoteControlPolicy != nil {
		required = append(required, PermGroupsSettingsRemoteControlPolicy)
	}
	if u.body.Permissions != nil {
		required = append(required, PermGroupsSettingsMemberPermissions)
	}
	if u.body.OwnerScopes != nil {
		required = append(required, PermGroupsSettingsOwnerScopes)
	}
	u.caller = ""
	for _, perm := range required {
		resolved, ok := requireGroupPermission(w, u.r, perm, u.g, ActionContext{Group: u.g.Name})
		if !ok {
			return false
		}
		u.caller = resolved
	}
	if !requireGroupActive(w, u.g) {
		return false
	}
	if a := peerGrantFromRequest(u.r); a != nil && u.body.DefaultProfile != nil {
		name, err := resolveGroupDefaultProfileName(*u.body.DefaultProfile)
		if err == nil {
			err = a.allowsDefaultProfile(name)
		}
		if err != nil {
			writeError(w, http.StatusForbidden, "profile_not_allowed", "a peer may set only a profile it may pick for a spawn into this group")
			return false
		}
	}
	if a := peerGrantFromRequest(u.r); a != nil && u.body.DefaultPrompts != nil && !a.allowsDefaultPrompts() {
		writeError(w, http.StatusForbidden, "permission", fmt.Sprintf("this node does not grant you %s for group:%s", PermGroupsMembersSpawnPrompts, u.g.Name))
		return false
	}
	return true
}

// normalize validates and canonicalizes every field before any is written:
// prompts, the permission-administration gate, owner scopes, environment,
// re-injection mode, permission rows, directory, context, profile and
// remote-control policy.
func (u *groupUpdate) normalize(w http.ResponseWriter) bool {
	if u.body.DefaultPrompts != nil {
		// Validate before any field is written so a bad name cannot leave a
		// partial settings update behind.
		var fail *spawnFailure
		if u.defaultPrompts, fail = resolveSavedPromptRefs(*u.body.DefaultPrompts); fail != nil {
			writeError(w, fail.Status, fail.Kind, fail.Msg)
			return false
		}
	}
	if u.body.Permissions != nil || u.body.OwnerScopes != nil {
		// Authorize the higher-impact fields before applying ANY part of a mixed
		// PATCH, so a caller cannot get a partial settings update followed by a
		// permission-administration 403.
		if _, ok := requirePermission(w, u.r, PermPermissionsGrant); !ok {
			return false
		}
		if _, ok := requirePermission(w, u.r, PermPermissionsRevoke); !ok {
			return false
		}
	}
	// Validate + canonicalize the owner-scope map before any write lands, for
	// the same reason: a rejected map must not follow a committed descr.
	u.ownerScopes = ""
	if u.body.OwnerScopes != nil {
		canonical, err := canonicalOwnerScopes(string(*u.body.OwnerScopes))
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_owner_scopes", err.Error())
			return false
		}
		u.ownerScopes = canonical
	}
	if u.body.Environment != nil {
		var err error
		u.normalizedEnvironment, err = sandboxpolicy.NormalizeEnvironment(*u.body.Environment)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_environment", err.Error())
			return false
		}
	}
	u.reinjectMode = ""
	if u.body.ReinjectAfterCompact != nil {
		u.reinjectMode = strings.TrimSpace(*u.body.ReinjectAfterCompact)
		if u.reinjectMode == "" || !db.ValidReinjectAfterCompact(u.reinjectMode) {
			writeError(w, http.StatusBadRequest, "invalid_reinject_after_compact",
				fmt.Sprintf("invalid reinject_after_compact %q (want contexts, identity, or off)", u.reinjectMode))
			return false
		}
	}
	if u.body.Permissions != nil {
		var failure *spawnFailure
		u.normalizedPermissions, failure = normalizeBlueprintGrants(*u.body.Permissions)
		if failure != nil {
			writeError(w, failure.Status, failure.Kind, failure.Msg)
			return false
		}
		sort.Slice(u.normalizedPermissions, func(i, j int) bool {
			return u.normalizedPermissions[i].Slug < u.normalizedPermissions[j].Slug
		})
		// A group grant is conferred on every current AND future member, so it is
		// a minting surface. An agent caller must clear the same attenuation bar
		// as the spawn boundary and /v1/permissions/grant. Without this an agent
		// whose own hold on a slug is scoped could add that slug to a group it
		// belongs to and, on the very next request, resolve unscoped: the group
		// tier unions its rows, and one unscoped row absorbs the tier.
		conferred := make([]conferredGrant, 0, len(u.normalizedPermissions))
		for _, grant := range u.normalizedPermissions {
			conferred = append(conferred, conferredGrant{Slug: grant.Slug, Scope: grant.Scope})
		}
		if err := checkGrantAttenuation(u.caller, grantConferee{}, conferred); err != nil {
			writeError(w, http.StatusForbidden, "scope_not_attenuated", err.Error())
			return false
		}
	}
	// Every field is validated before the first write, so a rejected save leaves
	// nothing behind.
	if u.body.DefaultCwd != nil {
		// Normalise + validate: expand "~", require an absolute path (a
		// relative default would resolve against the daemon's cwd at
		// spawn time, which is meaningless). Empty stays empty — clears it.
		var err error
		if u.cwd, err = resolveGroupDefaultCwd(*u.body.DefaultCwd); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_cwd", err.Error())
			return false
		}
	}
	if u.body.DefaultContext != nil {
		// Fold CRLF → LF and enforce the size cap. Empty stays empty —
		// that clears the group context.
		var err error
		if u.ctx, err = normalizeGroupContext(*u.body.DefaultContext); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
			return false
		}
	}
	if u.body.DefaultProfile != nil {
		// Validate only that the referenced profile exists (its launch fields
		// were validated against their own harness at save) — the harness-correct
		// replacement for the retired Claude-only default_model gate (#343).
		// Empty stays empty — that clears the group default.
		var err error
		if u.profile, err = resolveGroupDefaultProfileName(*u.body.DefaultProfile); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_profile", err.Error())
			return false
		}
		if fail := defaultProfileConferralFailure(u.r, u.caller, u.g.Name, u.profile); fail != nil {
			writeError(w, fail.Status, fail.Kind, fail.Msg)
			return false
		}
	}
	if u.body.RemoteControlPolicy != nil {
		var err error
		if u.policy, err = parseRemoteControlPolicy(*u.body.RemoteControlPolicy); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_remote_control_policy", err.Error())
			return false
		}
	}
	return true
}

// applyLaunchDefaults writes the description and the launch defaults (default
// directory, default spawn group, context, environment, profile, prompts),
// keeping a spent sudo use once anything is written.
func (u *groupUpdate) applyLaunchDefaults(w http.ResponseWriter) bool {
	u.resp = map[string]any{"group": u.g.Name}
	// The writes are not one transaction, so a failure part way through has
	// changed something, and a sudo use spent on it stays spent.
	u.cleanups = append(u.cleanups, func() {
		if len(u.resp) > 1 {
			keepSudoUses(u.r)
		}
	})

	if u.body.Descr != nil {
		// The group descr is a one-line label. Fold any embedded
		// newline (an API caller could send one — the CLI positional
		// and the dashboard's <input type=text> cannot) to a space so
		// it never breaks the single-line dashboard header. Empty
		// stays empty — that clears the description.
		descr := normalizeGroupDescr(*u.body.Descr)
		if !u.writeField(w, "descr", func() (int64, error) { return db.SetAgentGroupDescr(u.g.Name, descr) }, descr) {
			return false
		}
	}

	if u.body.DefaultCwd != nil {
		if !u.writeField(w, "default_cwd", func() (int64, error) { return db.SetAgentGroupDefaultCwd(u.g.Name, u.cwd) }, u.cwd) {
			return false
		}
	}

	if u.body.DefaultSpawnGroup != nil {
		if !u.writeFieldOr(w, http.StatusBadRequest, "invalid_default_spawn_group", "default_spawn_group", func() (int64, error) { return db.SetAgentGroupDefaultSpawn(u.g.Name, *u.body.DefaultSpawnGroup) }, *u.body.DefaultSpawnGroup) {
			return false
		}
	}

	if u.body.DefaultContext != nil {
		if !u.writeField(w, "default_context", func() (int64, error) { return db.SetAgentGroupDefaultContext(u.g.Name, u.ctx) }, u.ctx) {
			return false
		}
	}

	if u.body.Environment != nil {
		if !u.writeField(w, "environment", func() (int64, error) { return db.SetAgentGroupEnvironment(u.g.Name, u.normalizedEnvironment) }, u.normalizedEnvironment) {
			return false
		}
	}

	if u.body.DefaultProfile != nil {
		if !u.writeField(w, "default_profile", func() (int64, error) { return db.SetAgentGroupDefaultProfile(u.g.Name, u.profile) }, u.profile) {
			return false
		}
	}

	if u.body.DefaultPrompts != nil {
		if !u.writeField(w, "default_prompts", func() (int64, error) { return db.SetAgentGroupDefaultPrompts(u.g.Name, u.defaultPrompts) }, u.defaultPrompts) {
			return false
		}
	}

	return true
}

// applyMembershipPolicy writes the member cap, notifications, remote-control
// policy, re-injection mode, permissions and owner scopes, and answers with
// what was stored.
func (u *groupUpdate) applyMembershipPolicy(w http.ResponseWriter) bool {
	if u.body.MaxMembers != nil {
		// db.SetAgentGroupMaxMembers clamps a negative value to 0
		// (unlimited) rather than rejecting it, so a careless caller
		// can never wedge a group with an impossible cap.
		stored := *u.body.MaxMembers
		if stored < 0 {
			stored = 0
		}
		if !u.writeField(w, "max_members", func() (int64, error) { return db.SetAgentGroupMaxMembers(u.g.Name, *u.body.MaxMembers) }, stored) {
			return false
		}
	}

	if u.body.NotifyEnabled != nil {
		if !u.writeField(w, "notify_enabled", func() (int64, error) { return db.SetAgentGroupNotifyEnabled(u.g.Name, *u.body.NotifyEnabled) }, *u.body.NotifyEnabled) {
			return false
		}
	}

	if u.body.RemoteControlPolicy != nil {
		if !u.writeField(w, "remote_control_policy", func() (int64, error) { return db.SetAgentGroupRemoteControl(u.g.Name, u.policy) }, remoteControlPolicyToWire(u.policy)) {
			return false
		}
	}

	if u.body.ReinjectAfterCompact != nil {
		// Validated above, before any write landed.
		if !u.writeField(w, "reinject_after_compact", func() (int64, error) { return db.SetAgentGroupReinjectAfterCompact(u.g.Name, u.reinjectMode) }, u.reinjectMode) {
			return false
		}
	}

	if u.body.Permissions != nil {
		// Changing a group allowlist is permission administration, not ordinary
		// cosmetic group editing. Both meta-slugs were checked before mutations
		// because a replacement can widen and narrow the set in one request.
		grantedBy := u.caller
		if grantedBy == "" {
			grantedBy = "<human>"
		}
		if err := db.ReplaceAgentGroupPermissionGrants(u.g.ID, u.normalizedPermissions, grantedBy); err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return false
		}
		u.resp["permissions"] = u.normalizedPermissions
	}

	if u.body.OwnerScopes != nil {
		// Same class as the allowlist above, and gated on the same pair. The
		// canonical form was computed before any write landed, so this cannot
		// fail on validation here.
		if !u.writeField(w, "owner_scopes", func() (int64, error) { return db.SetAgentGroupOwnerScopes(u.g.Name, u.ownerScopes) }, ownerScopesWire(u.ownerScopes)) {
			return false
		}
	}

	writeJSON(w, http.StatusOK, u.resp)
	return true
}

// writeField makes one field's write and records the stored value in the
// answer. An error is reported as io; zero rows means the group was renamed or
// deleted between the dispatcher's lookup and this update, so it reports
// not_found rather than a misleading 200.
func (u *groupUpdate) writeField(w http.ResponseWriter, key string, write func() (int64, error), stored any) bool {
	return u.writeFieldOr(w, http.StatusInternalServerError, "io", key, write, stored)
}

// writeFieldOr is writeField for a write whose error is the caller's to fix,
// reported with the given status and code.
func (u *groupUpdate) writeFieldOr(w http.ResponseWriter, errStatus int, errCode, key string, write func() (int64, error), stored any) bool {
	n, err := write()
	if err != nil {
		writeError(w, errStatus, errCode, err.Error())
		return false
	}
	if n == 0 {
		writeError(w, http.StatusNotFound, "not_found", "no such group")
		return false
	}
	u.resp[key] = stored
	return true
}

// ownerScopesWireOmitEmpty is ownerScopesWire for a field tagged omitempty: it
// answers nil rather than an empty map so an unnarrowed group serializes with
// no owner_scopes key at all.
func ownerScopesWireOmitEmpty(raw string) map[string]PermissionScope {
	out := ownerScopesWire(raw)
	if len(out) == 0 {
		return nil
	}
	return out
}

// ownerScopesWire renders a stored owner-scope map for a JSON response: the
// decoded object, or an empty object when the group imposes no narrowing. A
// raw string would force every client to double-decode, and json.RawMessage
// of "" is not valid JSON.
//
// A stored map this build cannot decode also renders empty — there is nothing
// typed to show — so it is LOGGED here rather than passing silently. The
// display is lossy in that case, but it cannot destroy the stored value: an
// editor sends owner_scopes only when its box was edited, and an omitted field
// leaves the row untouched.
func ownerScopesWire(raw string) map[string]PermissionScope {
	out := map[string]PermissionScope{}
	scopes, err := ownerScopesForEval(raw)
	if err != nil {
		slog.Warn("permissions: stored owner scopes do not decode on this build; "+
			"rendering them as absent (the gate still fails their bypass closed)",
			"error", err)
		return out
	}
	for slug, scope := range scopes {
		out[slug] = scope
	}
	return out
}

// parseRemoteControlPolicy maps a group remote-control policy wire token to the
// tri-state *bool stored on the group (JOH-262): "" / "inherit" → nil (defer to
// the spawn profile's default), "optin" / "on" → true (force Remote Access on),
// "deny" / "off" → false (force it off). Any other token is an error so a typo
// can't silently land as "inherit". Tolerant of a few synonyms so the CLI and
// dashboard can speak the operator's words.
func parseRemoteControlPolicy(s string) (*bool, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "inherit", "unset", "default":
		return nil, nil
	case "optin", "opt-in", "on", "enable", "enabled", "true":
		v := true
		return &v, nil
	case "deny", "off", "disable", "disabled", "false":
		v := false
		return &v, nil
	default:
		return nil, fmt.Errorf("invalid remote_control_policy %q (want inherit, optin, or deny)", s)
	}
}

// remoteControlPolicyToWire is the inverse of parseRemoteControlPolicy: it maps
// the stored tri-state *bool back to its canonical wire token for read surfaces
// (group GET, dashboard payload). nil → "inherit", true → "optin", false → "deny".
func remoteControlPolicyToWire(p *bool) string {
	switch {
	case p == nil:
		return "inherit"
	case *p:
		return "optin"
	default:
		return "deny"
	}
}
