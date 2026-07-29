package agentd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"slices"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

type sandboxProfileEnforcementTarget struct {
	Implementation string                      `json:"implementation"`
	Harness        string                      `json:"harness"`
	Platform       string                      `json:"platform"`
	Predicted      bool                        `json:"predicted"`
	Axes           harness.PredictedAccessAxes `json:"axes"`
	Caveat         string                      `json:"caveat,omitempty"`
}

type sandboxProfileEnforcementResponse struct {
	Profile string                            `json:"profile"`
	Targets []sandboxProfileEnforcementTarget `json:"targets"`
}

type sandboxProfileEnforcementTargetRequest struct {
	Implementation string `json:"implementation"`
	Harness        string `json:"harness"`
	Platform       string `json:"platform"`
	Sandbox        string `json:"sandbox,omitempty"`
}

type sandboxProfileDraftEnforcementRequest struct {
	Draft   sandboxProfileJSON                        `json:"draft"`
	Targets []sandboxProfileEnforcementTargetRequest  `json:"targets,omitempty"`
	Context sandboxProfileDraftEnforcementContextHint `json:"context,omitempty"`
}

type sandboxProfileDraftEnforcementContextHint struct {
	Global string `json:"global,omitempty"`
	Group  string `json:"group,omitempty"`
}

type sandboxProfileDraftEnforcementTarget struct {
	Target         sandboxProfileEnforcementTargetRequest `json:"target"`
	ResolvedBy     string                                 `json:"resolved_by,omitempty"`
	Predicted      bool                                   `json:"predicted"`
	Axes           harness.PredictedAccessAxes            `json:"axes"`
	NetworkEntries []harness.PredictedNetworkEntry        `json:"network_entries,omitempty"`
	ContextAxes    []harness.PredictedAccessAxes          `json:"context_axes,omitempty"`
}

type sandboxProfileEffectiveContext struct {
	Context          map[string]string               `json:"context"`
	Filesystem       []sandboxpolicy.FilesystemGrant `json:"filesystem"`
	Environment      []string                        `json:"environment"`
	AgentDirectories []string                        `json:"agent_directories"`
	Network          sandboxpolicy.NetworkRules      `json:"network"`
	UnixSockets      sandboxpolicy.UnixSocketRules   `json:"unix_sockets"`
	AgentdSocket     string                          `json:"agentd_socket"`
	Notices          []sandboxpolicy.AccessNotice    `json:"notices"`
	policy           sandboxpolicy.Profile
}

type sandboxProfileDraftEnforcementResponse struct {
	Targets           []sandboxProfileDraftEnforcementTarget `json:"targets"`
	Contexts          []sandboxProfileEffectiveContext       `json:"contexts"`
	RemainingContexts int                                    `json:"remaining_contexts,omitempty"`
}

type parsedSandboxProfileEnforcementTarget struct {
	implementation sandboxpolicy.Implementation
	harness        *harness.Harness
	platform       string
	raw            string
}

func handleSandboxProfileEnforcement(w http.ResponseWriter, r *http.Request) {
	if _, ok := requirePermission(w, r, PermSandboxProfilesManage); !ok {
		return
	}
	name := strings.TrimSpace(r.PathValue("name"))
	if name == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg", "missing sandbox profile name")
		return
	}
	rawTargets := r.URL.Query()["for"]
	if len(rawTargets) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_arg", "at least one for target is required")
		return
	}
	profile, err := db.GetSandboxProfile(name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	if profile == nil {
		writeError(w, http.StatusNotFound, "not_found", "no such sandbox profile")
		return
	}
	flattened, err := flattenSandboxProfileForPrediction(profile)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_sandbox_profile", err.Error())
		return
	}
	axes, err := sandboxpolicy.DeriveAccessAxes(flattened)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_sandbox_profile", err.Error())
		return
	}
	response := sandboxProfileEnforcementResponse{
		Profile: profile.Name,
		Targets: make([]sandboxProfileEnforcementTarget, 0, len(rawTargets)),
	}
	for _, raw := range rawTargets {
		target, err := parseSandboxProfileEnforcementTarget(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
			return
		}
		mode, err := resolveSandboxProfilePredictionMode(target, "")
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg",
				fmt.Sprintf("invalid --for target %q: %v", raw, err))
			return
		}
		prediction, err := harness.PredictAccessEnforcement(
			target.harness, target.implementation, axes, mode, target.platform,
		)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg",
				fmt.Sprintf("invalid --for target %q: %v", raw, err))
			return
		}
		item := sandboxProfileEnforcementTarget{
			Implementation: string(target.implementation),
			Harness:        target.harness.Name,
			Platform:       target.platform,
			Predicted:      true,
			Axes: describePredictedSandboxProfile(
				flattened, target, mode,
				harness.DescribePredictedAccess(axes, prediction),
			),
		}
		if target.platform != runtime.GOOS {
			item.Caveat = "(prediction for a non-host platform; host capability probes did not run)"
		} else if target.implementation.UsesTclaudeLayer() {
			probe := cachedTclaudeLayerHostAvailability
			if err := probe(); err != nil {
				item.Caveat = "(prediction only; the target's host capability could not be verified: " + err.Error() + ")"
			}
		}
		response.Targets = append(response.Targets, item)
	}
	writeJSON(w, http.StatusOK, response)
}

// handleSandboxProfileDraftEnforcement predicts an unsaved editor draft. It is
// deliberately inspection-only: the target path terminates at
// PredictAccessEnforcement and therefore cannot produce the opaque launch token
// accepted by PlanAccessEnforcement.
func handleSandboxProfileDraftEnforcement(w http.ResponseWriter, r *http.Request) {
	if _, ok := requirePermission(w, r, PermSandboxProfilesManage); !ok {
		return
	}
	var body sandboxProfileDraftEnforcementRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	draft, _, err := buildSandboxProfile(body.Draft)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_sandbox_profile", err.Error())
		return
	}
	draft.ID = body.Draft.ID
	flattened, err := flattenDraftSandboxProfileForPrediction(draft)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_sandbox_profile", err.Error())
		return
	}
	axes, err := sandboxpolicy.DeriveAccessAxes(flattened)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_sandbox_profile", err.Error())
		return
	}
	draftAxes, err := sandboxpolicy.DeriveAccessAxes(*sandboxProfileDBToPolicy(draft))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_sandbox_profile", err.Error())
		return
	}

	targets := body.Targets
	resolvedBy := ""
	if len(targets) == 0 {
		target, provenance, targetErr := defaultSandboxProfilePredictionTarget(body.Context.Group)
		if targetErr != nil {
			writeError(w, http.StatusInternalServerError, "io", targetErr.Error())
			return
		}
		targets = []sandboxProfileEnforcementTargetRequest{target}
		resolvedBy = provenance
	}
	response := sandboxProfileDraftEnforcementResponse{
		Targets:  []sandboxProfileDraftEnforcementTarget{},
		Contexts: []sandboxProfileEffectiveContext{},
	}
	contexts, remaining, err := effectiveDraftSandboxProfileContexts(draft, body.Context.Group)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_sandbox_profile", err.Error())
		return
	}
	response.Contexts = contexts
	if len(response.Contexts) > 10 {
		response.Contexts = response.Contexts[:10]
	}
	response.RemainingContexts = remaining
	for _, requested := range targets {
		raw := strings.Join([]string{
			strings.TrimSpace(requested.Implementation),
			strings.TrimSpace(requested.Harness),
			strings.TrimSpace(requested.Platform),
		}, "/")
		target, parseErr := parseSandboxProfileEnforcementTarget(raw)
		if parseErr != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", parseErr.Error())
			return
		}
		mode, parseErr := resolveSandboxProfilePredictionMode(target, requested.Sandbox)
		if parseErr != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", parseErr.Error())
			return
		}
		predicted, predictErr := harness.PredictAccessEnforcement(
			target.harness, target.implementation, axes,
			mode, target.platform,
		)
		if predictErr != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", predictErr.Error())
			return
		}
		draftPredicted, predictErr := harness.PredictAccessEnforcement(
			target.harness, target.implementation, draftAxes,
			mode, target.platform,
		)
		if predictErr != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", predictErr.Error())
			return
		}
		described, contextAxes, describeErr := describePredictedDraftSandboxProfile(
			flattened, contexts, target, mode,
			harness.DescribePredictedAccess(axes, predicted),
		)
		if describeErr != nil {
			writeError(w, http.StatusBadRequest, "invalid_sandbox_profile", describeErr.Error())
			return
		}
		if len(contextAxes) > len(response.Contexts) {
			contextAxes = contextAxes[:len(response.Contexts)]
		}
		response.Targets = append(response.Targets, sandboxProfileDraftEnforcementTarget{
			Target: sandboxProfileEnforcementTargetRequest{
				Implementation: string(target.implementation),
				Harness:        target.harness.Name,
				Platform:       target.platform,
				Sandbox:        mode,
			},
			ResolvedBy: resolvedBy,
			Predicted:  true,
			Axes:       described,
			NetworkEntries: predictedDraftNetworkEntries(
				draftAxes.Network, draftPredicted, body.Draft.Network,
			),
			ContextAxes: contextAxes,
		})
	}
	writeJSON(w, http.StatusOK, response)
}

func predictedDraftNetworkEntries(
	rules sandboxpolicy.NetworkRules,
	caps harness.PredictedAccessEnforcement,
	authored *sandboxpolicy.NetworkRules,
) []harness.PredictedNetworkEntry {
	rows := harness.DescribePredictedNetworkEntries(rules, caps)
	if authored == nil {
		return rows
	}
	for _, entry := range authored.Allow {
		materialized, err := sandboxpolicy.MaterializeNetworkRules(
			sandboxpolicy.NetworkRules{
				Baseline: sandboxpolicy.NetworkBaselineDeny,
				Allow:    []sandboxpolicy.NetworkAllowEntry{entry},
			},
		)
		if err != nil || len(materialized.Allow) != 1 {
			continue
		}
		canonicalKey := harness.NetworkEntryPredictionKey(materialized.Allow[0])
		authoredKey := harness.NetworkEntryPredictionKey(entry)
		for i := range rows {
			if harness.NetworkEntryPredictionKey(rows[i].Entry) != canonicalKey ||
				slices.Contains(rows[i].Keys, harness.NetworkEntryModePredictionKey("allow", entry)) {
				continue
			}
			rows[i].Keys = append(rows[i].Keys,
				harness.NetworkEntryModePredictionKey("allow", entry),
				authoredKey,
			)
		}
	}
	denyEntries := make([]sandboxpolicy.NetworkAllowEntry, 0, len(authored.Deny))
	for _, id := range authored.DenyPacks {
		entries, err := sandboxpolicy.ExpandNetworkPackEntries(id)
		if err != nil {
			continue
		}
		denyEntries = append(denyEntries, entries...)
	}
	type denyAlias struct {
		canonical sandboxpolicy.NetworkAllowEntry
		authored  sandboxpolicy.NetworkAllowEntry
	}
	denyAliases := make([]denyAlias, 0, len(authored.Deny))
	for _, entry := range authored.Deny {
		materialized, err := sandboxpolicy.MaterializeNetworkRules(
			sandboxpolicy.NetworkRules{
				Baseline: sandboxpolicy.NetworkBaselineDeny,
				Allow:    []sandboxpolicy.NetworkAllowEntry{entry},
			},
		)
		if err != nil || len(materialized.Allow) != 1 {
			continue
		}
		denyEntries = append(denyEntries, materialized.Allow[0])
		denyAliases = append(denyAliases, denyAlias{
			canonical: materialized.Allow[0],
			authored:  entry,
		})
	}
	denyRows := harness.DescribePredictedNetworkDenyEntries(denyEntries)
	for _, alias := range denyAliases {
		canonicalKey := harness.NetworkEntryPredictionKey(alias.canonical)
		authoredKey := harness.NetworkEntryPredictionKey(alias.authored)
		for i := range denyRows {
			if harness.NetworkEntryPredictionKey(denyRows[i].Entry) != canonicalKey ||
				slices.Contains(denyRows[i].Keys, harness.NetworkEntryModePredictionKey("deny", alias.authored)) {
				continue
			}
			denyRows[i].Keys = append(denyRows[i].Keys,
				harness.NetworkEntryModePredictionKey("deny", alias.authored),
				authoredKey,
			)
		}
	}
	rows = append(rows, denyRows...)
	return rows
}

func flattenDraftSandboxProfileForPrediction(draft *db.SandboxProfile) (sandboxpolicy.Profile, error) {
	rows, err := db.ListSandboxProfiles()
	if err != nil {
		return sandboxpolicy.Profile{}, err
	}
	registry := make(map[string]*sandboxpolicy.Profile, len(rows)+1)
	for _, row := range rows {
		if draft.ID > 0 && row.ID == draft.ID {
			continue
		}
		registry[row.Name] = sandboxProfileDBToPolicy(row)
	}
	root := sandboxProfileDBToPolicy(draft)
	registry[draft.Name] = root
	return sandboxpolicy.Flatten(*root, func(name string) (*sandboxpolicy.Profile, error) {
		return registry[name], nil
	})
}

func defaultSandboxProfilePredictionTarget(groupName string) (sandboxProfileEnforcementTargetRequest, string, error) {
	var groupProfile *db.SpawnProfile
	if strings.TrimSpace(groupName) != "" {
		group, err := db.GetAgentGroupByName(strings.TrimSpace(groupName))
		if err != nil {
			return sandboxProfileEnforcementTargetRequest{}, "", err
		}
		groupProfile = groupDefaultProfile(group)
	}
	globalProfile := globalDefaultProfile()
	tiers := []launchProfileTier{
		{profile: groupProfile, source: profileSource(groupProfile, agent.ProvGroupProfileSource)},
		{profile: globalProfile, source: profileSource(globalProfile, agent.ProvGlobalProfileSource)},
	}

	harnessName := harness.DefaultName
	harnessSource := agent.ProvHarnessDefault
	for _, tier := range tiers {
		if tier.profile != nil {
			harnessName = harnessOrDefault(tier.profile.Harness)
			harnessSource = tier.source
			break
		}
	}
	resolvedHarness, err := harness.Resolve(harnessName)
	if err != nil {
		return sandboxProfileEnforcementTargetRequest{}, "", err
	}

	requestedSandbox, sandboxSource, _, fail := resolveStringLaunchField(
		"sandbox", "", resolvedHarness.Name, tiers,
		func(profile *db.SpawnProfile) string { return profile.Sandbox },
		func(raw string) (string, error) { return harness.ValidateSandboxMode(resolvedHarness, raw) },
	)
	if fail != nil {
		return sandboxProfileEnforcementTargetRequest{}, "", fmt.Errorf("%s", fail.Msg)
	}
	requestedImplementation, implementationSource, _, fail := resolveStringLaunchField(
		sandboxImplementationField, "", resolvedHarness.Name, tiers,
		func(profile *db.SpawnProfile) string { return profile.SandboxImplementation },
		func(raw string) (string, error) {
			return validateSandboxImplementationForHarness(resolvedHarness, raw)
		},
	)
	if fail != nil {
		return sandboxProfileEnforcementTargetRequest{}, "", fmt.Errorf("%s", fail.Msg)
	}
	implementation, err := sandboxpolicy.NormalizeImplementation(requestedImplementation)
	if err != nil {
		return sandboxProfileEnforcementTargetRequest{}, "", err
	}
	sandboxMode, err := harness.ResolveSandboxMode(resolvedHarness, requestedSandbox)
	if err != nil {
		return sandboxProfileEnforcementTargetRequest{}, "", err
	}
	if implementation == sandboxpolicy.ImplementationStacked {
		sandboxMode = predictedBuiltinMode(harnessName)
	}
	sandboxMode, err = harness.ResolveOpenCodeSandboxImplementationMode(
		resolvedHarness.Name, sandboxMode, implementation)
	if err != nil {
		return sandboxProfileEnforcementTargetRequest{}, "", err
	}

	sources := []string{}
	for _, source := range []string{harnessSource, sandboxSource, implementationSource} {
		if source == "" {
			continue
		}
		seen := false
		for _, existing := range sources {
			if existing == source {
				seen = true
				break
			}
		}
		if !seen {
			sources = append(sources, source)
		}
	}
	resolvedBy := strings.Join(sources, "; ")
	return sandboxProfileEnforcementTargetRequest{
		Implementation: string(implementation),
		Harness:        harnessName,
		Platform:       runtime.GOOS,
		Sandbox:        sandboxMode,
	}, resolvedBy, nil
}

func effectiveDraftSandboxProfileContexts(
	draft *db.SandboxProfile,
	originatingGroup string,
) ([]sandboxProfileEffectiveContext, int, error) {
	rows, err := db.ListSandboxProfiles()
	if err != nil {
		return nil, 0, err
	}
	registry := make(map[string]*sandboxpolicy.Profile, len(rows)+1)
	byID := make(map[int64]*db.SandboxProfile, len(rows))
	for _, row := range rows {
		byID[row.ID] = row
		if draft.ID > 0 && row.ID == draft.ID {
			continue
		}
		registry[row.Name] = sandboxProfileDBToPolicy(row)
	}
	registry[draft.Name] = sandboxProfileDBToPolicy(draft)
	global, err := db.GetGlobalSandboxProfile()
	if err != nil {
		return nil, 0, err
	}
	if draft.ID > 0 && global != nil && global.ID == draft.ID {
		global = draft
	}
	groups, err := db.ListAgentGroups()
	if err != nil {
		return nil, 0, err
	}
	type role struct {
		global, group, explicit *db.SandboxProfile
		groupName               string
	}
	roles := []role{}
	draftIsGlobal := draft.ID > 0 && global != nil && global.ID == draft.ID
	if draftIsGlobal {
		for _, group := range groups {
			roles = append(roles, role{
				global: draft, group: byID[group.SandboxProfileID], groupName: group.Name,
			})
		}
		if len(groups) == 0 {
			roles = append(roles, role{global: draft})
		}
	} else {
		for _, group := range groups {
			if draft.ID > 0 && group.SandboxProfileID == draft.ID {
				roles = append(roles, role{global: global, group: draft, groupName: group.Name})
			}
		}
	}
	if len(roles) == 0 {
		var groupProfile *db.SandboxProfile
		groupName := strings.TrimSpace(originatingGroup)
		if groupName != "" {
			for _, group := range groups {
				if group.Name == groupName {
					groupProfile = byID[group.SandboxProfileID]
					break
				}
			}
		}
		roles = append(roles, role{global: global, group: groupProfile, explicit: draft, groupName: groupName})
	}
	remaining := 0
	if len(roles) > 10 {
		remaining = len(roles) - 10
	}
	out := make([]sandboxProfileEffectiveContext, 0, len(roles))
	for _, item := range roles {
		flatten := func(profile *db.SandboxProfile) (*sandboxpolicy.Profile, []sandboxpolicy.AccessNotice, error) {
			if profile == nil {
				return nil, nil, nil
			}
			root := sandboxProfileDBToPolicy(profile)
			value, notices, flattenErr := sandboxpolicy.FlattenWithNotices(*root, func(name string) (*sandboxpolicy.Profile, error) {
				return registry[name], nil
			})
			return &value, notices, flattenErr
		}
		globalPolicy, globalNotices, err := flatten(item.global)
		if err != nil {
			return nil, 0, err
		}
		groupPolicy, groupNotices, err := flatten(item.group)
		if err != nil {
			return nil, 0, err
		}
		explicitPolicy, explicitNotices, err := flatten(item.explicit)
		if err != nil {
			return nil, 0, err
		}
		effective, err := sandboxpolicy.Resolve(sandboxpolicy.Scopes{
			Global: globalPolicy, Group: groupPolicy, Explicit: explicitPolicy,
		})
		if err != nil {
			return nil, 0, err
		}
		notices := []sandboxpolicy.AccessNotice{}
		for _, set := range [][]sandboxpolicy.AccessNotice{
			globalNotices, groupNotices, explicitNotices, effective.AccessNotices,
		} {
			for _, notice := range set {
				if notice.Class == sandboxpolicy.AccessNoticeClassComposition {
					notices = append(notices, notice)
				}
			}
		}
		effectivePolicy := sandboxpolicy.Profile{
			NetworkAccess: effective.NetworkAccess,
			Network:       effective.Network,
			UnixSockets:   effective.UnixSockets,
		}
		axes, err := sandboxpolicy.DeriveAccessAxes(effectivePolicy)
		if err != nil {
			return nil, 0, err
		}
		context := map[string]string{}
		if item.global != nil {
			context["global"] = item.global.Name
		}
		if item.group != nil {
			context["group"] = item.group.Name
		}
		if item.groupName != "" {
			context["group_name"] = item.groupName
		}
		if item.explicit != nil {
			context["explicit"] = item.explicit.Name
		}
		environment := make([]string, 0, len(effective.Environment))
		for _, entry := range effective.Environment {
			environment = append(environment, entry.Name)
		}
		policy := sandboxpolicy.Profile{
			Filesystem:       append([]sandboxpolicy.FilesystemGrant(nil), effective.Filesystem...),
			Environment:      append([]sandboxpolicy.EnvironmentEntry(nil), effective.Environment...),
			AgentDirectories: append([]string(nil), effective.AgentDirectories...),
			NetworkAccess:    effective.NetworkAccess,
			Network:          effective.Network,
			UnixSockets:      effective.UnixSockets,
		}
		out = append(out, sandboxProfileEffectiveContext{
			Context:          context,
			Filesystem:       policy.Filesystem,
			Environment:      environment,
			AgentDirectories: policy.AgentDirectories,
			Network:          axes.Network,
			UnixSockets:      axes.UnixSockets,
			AgentdSocket:     "always reachable",
			Notices:          notices,
			policy:           policy,
		})
	}
	return out, remaining, nil
}

func sandboxAssignmentCompositionNotices(
	global, group *db.SandboxProfile,
	includeGlobalNotices bool,
) ([]sandboxpolicy.AccessNotice, error) {
	rows, err := db.ListSandboxProfiles()
	if err != nil {
		return nil, err
	}
	registry := make(map[string]*sandboxpolicy.Profile, len(rows))
	for _, row := range rows {
		registry[row.Name] = sandboxProfileDBToPolicy(row)
	}
	flatten := func(profile *db.SandboxProfile) (*sandboxpolicy.Profile, []sandboxpolicy.AccessNotice, error) {
		if profile == nil {
			return nil, nil, nil
		}
		value, notices, flattenErr := sandboxpolicy.FlattenWithNotices(
			*sandboxProfileDBToPolicy(profile),
			func(name string) (*sandboxpolicy.Profile, error) { return registry[name], nil },
		)
		return &value, notices, flattenErr
	}
	globalPolicy, globalNotices, err := flatten(global)
	if err != nil {
		return nil, err
	}
	groupPolicy, groupNotices, err := flatten(group)
	if err != nil {
		return nil, err
	}
	effective, err := sandboxpolicy.Resolve(sandboxpolicy.Scopes{
		Global: globalPolicy, Group: groupPolicy,
	})
	if err != nil {
		return nil, err
	}
	out := []sandboxpolicy.AccessNotice{}
	sets := [][]sandboxpolicy.AccessNotice{groupNotices, effective.AccessNotices}
	if includeGlobalNotices {
		sets = append([][]sandboxpolicy.AccessNotice{globalNotices}, sets...)
	}
	for _, set := range sets {
		for _, notice := range set {
			if notice.Class == sandboxpolicy.AccessNoticeClassComposition {
				out = append(out, notice)
			}
		}
	}
	return out, nil
}

func globalSandboxAssignmentCompositionNotices(name string) ([]sandboxpolicy.AccessNotice, error) {
	global, err := db.GetSandboxProfile(strings.TrimSpace(name))
	if err != nil {
		return nil, err
	}
	if global == nil {
		return nil, db.ErrSandboxProfileNotFound
	}
	out, err := db.SandboxProfileCompositionNotices(global)
	if err != nil {
		return nil, err
	}
	intrinsicallyEmptyAxes := map[string]struct{}{}
	for _, notice := range out {
		if notice.Class == sandboxpolicy.AccessNoticeClassComposition {
			intrinsicallyEmptyAxes[notice.Axis] = struct{}{}
		}
	}
	groups, err := db.ListAgentGroups()
	if err != nil {
		return nil, err
	}
	for _, group := range groups {
		if group.SandboxProfileID == 0 {
			continue
		}
		groupProfile, err := db.GetSandboxProfileByID(group.SandboxProfileID)
		if err != nil {
			return nil, err
		}
		notices, err := sandboxAssignmentCompositionNotices(global, groupProfile, false)
		if err != nil {
			return nil, err
		}
		for _, notice := range notices {
			if _, alreadyEmpty := intrinsicallyEmptyAxes[notice.Axis]; alreadyEmpty {
				continue
			}
			notice.Detail = fmt.Sprintf("group %q: %s", group.Name, notice.Detail)
			out = append(out, notice)
		}
	}
	return out, nil
}

func groupSandboxAssignmentCompositionNotices(groupName, name string) ([]sandboxpolicy.AccessNotice, error) {
	group, err := db.GetSandboxProfile(strings.TrimSpace(name))
	if err != nil {
		return nil, err
	}
	if group == nil {
		return nil, db.ErrSandboxProfileNotFound
	}
	global, err := db.GetGlobalSandboxProfile()
	if err != nil {
		return nil, err
	}
	notices, err := sandboxAssignmentCompositionNotices(global, group, true)
	if err != nil {
		return nil, err
	}
	for i := range notices {
		notices[i].Detail = fmt.Sprintf("group %q: %s", groupName, notices[i].Detail)
	}
	return notices, nil
}

func flattenSandboxProfileForPrediction(profile *db.SandboxProfile) (sandboxpolicy.Profile, error) {
	registryRows, err := db.ListSandboxProfiles()
	if err != nil {
		return sandboxpolicy.Profile{}, err
	}
	registry := make(map[string]*sandboxpolicy.Profile, len(registryRows))
	for _, row := range registryRows {
		registry[row.Name] = sandboxProfileDBToPolicy(row)
	}
	root := registry[profile.Name]
	if root == nil {
		root = sandboxProfileDBToPolicy(profile)
	}
	return sandboxpolicy.Flatten(*root, func(name string) (*sandboxpolicy.Profile, error) {
		return registry[name], nil
	})
}

func sandboxProfileDBToPolicy(profile *db.SandboxProfile) *sandboxpolicy.Profile {
	if profile == nil {
		return nil
	}
	return &sandboxpolicy.Profile{
		Name: profile.Name, Filesystem: profile.Filesystem,
		FilesystemSpellings: profile.FilesystemSpellings,
		Environment:         profile.Environment, AgentDirectories: profile.AgentDirectories,
		NetworkAccess: profile.NetworkAccess, Network: profile.Network,
		UnixSockets: profile.UnixSockets, Includes: profile.Includes,
	}
}

func parseSandboxProfileEnforcementTarget(raw string) (parsedSandboxProfileEnforcementTarget, error) {
	raw = strings.TrimSpace(raw)
	parts := strings.Split(raw, "/")
	if raw == "" || len(parts) > 3 {
		return parsedSandboxProfileEnforcementTarget{}, invalidSandboxProfileTarget(raw)
	}
	for _, part := range parts {
		if strings.TrimSpace(part) == "" {
			return parsedSandboxProfileEnforcementTarget{}, invalidSandboxProfileTarget(raw)
		}
	}
	implementation, err := sandboxpolicy.NormalizeImplementation(parts[0])
	if err != nil {
		return parsedSandboxProfileEnforcementTarget{}, invalidSandboxProfileTarget(raw)
	}
	harnessName := harness.DefaultName
	if len(parts) >= 2 {
		harnessName = parts[1]
	}
	switch harnessName {
	case harness.DefaultName, harness.CodexName, harness.OpenCodeName:
	default:
		return parsedSandboxProfileEnforcementTarget{}, invalidSandboxProfileTarget(raw)
	}
	h, err := harness.Resolve(harnessName)
	if err != nil {
		return parsedSandboxProfileEnforcementTarget{}, invalidSandboxProfileTarget(raw)
	}
	platform := runtime.GOOS
	if len(parts) == 3 {
		platform = parts[2]
	}
	if platform != "linux" && platform != "darwin" {
		return parsedSandboxProfileEnforcementTarget{}, invalidSandboxProfileTarget(raw)
	}
	if implementation == sandboxpolicy.ImplementationStacked && platform != "linux" {
		return parsedSandboxProfileEnforcementTarget{}, fmt.Errorf(
			`invalid --for target %q: stacked sandbox prediction is supported only on linux`,
			raw,
		)
	}
	return parsedSandboxProfileEnforcementTarget{
		implementation: implementation, harness: h, platform: platform, raw: raw,
	}, nil
}

func invalidSandboxProfileTarget(raw string) error {
	return fmt.Errorf(
		`invalid --for target %q (want implementation[/harness[/platform]]; `+
			`implementation: harness-builtin, tclaude-layer, stacked; `+
			`harness: claude, codex, opencode; platform: linux, darwin)`,
		raw,
	)
}

func predictedBuiltinMode(harnessName string) string {
	switch harnessName {
	case harness.DefaultName:
		return harness.ClaudeSandboxOn
	case harness.CodexName:
		return harness.SandboxManagedProfile
	case harness.OpenCodeName:
		return harness.OpenCodeSandboxAccessControl
	default:
		return ""
	}
}

// resolveSandboxProfilePredictionMode mirrors launch-time OpenCode
// normalization. OpenCode's access-control mode is a command filter, not an OS
// sandbox, so the only target this evaluator accepts for it is the tclaude
// layer, reported truthfully as sandbox mode "tclaude-layer".
func resolveSandboxProfilePredictionMode(
	target parsedSandboxProfileEnforcementTarget,
	requested string,
) (string, error) {
	mode := predictedBuiltinMode(target.harness.Name)
	if strings.TrimSpace(requested) != "" {
		var err error
		mode, err = harness.ResolveSandboxMode(target.harness, requested)
		if err != nil {
			return "", err
		}
	}
	if target.implementation == sandboxpolicy.ImplementationStacked {
		mode = predictedBuiltinMode(target.harness.Name)
	}
	return harness.ResolveOpenCodeSandboxImplementationMode(
		target.harness.Name, mode, target.implementation)
}
