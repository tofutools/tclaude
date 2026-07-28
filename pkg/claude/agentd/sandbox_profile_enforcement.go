package agentd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"strings"

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
	Target     sandboxProfileEnforcementTargetRequest `json:"target"`
	ResolvedBy string                                 `json:"resolved_by,omitempty"`
	Predicted  bool                                   `json:"predicted"`
	Axes       harness.PredictedAccessAxes            `json:"axes"`
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
		mode := predictedBuiltinMode(target.harness.Name)
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
		predicted, predictErr := harness.PredictAccessEnforcement(
			target.harness, target.implementation, axes,
			predictedBuiltinMode(target.harness.Name), target.platform,
		)
		if predictErr != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", predictErr.Error())
			return
		}
		described := describePredictedDraftSandboxProfile(
			flattened, contexts, target, predictedBuiltinMode(target.harness.Name),
			harness.DescribePredictedAccess(axes, predicted),
		)
		response.Targets = append(response.Targets, sandboxProfileDraftEnforcementTarget{
			Target: sandboxProfileEnforcementTargetRequest{
				Implementation: string(target.implementation),
				Harness:        target.harness.Name,
				Platform:       target.platform,
			},
			ResolvedBy: resolvedBy,
			Predicted:  true,
			Axes:       described,
		})
	}
	writeJSON(w, http.StatusOK, response)
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
	profile := globalDefaultProfile()
	resolvedBy := ""
	if profile != nil {
		resolvedBy = fmt.Sprintf("dashboard default spawn profile %q", profile.Name)
	}
	if profile == nil && strings.TrimSpace(groupName) != "" {
		group, err := db.GetAgentGroupByName(strings.TrimSpace(groupName))
		if err != nil {
			return sandboxProfileEnforcementTargetRequest{}, "", err
		}
		profile = groupDefaultProfile(group)
		if profile != nil {
			resolvedBy = fmt.Sprintf("group default spawn profile %q", profile.Name)
		}
	}
	harnessName := harness.DefaultName
	implementation := string(sandboxpolicy.ImplementationHarnessBuiltin)
	if profile != nil {
		if strings.TrimSpace(profile.Harness) != "" {
			harnessName = strings.TrimSpace(profile.Harness)
		}
		if strings.TrimSpace(profile.SandboxImplementation) != "" {
			implementation = strings.TrimSpace(profile.SandboxImplementation)
		}
	}
	if resolvedBy == "" {
		resolvedBy = "harness default"
	}
	return sandboxProfileEnforcementTargetRequest{
		Implementation: implementation,
		Harness:        harnessName,
		Platform:       runtime.GOOS,
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
	if draft.ID > 0 && global != nil && global.ID == draft.ID {
		roles = append(roles, role{global: draft})
	}
	for _, group := range groups {
		if draft.ID > 0 && group.SandboxProfileID == draft.ID {
			roles = append(roles, role{global: global, group: draft, groupName: group.Name})
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
		roles = roles[:10]
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
