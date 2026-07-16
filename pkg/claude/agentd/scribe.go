package agentd

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// Scribe summon (JOH-361) — the "summon a pre-briefed, pre-granted
// scribe agent by chat" primitive behind the dashboard's "Edit with agent"
// buttons. A scribe is an ordinary agent the human talks to; its whole value
// is that it comes up already briefed on the task and already holding the
// permission slugs to do it (e.g. templates.manage to edit summoning circles).
//
// Deliberately generic: the request is {name, slugs, brief} and the endpoint
// knows nothing about templates. The Templates tab is the first caller; the
// settings-editing follow-up (JOH-362) uses the same endpoint for spawn
// profiles / the role library by passing different slugs + a different brief.
//
// Shape decisions:
//   - Fresh by default; callers may opt into exact structured-scope reuse.
//     Reuse requires an alive active agent with the same persisted kind/id and
//     exact permission overrides, and refreshes its transient brief via inbox.
//   - Scribes of one kind share the base-name group. executeSpawn is group-bound
//     (no group-less spawn primitive), while a random suffix on each agent name
//     keeps simultaneous scribes individually addressable.
//   - Stable, shared, pre-trusted cwd (~/.tclaude/scribe, NOT $HOME): a scribe
//     edits daemon-side state through the `tclaude agent` CLI, so it needs no
//     repo checkout — but it does need a directory it can START in unprompted.
//     $HOME made the harness ask the human to approve the folder on every
//     launch (JOH-369). See scribeWorkdir for why the dir is stable + shared.

// scribeGroupDescr is stamped on the shared scribe-kind group so the Groups tab
// explains why the ad-hoc agents are grouped together.
const scribeGroupDescr = "Ad-hoc scribe agent — summoned from the dashboard to edit tclaude state by chat (JOH-361)."

// scribeSummonRequest is the wire body of POST /api/scribe and its /v1 twin.
type scribeSummonRequest struct {
	// Name is the base display name and the name of the scribe-kind group. Each
	// spawned agent receives a random suffix.
	Name string `json:"name"`
	// Slugs are the permission slugs to grant the scribe at birth, e.g.
	// ["templates.manage"]. Each is validated against the slug
	// registry; an unknown slug is a 400 listing the known slugs.
	Slugs []string `json:"slugs"`
	// DenySlugs are explicit permission denies applied at birth.
	// This lets a capability-reducing scribe defend its safety boundary even
	// when a global default grants more power.
	DenySlugs []string `json:"deny_slugs,omitempty"`
	// Exclusive turns Slugs into the exact positive capability set: every
	// other registered permission is denied at birth.
	Exclusive bool `json:"exclusive,omitempty"`
	// Scope opts this summon into reuse. Kind and ID are bounded identifiers,
	// never free-form prose: an empty ID is the kind's library scope, while a
	// non-empty ID is one exact resource. The canonical scope is persisted on
	// the scribe's membership; refreshed ref/hash context remains in Brief.
	Scope *scribeReuseScope `json:"scope,omitempty"`
	// TaskURL / TaskLabel give the scoped conversation a durable, clickable
	// task reference. The URL passes the same http(s)-only validation as a
	// regular spawn/task edit before it can reach the dashboard snapshot.
	TaskURL   string `json:"task_ref_url,omitempty"`
	TaskLabel string `json:"task_ref_label,omitempty"`
	// Brief is the pre-briefing delivered to the scribe's inbox — the concept
	// pointer + scope anchor the human's chat starts from. Same charset/length
	// rule as any spawn initial_message.
	Brief string `json:"brief"`
}

type scribeReuseScope struct {
	Kind string `json:"kind"`
	ID   string `json:"id,omitempty"`
}

const (
	scribeScopeDescrPrefix = "Reusable scribe scope: "
	maxScribeScopeKindLen  = 64
	maxScribeScopeIDLen    = 128
)

var scribeScopePartPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// scribeOutcome is summonScribe's result: the fresh or reused scribe's conv-id and the
// open-window handshake (native vs the in-browser terminal fallback),
// mirroring the spawn/open-window responses.
type scribeOutcome struct {
	Name      string
	ConvID    string
	FocusMode string // "native" | "browser" | ""
	FocusWS   string // set when FocusMode == "browser"
}

// handleScribeSummon is the shared /v1 handler. The human (dashboard peer or a
// human on the socket) always passes; an agent caller must hold groups.spawn —
// and, because a summon applies birth-time grants, permissions.grant too. That
// is exactly the bar handleGroupSpawn sets for a birth-time-granted spawn, so
// no new privilege model is introduced.
func handleScribeSummon(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	// requirePermission hands back the caller's conv-id: a real agent resolves
	// to its conv, the human resolves to "".
	spawnerConvID, ok := requirePermission(w, r, PermGroupsSpawn)
	if !ok {
		return
	}

	var body scribeSummonRequest
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "json", err.Error())
			return
		}
	}

	name := strings.TrimSpace(body.Name)
	if name == "" {
		writeError(w, http.StatusBadRequest, "name", "name is required")
		return
	}
	// The name is the scribe's group name (and, when it clears the rename-title
	// gate, its launch --name). Validate it as a group name up front.
	if err := validateGroupName(name); err != nil {
		writeError(w, http.StatusBadRequest, "name", "invalid scribe name: "+err.Error())
		return
	}

	// Slugs → birth-time grants. Reuse the spawn-boundary normaliser so an
	// unknown slug is rejected with the same actionable "known slugs: …" list
	// the spawn endpoint gives.
	in := make(map[string]string, len(body.Slugs))
	for _, s := range body.Slugs {
		if s = strings.TrimSpace(s); s != "" {
			in[s] = db.PermEffectGrant
		}
	}
	for _, s := range body.DenySlugs {
		if s = strings.TrimSpace(s); s != "" {
			if in[s] == db.PermEffectGrant {
				writeError(w, http.StatusBadRequest, "slugs", "permission slug "+s+" cannot be both granted and denied")
				return
			}
			in[s] = db.PermEffectDeny
		}
	}
	if body.Exclusive {
		for _, spec := range permissionRegistry {
			if in[spec.Slug] != db.PermEffectGrant {
				in[spec.Slug] = db.PermEffectDeny
			}
		}
	}
	overrides, povErr := normalizeSpawnPermissionOverrides(in)
	if povErr != "" {
		writeError(w, http.StatusBadRequest, "slugs", povErr)
		return
	}
	if len(overrides) == 0 {
		writeError(w, http.StatusBadRequest, "slugs", "at least one permission slug is required to summon a scribe")
		return
	}

	// A summon applies birth-time grants, so an agent caller (not the human)
	// needs permissions.grant on top of groups.spawn — the same guard
	// handleGroupSpawn puts on a spawn carrying permission_overrides.
	var permissionGrantSudoID int64
	if spawnerConvID != "" {
		resolution, sudoID := resolvePermissionWithSudoGrantID(spawnerConvID, PermPermissionsGrant)
		if resolution != permAllow {
			writeError(w, http.StatusForbidden, "permission",
				"granting a scribe birth-time permissions requires the "+PermPermissionsGrant+" permission")
			return
		}
		permissionGrantSudoID = sudoID
	}

	brief := strings.TrimSpace(body.Brief)
	if brief == "" {
		writeError(w, http.StatusBadRequest, "brief", "brief is required")
		return
	}
	// The brief rides the inbox as an agent_messages row, so it must clear the
	// same charset/length rule every initial_message does.
	if !isValidInitialMessage(brief) {
		writeError(w, http.StatusBadRequest, "brief",
			fmt.Sprintf("brief must be at most %d characters; newlines and tabs are allowed "+
				"(it is delivered to the scribe's inbox, not typed into its pane), but other "+
				"control characters are not", agent.MaxInitialMessageBytes))
		return
	}
	scopeKey, scopeErr := canonicalScribeScope(body.Scope)
	if scopeErr != "" {
		writeError(w, http.StatusBadRequest, "scope", scopeErr)
		return
	}
	taskURL := strings.TrimSpace(body.TaskURL)
	taskLabel := strings.TrimSpace(body.TaskLabel)
	if err := validateTaskRefLabel(taskLabel); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_task_label", err.Error())
		return
	}
	if taskURL != "" {
		if err := validateTaskRefURL(taskURL); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_task_url", err.Error())
			return
		}
	}

	correlationID := newApprovalID()
	granter := scribeSummonGranter(r, spawnerConvID, permissionGrantSudoID, correlationID)
	out, reused, fail := summonScribe(name, overrides, brief, body.Exclusive, scopeKey, taskURL, taskLabel, granter, spawnerConvID)
	if fail != nil {
		writeError(w, fail.Status, fail.Kind, fail.Msg)
		return
	}

	resp := map[string]any{
		"name":    out.Name,
		"conv_id": out.ConvID,
		"reused":  reused,
	}
	if aid := peerAgentID(out.ConvID); aid != "" {
		resp["agent_id"] = aid
	}
	// FocusMode is "browser" when no native window could be popped (headless
	// agentd / no terminal emulator) — the dashboard points at the in-browser
	// terminal instead of claiming success, the same handshake spawn uses.
	if out.FocusMode != "" {
		resp["focus_mode"] = out.FocusMode
		if out.FocusMode == "browser" {
			resp["focus_ws"] = out.FocusWS
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// scribeSummonGranter records the trusted actor class, any sudo lineage for
// permissions.grant, and one server-minted correlation id on every birth-time
// permission row. The id is not an authorization token, and no caller-supplied
// value enters it.
func scribeSummonGranter(r *http.Request, spawnerConvID string, sudoGrantID int64, correlationID string) string {
	actor := granterLabel(spawnerConvID)
	if spawnerConvID != "" && sudoGrantID != 0 {
		actor = fmt.Sprintf("%s:via-sudo:grant-id=%d", spawnerConvID, sudoGrantID)
	}
	if p := peerFromContext(r.Context()); p != nil && p.DashboardHuman {
		actor = dashboardGranter
	}
	return actor + ":scribe-summon:correlation-id=" + correlationID
}

// canonicalScribeScope validates and canonicalises the structured reuse key.
// The slash separator is unambiguous because neither component may contain
// one. Raw briefs/template bodies never enter the key, name, cwd, or pane path.
func canonicalScribeScope(scope *scribeReuseScope) (string, string) {
	if scope == nil {
		return "", ""
	}
	kind := strings.TrimSpace(scope.Kind)
	id := strings.TrimSpace(scope.ID)
	if kind == "" {
		return "", "scope.kind is required"
	}
	if len(kind) > maxScribeScopeKindLen || !scribeScopePartPattern.MatchString(kind) {
		return "", fmt.Sprintf("scope.kind must match %s and be at most %d characters", scribeScopePartPattern, maxScribeScopeKindLen)
	}
	if len(id) > maxScribeScopeIDLen || (id != "" && !scribeScopePartPattern.MatchString(id)) {
		return "", fmt.Sprintf("scope.id must be empty or match %s and be at most %d characters", scribeScopePartPattern, maxScribeScopeIDLen)
	}
	return kind + "/" + id, ""
}

// handleDashboardScribeAPI is the cookie-auth /api twin: the dashboard cookie +
// Origin pin is the human-consent layer, so it stamps a synthetic human peer
// and delegates to the shared handler (same pattern as dashboardSpawnInGroup).
func handleDashboardScribeAPI(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	handleScribeSummon(w, asDashboardHumanPeer(r))
}

// scribeSummonMu serializes summons so two near-simultaneous clicks cannot
// race on the UNIQUE group-name insert.
// Summons are rare human actions, so a global lock costs nothing.
var scribeSummonMu sync.Mutex

// summonScribe is the reusable core: ensure the scribe-kind group, reuse one
// exact compatible structured scope when requested, or spawn a fresh uniquely
// named agent in the shared pre-trusted workdir with birth-time grants.
func summonScribe(name string, overrides map[string]string, brief string, exclusive bool, scopeKey, taskURL, taskLabel, granter, spawnerConvID string) (*scribeOutcome, bool, *spawnFailure) {
	scribeSummonMu.Lock()
	defer scribeSummonMu.Unlock()

	g, fail := ensureScribeGroup(name)
	if fail != nil {
		return nil, false, fail
	}

	// Dead sessions are no longer doing useful parallel work. Preserve the old
	// cleanup behavior by unlinking them, while every live scribe stays in the
	// group and continues its independent editing task.
	pruneDeadScribes(g)
	if scopeKey != "" {
		if reused, reuseFail := reuseScopedScribe(g, scopeKey, overrides, brief, taskURL, taskLabel); reused != nil || reuseFail != nil {
			return reused, reused != nil, reuseFail
		}
	}

	// Adopt the operator's configured scribe launch profile (JOH-371) BEFORE we
	// resolve the trust-seed harness and spawn. Both the trust seed
	// (scribeSpawnHarness) and the launch shape (executeSpawn →
	// applyDefaultProfile) read the group's default profile, so stamping it here
	// makes them agree by construction — a codex-profile scribe gets codex trust
	// seeding, never CC's. Every summon is fresh, so every new scribe observes
	// the profile currently configured by the operator.
	stampScribeProfileFromConfig(g)
	spawnHarness, fail := scribeSpawnHarness(g)
	if fail != nil {
		return nil, false, fail
	}

	// Resolve + create the shared scribe workdir and pre-trust it for the
	// harness this scribe will launch on, so its detached pane comes up without
	// a folder-trust prompt (JOH-369).
	cwd, fail := ensureScribeWorkdir()
	if fail != nil {
		return nil, false, fail
	}
	seedScribeDirTrust(spawnHarness, cwd)

	// executeSpawn enrolls the scribe into its group, applies the birth-time
	// grants (enrollSpawnedConv), delivers the brief to its inbox and —
	// AutoFocus — opens its terminal window. Synchronous (Async left false) so
	// the conv-id materialises for the grants + the response.
	p := spawnParams{
		Name:                uniqueScribeName(name),
		Role:                scribeScopeRole(scopeKey),
		Descr:               scribeScopeDescr(scopeKey),
		TaskURL:             taskURL,
		TaskLabel:           taskLabel,
		Cwd:                 cwd,
		InitialMessage:      brief,
		AutoFocus:           !exclusive,
		PermissionOverrides: overrides,
		PermissionGranter:   granter,
		SpawnedByConv:       spawnerConvID,
	}
	outcome, spawnFail := executeSpawn(g, p)
	if spawnFail != nil {
		return nil, false, spawnFail
	}
	if exclusive {
		if err := enforceExclusiveScribeOverrides(outcome.ConvID, overrides, granter); err != nil {
			killScribeSession(outcome.ConvID)
			return nil, false, &spawnFailure{http.StatusInternalServerError, "permission",
				"capability-reducing scribe stopped after permission override failure: " + err.Error()}
		}
	}
	out := &scribeOutcome{Name: p.Name, ConvID: outcome.ConvID, FocusMode: outcome.FocusMode}
	if outcome.FocusMode == "browser" {
		out.FocusWS = spawnFocusWSPath(outcome.Label)
	}
	if exclusive {
		openScribeWindow(outcome.ConvID, out)
	}
	return out, false, nil
}

func scribeScopeRole(scopeKey string) string {
	if scopeKey == "" {
		return ""
	}
	return "scribe"
}

func scribeScopeDescr(scopeKey string) string {
	if scopeKey == "" {
		return ""
	}
	return scribeScopeDescrPrefix + scopeKey
}

// reuseScopedScribe returns the one alive, active, permission-compatible
// scribe for scopeKey. The caller holds scribeSummonMu, making lookup plus
// potential spawn idempotent across concurrent clicks.
func reuseScopedScribe(g *db.AgentGroup, scopeKey string, overrides map[string]string, brief, taskURL, taskLabel string) (*scribeOutcome, *spawnFailure) {
	members, err := db.ListAgentGroupMembers(g.ID)
	if err != nil {
		return nil, &spawnFailure{http.StatusInternalServerError, "group", "list reusable scribes: " + err.Error()}
	}
	wantDescr := scribeScopeDescr(scopeKey)
	for _, member := range members {
		if member.Descr != wantDescr || pickAliveSession(member.ConvID) == nil {
			continue
		}
		state, err := db.AgentState(member.ConvID)
		if err != nil || state != db.AgentStateActive {
			continue
		}
		current, err := db.ListAgentPermissionOverridesForConv(member.ConvID)
		if err != nil || !maps.Equal(current, overrides) {
			continue
		}
		// Sudo changes the effective capability set and wins over permanent
		// denies. Treat an elevated candidate as incompatible instead of
		// revoking another agent's grants: scribe callers hold grant authority,
		// not the distinct permissions.revoke authority.
		activeSudo, err := db.ListActiveSudoGrants(member.ConvID)
		if err != nil || len(activeSudo) != 0 {
			continue
		}
		if taskURL != "" {
			agentID, err := db.AgentIDForConv(member.ConvID)
			if err != nil || agentID == "" {
				return nil, &spawnFailure{http.StatusInternalServerError, "task_ref", "resolve reusable scribe task reference"}
			}
			if _, err := db.SetAgentTaskRef(agentID, taskURL, taskLabel); err != nil {
				return nil, &spawnFailure{http.StatusInternalServerError, "task_ref", "refresh reusable scribe task reference: " + err.Error()}
			}
		}
		_, err = db.InsertAgentMessage(&db.AgentMessage{
			GroupID:      g.ID,
			FromConv:     "",
			ToConv:       member.ConvID,
			Subject:      "Scribe scope refreshed",
			Body:         brief,
			ToRecipients: []string{member.ConvID},
		})
		if err != nil {
			return nil, &spawnFailure{http.StatusInternalServerError, "brief", "refresh reusable scribe context: " + err.Error()}
		}
		enqueueDeliveryForConv(member.ConvID)
		out := &scribeOutcome{Name: agent.FreshTitle(member.ConvID), ConvID: member.ConvID}
		openScribeWindow(member.ConvID, out)
		return out, nil
	}
	return nil, nil
}

// scribeWorkdir returns the ONE shared workdir all scribes spawn into,
// ~/.tclaude/scribe. Deliberately flat and shared across every scribe kind
// (JOH-369, revised from a per-name dir): a per-name dir would fire the
// harness's folder-trust prompt once PER scribe kind (every future JOH-362
// scribe — profiles, roles, … — re-prompting), whereas one shared path is
// trusted ONCE, ever. Still stable (not a per-summon temp dir), so a scribe's
// CC conversation — which is cwd-bound — starts from the same place and the
// path-attached trust holds. Accepted trade-off: all scribes share this
// dir's CC per-project memory (a shared "scribe house" know-how; revisit
// per-kind isolation only if cross-contamination ever bites).
func scribeWorkdir() (string, bool) {
	dir := config.ConfigDir() // ~/.tclaude, or "" if home can't be resolved
	if dir == "" {
		return "", false
	}
	return filepath.Join(dir, "scribe"), true
}

// ensureScribeWorkdir resolves and creates the shared scribe workdir. 0700:
// it's a private per-user scratch dir agents operate in — no need for group /
// other access.
func ensureScribeWorkdir() (string, *spawnFailure) {
	dir, ok := scribeWorkdir()
	if !ok {
		return "", &spawnFailure{http.StatusInternalServerError, "home", "resolve home directory for scribe workdir"}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", &spawnFailure{http.StatusInternalServerError, "workdir", "create scribe workdir: " + err.Error()}
	}
	return dir, nil
}

// scribeSpawnHarness reports the harness this scribe summon will actually
// launch on. A scribe leaves --harness unset, so it follows the scribe-kind
// group's default spawn profile, then the global default profile (neither →
// the default harness). Mirrors executeSpawn → applyDefaultProfile so the dir
// we pre-trust matches the harness that will read the trust store.
func scribeSpawnHarness(g *db.AgentGroup) (string, *spawnFailure) {
	profiles := []*db.SpawnProfile{groupDefaultProfile(g), globalDefaultProfile()}
	for _, prof := range profiles {
		if fail := disabledProfileFailure(prof); fail != nil {
			return "", fail
		}
	}
	for _, prof := range profiles {
		if prof != nil {
			return harnessOrDefault(prof.Harness), nil
		}
	}
	return harness.DefaultName, nil
}

// stampScribeProfileFromConfig points the scribe group's default spawn profile
// at config.scribe.profile (JOH-371), so the existing group-default-profile
// resolution launches the fresh scribe on that profile with ZERO new spawn
// logic: scribeSpawnHarness → seedScribeDirTrust picks the profile's harness
// for the trust pre-seed, and executeSpawn → applyDefaultProfile fills the
// launch fields from it — BOTH reading the same stamped value, so the trust
// seed can never target a different harness than the spawn (the coupling
// JOH-369 introduced).
//
// Re-stamped on every fresh summon so it tracks a config edit between summons;
// "" clears the stamp (harness default). A name whose profile was
// deleted/renamed is stamped as-is and self-heals to the default at resolution
// time (groupDefaultProfile → nil), mirroring `tclaude ask`'s live self-heal.
// Called on every fresh summon; a compatible reuse keeps its running profile.
//
// Best-effort: a config-load or DB-write failure logs and leaves the group's
// existing default profile in place (worst case the scribe launches on the
// previously-stamped profile, or the harness default), never a failed summon.
// The in-memory g.DefaultProfile is updated on success so the trust seed and
// spawn this same summon runs observe the new stamp without a group reload.
func stampScribeProfileFromConfig(g *db.AgentGroup) {
	cfg, err := config.Load()
	if err != nil {
		// config.Load already degraded to defaults; keep the group's current
		// default rather than clearing it on a transient/corrupt-file error.
		slog.Warn("scribe: load config for profile stamp failed; keeping group default",
			"group", g.Name, "error", err)
		return
	}
	profile := cfg.ScribeProfileName()
	if g.DefaultProfile == profile {
		return // already stamped — idempotent, skip the write
	}
	if _, err := db.SetAgentGroupDefaultProfile(g.Name, profile); err != nil {
		slog.Warn("scribe: stamp default profile failed; keeping group default",
			"group", g.Name, "profile", profile, "error", err)
		return
	}
	g.DefaultProfile = profile
}

// seedScribeDirTrust pre-trusts the shared scribe workdir for the given harness
// so a detached scribe pane never freezes on a folder-trust prompt (JOH-369).
// Best-effort by design: a failure logs and the summon proceeds — the worst
// case is a single one-time trust dialog the human clears via the pane's focus
// button, never a failed summon. Human-consented by construction: it only ever
// pre-trusts the daemon-created scribe dir, never a caller-supplied path.
func seedScribeDirTrust(harnessName, dir string) {
	switch harnessName {
	case harness.CodexName:
		if err := harness.EnsureCodexDirTrusted(dir); err != nil {
			slog.Warn("scribe: pre-trust codex workdir failed", "dir", dir, "error", err)
		}
	case harness.DefaultName: // "claude"
		if err := harness.EnsureClaudeDirTrusted(dir); err != nil {
			slog.Warn("scribe: pre-trust claude workdir failed", "dir", dir, "error", err)
		}
	default:
		// A harness with no known trust store (or one added later without a
		// seeding path wired here). Skip rather than guess — the pane may raise
		// a one-time trust prompt the human clears via its focus button.
		slog.Warn("scribe: no dir-trust seeding for harness; pane may prompt once",
			"harness", harnessName, "dir", dir)
	}
}

// ensureScribeGroup returns the shared scribe-kind group, creating it on first
// summon. The group may contain multiple concurrently active scribes.
//
// It fails closed on a NON-scribe group of the same name: the caller supplies
// the name, so without this a summon whose name collided with a real working
// group would resolve that group and spawn a privileged stray scribe into it.
// Verifying the scribe-group marker
// before touching an existing group keeps the summon operating only on groups
// this machinery created.
func ensureScribeGroup(name string) (*db.AgentGroup, *spawnFailure) {
	g, err := db.GetAgentGroupByName(name)
	if err != nil {
		return nil, &spawnFailure{http.StatusInternalServerError, "group", "look up scribe group: " + err.Error()}
	}
	if g != nil {
		if !isScribeGroup(g) {
			return nil, &spawnFailure{http.StatusConflict, "group",
				fmt.Sprintf("a non-scribe group named %q already exists — rename or remove it to summon a scribe by that name", name)}
		}
		return g, nil
	}
	id, err := db.CreateAgentGroup(name, scribeGroupDescr)
	if err != nil {
		return nil, &spawnFailure{http.StatusInternalServerError, "group", "create scribe group: " + err.Error()}
	}
	g, err = db.GetAgentGroupByID(id)
	if err != nil {
		return nil, &spawnFailure{http.StatusInternalServerError, "group", "reload scribe group: " + err.Error()}
	}
	if g == nil {
		return nil, &spawnFailure{http.StatusInternalServerError, "group", "scribe group vanished after create"}
	}
	return g, nil
}

// isScribeGroup reports whether g is a group this machinery created (marked by
// its descr), so a summon never spawns into a same-named foreign group.
// The descr is stamped at creation and nothing in the scribe path rewrites it; a
// human who edits it just makes future summons fail closed (safe), not misfire.
func isScribeGroup(g *db.AgentGroup) bool {
	return g != nil && g.Descr == scribeGroupDescr
}

// uniqueScribeName makes each summon individually addressable while retaining
// a normalized form of the caller-supplied base as a recognizable prefix. The
// suffix budget is reserved before truncation so the result always clears the
// same 64-byte safe-name gate used by spawn and rename. newApprovalID is a
// cryptographically random 32-hex token; eight characters are ample here
// because this name is cosmetic and the conversation ID remains authoritative.
func uniqueScribeName(base string) string {
	const suffixLen = 9 // '-' + eight hex characters
	prefix := agent.NormalizeSpawnName(base)
	if len(prefix) > agent.MaxSpawnNameLen-suffixLen {
		prefix = strings.TrimRight(prefix[:agent.MaxSpawnNameLen-suffixLen], "-")
	}
	if prefix == "" {
		prefix = "scribe"
	}
	return prefix + "-" + newApprovalID()[:suffixLen-1]
}

// pruneDeadScribes unlinks non-alive members from the shared scribe-kind group.
// Live members are deliberately retained: they may be editing different
// profiles or templates in parallel. Removing membership does not delete the
// old conversation.
func pruneDeadScribes(g *db.AgentGroup) {
	members, err := db.ListAgentGroupMembers(g.ID)
	if err != nil {
		return
	}
	for _, m := range members {
		if pickAliveSession(m.ConvID) != nil {
			continue
		}
		if err := db.RemoveAgentGroupMember(g.ID, m.ConvID); err != nil {
			slog.Warn("scribe: prune dead member failed", "group", g.Name, "conv", short8(m.ConvID), "error", err)
		}
	}
}

// enforceExclusiveScribeOverrides pins the complete capability set after
// enrollment. Exclusive sandbox scribes fail closed on any write error.
func enforceExclusiveScribeOverrides(convID string, overrides map[string]string, granter string) error {
	if _, err := db.RevokeSudoGrantsByConv(convID); err != nil {
		return fmt.Errorf("revoke active sudo grants: %w", err)
	}
	if err := db.ApplyAgentPermissionOverrides(convID, overrides, granter, false, false); err != nil {
		slog.Warn("scribe: apply permission overrides failed", "conv", short8(convID), "error", err)
		return fmt.Errorf("apply permission overrides: %w", err)
	}
	return nil
}

func killScribeSession(convID string) {
	if sess := pickAliveSession(convID); sess != nil {
		if err := clcommon.TmuxCommand("kill-session", "-t", clcommon.ExactTarget(sess.TmuxSession)).Run(); err != nil {
			slog.Error("scribe: failed to kill unsafe capability-reducing scribe", "conv", short8(convID), "error", err)
		}
	}
}

// openScribeWindow opens a terminal attached to the scribe's live session,
// recording the native/browser outcome onto out — the same native-first /
// in-browser-fallback the open-window row action does.
func openScribeWindow(convID string, out *scribeOutcome) {
	sess := pickAliveSession(convID)
	if sess == nil {
		return // caller just confirmed it was alive; a race here is harmless
	}
	if err := openTerminal(openAttachCmd(sess.ID)); err != nil {
		out.FocusMode = "browser"
		out.FocusWS = "/api/open-window-ws/" + url.PathEscape(convID)
		return
	}
	out.FocusMode = "native"
}
