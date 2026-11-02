package agentd

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// An external steward can start workers only inside limits the operator set for
// it, because it has no launch posture of its own for the approval and sandbox
// lineage rules to bound. Each limit is this file's job:
//
//   - One door: POST /v1/groups/{name}/spawn. executeSpawn refuses a principal
//     anywhere else, and the slugs behind the other doors are on the never-list.
//   - A pin: an agent.spawn grant naming its groups, one spawn profile and one
//     sandbox profile, checked when it is granted and again at spawn.
//   - A short request: only externalStewardSpawnFields marked true may be set.
//   - The baseline posture, so a human answers every action the child takes.
//   - Denies on the child, so a child can't do what its parent never may.
//   - A cap on how many run at once.

// externalStewardSpawnFields classifies every field of agent.SpawnRequest. A field
// marked false, or missing here, is refused when set: everything else about a
// launch comes from what the operator pinned. A test fails until a new field is
// classified.
var externalStewardSpawnFields = map[string]bool{
	"Name": true, "Profile": true, "InitialMessage": true, "TaskURL": true, "TaskLabel": true,

	"AllowUnenforcedSandbox": false, "ApprovalPolicy": false, "AskUserQuestionTimeout": false,
	"AutoFocus": false, "AutoFocusWeb": false, "AutoMemory": false, "AutoReview": false,
	"BreakGlassAcknowledged": false, "CallerCwd": false, "CodexAppServer": false, "CopilotAPI": false,
	"Cwd": false, "Descr": false, "Effort": false, "Environment": false, "Harness": false,
	"HarnessBuiltinMode": false, "HarnessConfig": false, "IsOwner": false, "Model": false,
	"ModelProxy": false, "NonInteractive": false, "OmitSandboxProfiles": false, "PeerMessaging": false,
	"PermissionOverrides": false, "PiDrive": false, "PromptRefs": false, "RemoteControl": false,
	"ReplyTo": false, "Role": false, "RoleRef": false, "RoleRefs": false, "RunTimeoutSeconds": false,
	"SSHWorkaround": false, "SandboxImplementation": false, "SandboxProfile": false,
	"TimeoutSeconds": false, "ToolGovernance": false, "TrustDir": false, "UsageProfiles": false,
	"Attachments": false, "AutoCompactWindow": false, "autoFocusSpecified": false, "autoReviewSpecified": false, "ContextFeatures": false, "ContextWindowMax": false, "descrSpecified": false, "FastMode": false, "IncludeGroupContext": false, "initialMessageSpecified": true, "isOwnerSpecified": false, "nameSpecified": true, "permissionOverridesSpecified": false, "promptRefsSpecified": false, "roleRefSpecified": false, "roleRefsSpecified": false, "roleSpecified": false, "trustDirSpecified": false, "WorktreeBranch": false, "WorktreePath": false, "WriteProofToken": false,
}

// externalStewardChildDenies are written on every child, ahead of anything the
// child's group or the config default would give it. Without them a child could
// hold what its parent never may, and go on acting after the credential ended. The
// spawn slugs keep the tree one level deep.
func externalStewardChildDenies() []string {
	return append(slices.Clone(externalStewardNever), PermAgentSpawn, PermSelfClone)
}

// externalStewardSpawnPin is what an agent.spawn grant to a principal has to name.
type externalStewardSpawnPin struct {
	Groups         []string `json:"groups"`
	SpawnProfile   string   `json:"spawn_profile"`
	SandboxProfile string   `json:"sandbox_profile"`
}

func (p externalStewardSpawnPin) scope() PermissionScope {
	return PermissionScope{
		ScopeDimGroup:          slices.Clone(p.Groups),
		ScopeDimSpawnProfile:   {p.SpawnProfile},
		ScopeDimSandboxProfile: {p.SandboxProfile},
	}
}

// parseExternalStewardSpawnPin reads a pin from a grant's scope. It refuses a scope
// that leaves anything open: the pin is the only bound on the child's sandbox.
func parseExternalStewardSpawnPin(scope PermissionScope) (externalStewardSpawnPin, error) {
	for dim := range scope {
		if dim != ScopeDimGroup && dim != ScopeDimSpawnProfile && dim != ScopeDimSandboxProfile {
			return externalStewardSpawnPin{}, fmt.Errorf("an external steward's agent.spawn scope can't include %s", dim)
		}
	}
	groups, profiles, sandboxes := scope[ScopeDimGroup], scope[ScopeDimSpawnProfile], scope[ScopeDimSandboxProfile]
	if len(groups) == 0 || len(profiles) != 1 || len(sandboxes) != 1 {
		return externalStewardSpawnPin{}, fmt.Errorf("an external steward's agent.spawn has to name its groups, one spawn profile and one sandbox profile")
	}
	for _, value := range slices.Concat(groups, profiles, sandboxes) {
		if strings.TrimSpace(value) == "" || strings.ContainsAny(value, "*?") {
			return externalStewardSpawnPin{}, fmt.Errorf("an external steward's agent.spawn has to name exact groups and profiles, not %q", value)
		}
	}
	return externalStewardSpawnPin{Groups: slices.Clone(groups), SpawnProfile: profiles[0], SandboxProfile: sandboxes[0]}, nil
}

// checkExternalStewardSpawnPin says why a pin can't work, so the operator hears it
// when choosing rather than when a worker fails to start.
func checkExternalStewardSpawnPin(pin externalStewardSpawnPin) error {
	for _, name := range pin.Groups {
		g, err := db.GetAgentGroupByName(name)
		if err != nil {
			return err
		}
		if g == nil {
			return fmt.Errorf("no group is named %q", name)
		}
		if strings.TrimSpace(g.DefaultCwd) == "" {
			return fmt.Errorf("group %q has no default directory, which is where its workers would start", name)
		}
	}
	profile, err := db.GetSpawnProfile(pin.SpawnProfile)
	if err != nil {
		return err
	}
	if profile == nil {
		return fmt.Errorf("no spawn profile is named %q", pin.SpawnProfile)
	}
	if err := externalStewardProfileProblem(profile); err != nil {
		return err
	}
	sandbox, err := db.GetSandboxProfile(pin.SandboxProfile)
	if err != nil {
		return err
	}
	if sandbox == nil {
		return fmt.Errorf("no sandbox profile is named %q", pin.SandboxProfile)
	}
	return nil
}

// externalStewardProfileProblem says why a spawn profile can't start a principal's
// worker: everything spawning would refuse about it, heard when choosing.
func externalStewardProfileProblem(profile *db.SpawnProfile) error {
	if fail := profileSpawnFailure(profile, "external-steward"); fail != nil {
		return fmt.Errorf("%s", fail.Msg)
	}
	if !harness.ApprovalIsBaseline(harnessOrDefault(profile.Harness), profile.Approval, false) {
		return fmt.Errorf("spawn profile %q has to set an approval mode where a human answers every action, such as default on Claude or untrusted on Codex; Copilot and a shell have none",
			profile.Name)
	}
	if (profile.IsOwner != nil && *profile.IsOwner) || len(profile.PermissionOverrides) > 0 {
		return fmt.Errorf("spawn profile %q gives its agents ownership or permissions, which an external steward can't hand out", profile.Name)
	}
	if !spawnSandboxLineageAllowed(externalStewardLineage, spawnLineageSandbox{
		Harness:            harnessOrDefault(profile.Harness),
		HarnessBuiltinMode: strings.TrimSpace(profile.Sandbox),
		Implementation:     normalizeLineageImplementation(profile.SandboxImplementation),
	}) {
		return fmt.Errorf("spawn profile %q has to confine its agents, or the sandbox profile won't be enforced: sandbox on for Claude, read-only or workspace-write for Codex, tclaude's pane sandbox otherwise",
			profile.Name)
	}
	return nil
}

// refuseExternalStewardSpawnGrant answers a direct grant of agent.spawn to a
// principal that doesn't pin what it has to. It returns the message, "" when fine.
func refuseExternalStewardSpawnGrant(convID, slug string, scope PermissionScope) string {
	if !db.IsExternalStewardConv(convID) || slug != PermAgentSpawn {
		return ""
	}
	pin, err := parseExternalStewardSpawnPin(scope)
	if err == nil {
		err = checkExternalStewardSpawnPin(pin)
	}
	if err != nil {
		return err.Error()
	}
	return ""
}

// externalStewardSpawnPinOf reads the principal's standing pin. Only a per-agent
// grant counts: a group tier can't name what this one has to.
func externalStewardSpawnPinOf(convID string) (*externalStewardSpawnPin, error) {
	rows, err := db.ListAgentPermissionOverrideRowsForConv(convID)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		if row.Slug != PermAgentSpawn || row.Effect != db.PermEffectGrant {
			continue
		}
		scope, err := permissionScopeForEval(row.ScopeJSON)
		if err != nil {
			return nil, err
		}
		pin, err := parseExternalStewardSpawnPin(scope)
		if err != nil {
			return nil, err
		}
		return &pin, nil
	}
	return nil, nil
}

// refuseExternalStewardSpawnFields names the first field the request may not set.
func refuseExternalStewardSpawnFields(body *agent.SpawnRequest) string {
	value, kind := reflect.ValueOf(*body), reflect.TypeOf(*body)
	for i := range kind.NumField() {
		if field := kind.Field(i); !externalStewardSpawnFields[field.Name] && !value.Field(i).IsZero() {
			if name, _, _ := strings.Cut(field.Tag.Get("json"), ","); name != "" {
				return name
			}
			return field.Name
		}
	}
	return ""
}

// prepareExternalStewardSpawn runs before the spawn gate. It refuses anything but
// the short request and fills in the pinned profiles, so the gate judges the launch
// that will happen. It writes the response itself when it returns false.
func prepareExternalStewardSpawn(w http.ResponseWriter, r *http.Request, body *agent.SpawnRequest) bool {
	if field := refuseExternalStewardSpawnFields(body); field != "" {
		writeError(w, http.StatusForbidden, externalStewardCeilingCode,
			"an external steward can only choose the group, spawn profile, name, initial message and task link; "+field+" comes from what the operator pinned")
		return false
	}
	pin, err := externalStewardSpawnPinOf(peerFromContext(r.Context()).ConvID)
	if err != nil {
		writeError(w, http.StatusForbidden, externalStewardCeilingCode, err.Error())
		return false
	}
	if pin == nil {
		writeError(w, http.StatusForbidden, "auth", "the operator hasn't let this external steward start workers; that's set in Config > Steward")
		return false
	}
	if body.Profile == "" {
		body.Profile = pin.SpawnProfile
	}
	body.SandboxProfile = pin.SandboxProfile
	return true
}

// externalStewardSpawnChild is a worker a principal started.
type externalStewardSpawnChild struct {
	AgentID string `json:"agent_id"`
	ConvID  string `json:"-"`
	Title   string `json:"title,omitempty"`
	Online  bool   `json:"online"`
}

// externalStewardChildren lists the principal's workers not retired, running or not.
func externalStewardChildren(agentID string) ([]externalStewardSpawnChild, error) {
	ids, err := db.ListAgentChildren(agentID)
	if err != nil {
		return nil, err
	}
	children := make([]externalStewardSpawnChild, 0, len(ids))
	for _, id := range ids {
		conv, err := db.CurrentConvForAgent(id)
		if err != nil || conv == "" {
			continue
		}
		if state, err := db.AgentState(conv); err != nil || state != db.AgentStateActive {
			continue
		}
		children = append(children, externalStewardSpawnChild{
			AgentID: id, ConvID: conv, Title: guideText(agent.CachedTitle(conv), 80), Online: pickAliveSession(conv) != nil,
		})
	}
	return children, nil
}

// externalStewardChildNames maps each worker an external steward started to that
// steward's name, for the roster.
func externalStewardChildNames() map[string]string {
	stewards, err := db.ListExternalStewards()
	if err != nil {
		slog.Warn("external steward: list for the roster failed", "error", err)
		return nil
	}
	names := map[string]string{}
	for _, steward := range stewards {
		children, err := db.ListAgentChildren(steward.AgentID)
		if err != nil {
			slog.Warn("external steward: list workers for the roster failed", "error", err)
			continue
		}
		for _, child := range children {
			names[child] = steward.Name
		}
	}
	return names
}

// externalStewardSpawnChoices is what the Config page offers for a pin: only what
// checkExternalStewardSpawnPin would take.
func externalStewardSpawnChoices() map[string][]string {
	choices := map[string][]string{"groups": {}, "spawn_profiles": {}, "sandbox_profiles": {}}
	if groups, err := db.ListAgentGroups(); err == nil {
		for _, g := range groups {
			if strings.TrimSpace(g.DefaultCwd) != "" {
				choices["groups"] = append(choices["groups"], g.Name)
			}
		}
	}
	if profiles, err := db.ListSpawnProfiles(); err == nil {
		for _, p := range profiles {
			if externalStewardProfileProblem(p) == nil {
				choices["spawn_profiles"] = append(choices["spawn_profiles"], p.Name)
			}
		}
	}
	if profiles, err := db.ListSandboxProfiles(); err == nil {
		for _, p := range profiles {
			choices["sandbox_profiles"] = append(choices["sandbox_profiles"], p.Name)
		}
	}
	return choices
}

// externalStewardSpawning counts the spawns still on their way per principal, so
// two at once can't both pass the cap.
var externalStewardSpawning = struct {
	sync.Mutex
	active map[string]int
}{active: map[string]int{}}

// claimExternalStewardChild reserves a place under the cap. The returned function
// gives it back, once the child is on the list or the spawn has failed.
func claimExternalStewardChild(w http.ResponseWriter, convID string) (func(), bool) {
	agentID, err := db.AgentIDForConv(convID)
	if err == nil && agentID == "" {
		err = fmt.Errorf("no agent identity")
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", "external steward: "+err.Error())
		return nil, false
	}
	cfg, _ := config.LoadShared()
	limit := cfg.ExternalStewardMaxChildren()
	// Read under the lock: a spawn that finishes between these reads and the check
	// would otherwise have let go of its place without yet being counted.
	externalStewardSpawning.Lock()
	defer externalStewardSpawning.Unlock()
	children, err := externalStewardChildren(agentID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", "external steward: "+err.Error())
		return nil, false
	}
	pending, err := pendingExternalStewardChildren(convID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", "external steward: "+err.Error())
		return nil, false
	}
	live := pending
	for _, child := range children {
		if child.Online {
			live++
		}
	}
	if live+externalStewardSpawning.active[agentID] >= limit {
		writeError(w, http.StatusConflict, "external_steward_child_limit",
			fmt.Sprintf("this external steward already has %d running workers, the most the operator allows", limit))
		return nil, false
	}
	externalStewardSpawning.active[agentID]++
	return func() {
		externalStewardSpawning.Lock()
		defer externalStewardSpawning.Unlock()
		if externalStewardSpawning.active[agentID]--; externalStewardSpawning.active[agentID] <= 0 {
			delete(externalStewardSpawning.active, agentID)
		}
	}, true
}

// pendingExternalStewardChildren counts spawns whose conversation hasn't appeared.
func pendingExternalStewardChildren(convID string) (int, error) {
	pending, err := db.ListPendingSpawns()
	if err != nil {
		return 0, err
	}
	count := 0
	for _, spawn := range pending {
		if spawn.SpawnedByConv == convID {
			count++
		}
	}
	return count, nil
}

// writeExternalStewardChildDenies makes a principal's child unable to hold what the
// principal can't. Unlike ordinary birth overrides a failure stops the spawn: a
// child without them would be a way around the never-list.
func writeExternalStewardChildDenies(convID, spawnedByConv string) error {
	_, name := externalStewardIdentity(spawnedByConv)
	granter := "external-steward-child:" + name
	for _, slug := range externalStewardChildDenies() {
		if err := db.SetAgentPermissionOverrideWithScope(convID, slug, db.PermEffectDeny, "", granter); err != nil {
			return fmt.Errorf("deny %s: %w", slug, err)
		}
	}
	return nil
}

// POST /v1/external-stewards/{name}/spawn: choose where this principal may start
// workers. With off set it can't start any. It works with the switch off, so access
// can always be taken away.
func handleExternalStewardSpawnPin(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "set where an external steward can start workers") {
		return
	}
	s, ok := externalStewardNamed(w, r)
	if !ok {
		return
	}
	var body struct {
		externalStewardSpawnPin
		Off bool `json:"off"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	if body.Off {
		if _, err := db.RevokeAgentPermission(s.ConvID, PermAgentSpawn); err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"spawn": nil})
		return
	}
	pin, err := parseExternalStewardSpawnPin(body.scope())
	if err == nil {
		err = checkExternalStewardSpawnPin(pin)
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	scopeJSON, err := json.Marshal(pin.scope())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	if err := db.GrantAgentPermissionWithScope(s.ConvID, PermAgentSpawn, string(scopeJSON), "human"); err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"spawn": pin})
}

// externalStewardExpiryNotified holds the principals the operator has been told
// about, so a notice goes out once. A restart may repeat it, which costs one more
// message about workers still running.
var externalStewardExpiryNotified sync.Map

// startExternalStewardExpiryWatch tells the operator when workers are left running
// by a principal whose last credential ran out. Nothing else happens at that moment
// to show them: an expiry is the absence of a request.
func startExternalStewardExpiryWatch(stop <-chan struct{}) {
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				notifyExternalStewardExpiries(time.Now())
			}
		}
	}()
}

func notifyExternalStewardExpiries(now time.Time) {
	stewards, err := db.ListExternalStewards()
	if err != nil {
		slog.Warn("external steward: list for the expiry watch failed", "error", err)
		return
	}
	for _, steward := range stewards {
		live := slices.ContainsFunc(steward.Credentials, func(c db.ExternalStewardCredential) bool { return c.Live(now) })
		// Only the newest counts: rotating ends an old credential by revoking it, and
		// the one after it can still run out.
		newest := slices.MaxFunc(steward.Credentials, func(a, b db.ExternalStewardCredential) int { return a.CreatedAt.Compare(b.CreatedAt) })
		if live {
			externalStewardExpiryNotified.Delete(steward.AgentID)
			continue
		}
		if !newest.RevokedAt.IsZero() {
			continue // Revoking is the operator's own act, and it lists the workers.
		}
		children, err := externalStewardChildren(steward.AgentID)
		if err != nil {
			slog.Warn("external steward: list workers for the expiry watch failed", "error", err)
			continue
		}
		running := slices.DeleteFunc(children, func(c externalStewardSpawnChild) bool { return !c.Online })
		if len(running) == 0 {
			continue
		}
		if _, told := externalStewardExpiryNotified.LoadOrStore(steward.AgentID, true); told {
			continue
		}
		if _, err := db.InsertHumanMessage(&db.HumanMessage{
			Subject: "External steward's credential ran out",
			Body: fmt.Sprintf("%s's credential ran out and it can't sign in. %d of its workers are still running. "+
				"They can't act without you, and you can stop them from the roster.", steward.Name, len(running)),
		}); err != nil {
			externalStewardExpiryNotified.Delete(steward.AgentID)
			slog.Warn("external steward: expiry notice failed", "error", err)
		}
	}
}
