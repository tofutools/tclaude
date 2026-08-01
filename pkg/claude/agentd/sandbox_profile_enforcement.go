package agentd

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"slices"
	"sort"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// sandboxProfileEnforcementRefusal is the preview's per-target rendering of a
// typed *harness.SandboxCapabilityError: this target's harness cannot faithfully
// enforce this policy, so a launch against it would be refused outright.
//
// It is deliberately NOT expressed as an axis verdict. The refusal is a property
// of the (policy, target) pair decided before any individual rule was judged, so
// there are no axes to report; presenting one would claim a per-rule verdict the
// evaluator never produced. Consumers must therefore branch on this field before
// reading Axes — a refused target carries the zero PredictedAccessAxes, which is
// indistinguishable from an old daemon's missing axes.
//
// NetworkEntries is the exception to "carries no verdict": it holds the
// DRAFT-ONLY prediction, which succeeds independently of this refusal, so a
// refused target may still have non-empty entries there. They describe the
// authored draft, not the refused (policy, target) pair, and must not be read
// as verdicts for it. ContextNetworkEntries carries an explicit nil at a
// refused index to stay index-aligned.
//
// The dashboard currently resolves that nil with `??`, which treats it as
// absent and falls back to these draft-only rows. That is unreachable in
// practice, because the renderer returns on the refusal before the value is
// read. It is a known gap, tracked in TCL-914, rather than hardened here — an
// attempt to guard it in this PR was reverted for being more error-prone than
// the path it protected. TCL-914 also covers the underlying defect: the
// renderer and sandboxPolicyNeedsAttention derive this value independently and
// can disagree.
type sandboxProfileEnforcementRefusal struct {
	Kind    string `json:"kind"`
	Harness string `json:"harness,omitempty"`
	Message string `json:"message"`
}

// sandboxProfilePredictionRefusal is the ONE rule for which prediction errors
// become a per-target refusal row instead of failing the whole request.
//
// Per-target: a typed *harness.SandboxCapabilityError. It says a well-formed
// target cannot enforce a well-formed policy, which is a verdict about that
// target alone; the other targets in the same request are unaffected and keep
// their normal rows.
//
// Whole-request: everything else. An unparseable --for target, an unresolvable
// sandbox mode, and an invalid profile or draft are all malformed REQUESTS —
// they are detected before prediction and there is no valid target to attach a
// row to. An untyped error out of PredictAccessEnforcement means the evaluator's
// own contract was violated (for instance capabilities that never passed the
// implementation gate); degrading that to a row would report a broken evaluator
// as a policy verdict.
func sandboxProfilePredictionRefusal(err error) *sandboxProfileEnforcementRefusal {
	var capability *harness.SandboxCapabilityError
	if !errors.As(err, &capability) {
		return nil
	}
	return &sandboxProfileEnforcementRefusal{
		Kind:    capability.Kind,
		Harness: capability.Harness,
		Message: capability.Message,
	}
}

type sandboxProfileEnforcementTarget struct {
	Implementation string                      `json:"implementation"`
	Harness        string                      `json:"harness"`
	Platform       string                      `json:"platform"`
	Predicted      bool                        `json:"predicted"`
	Axes           harness.PredictedAccessAxes `json:"axes"`
	Caveat         string                      `json:"caveat,omitempty"`
	// Refusal is set exactly when Predicted is false.
	Refusal *sandboxProfileEnforcementRefusal `json:"refusal,omitempty"`
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
	Target                sandboxProfileEnforcementTargetRequest `json:"target"`
	ResolvedBy            string                                 `json:"resolved_by,omitempty"`
	Predicted             bool                                   `json:"predicted"`
	Axes                  harness.PredictedAccessAxes            `json:"axes"`
	NetworkEntries        []harness.PredictedNetworkEntry        `json:"network_entries,omitempty"`
	ContextAxes           []harness.PredictedAccessAxes          `json:"context_axes,omitempty"`
	ContextNetworkEntries [][]harness.PredictedNetworkEntry      `json:"context_network_entries,omitempty"`
	// Refusal is set exactly when Predicted is false: this target refuses the
	// whole policy, and Axes/ContextAxes carry no verdict.
	Refusal *sandboxProfileEnforcementRefusal `json:"refusal,omitempty"`
	// ContextRefusals is index-aligned with ContextAxes. A non-nil entry means
	// that effective assignment context refuses on this target while the others
	// keep their ordinary rows; the aligned ContextAxes entry is the zero value
	// and must not be read as a verdict.
	ContextRefusals []*sandboxProfileEnforcementRefusal `json:"context_refusals,omitempty"`
	// OmittedRefusals carries refusals from assignment contexts beyond the
	// display cap. Those contexts have no index in ContextAxes and contribute
	// nothing to the aggregate (which summarizes surviving contexts only), so
	// without this field a refusal past the cap would be invisible while the
	// editor still claims every assignment was checked.
	OmittedRefusals []*sandboxProfileEnforcementRefusal `json:"omitted_refusals,omitempty"`
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

// sandboxProfilePredictionCaveat qualifies a prediction that could not be
// checked against this host. Shared by the enforced and the refused row so the
// two are stated with the same confidence.
func sandboxProfilePredictionCaveat(target parsedSandboxProfileEnforcementTarget) string {
	if target.platform != runtime.GOOS {
		return "(prediction for a non-host platform; host capability probes did not run)"
	}
	if !target.implementation.UsesTclaudeLayer() {
		return ""
	}
	// Disclosure, like the dashboard catalog: this endpoint predicts and cannot
	// mint a launch token, so it reads the fork-free presence table rather than
	// executing the sandbox engine per request.
	if err := sandboxToolPresence(
		sandboxToolLayerHost, tclaudeLayerHostPresence,
	); err != nil {
		return "(prediction only; the target's host tooling is not installed: " + err.Error() + ")"
	}
	return ""
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
			// A capability conflict is this target's verdict, not this request's
			// error: the remaining --for targets are unaffected and still render.
			if refusal := sandboxProfilePredictionRefusal(err); refusal != nil {
				response.Targets = append(response.Targets, sandboxProfileEnforcementTarget{
					Implementation: string(target.implementation),
					Harness:        target.harness.Name,
					Platform:       target.platform,
					Predicted:      false,
					Refusal:        refusal,
					// A refusal is a prediction like any other, so it inherits the
					// same qualification. Stating it unqualified would make the
					// refused row read MORE confidently than the enforced rows
					// beside it, on a host that ran no capability probes at all.
					Caveat: sandboxProfilePredictionCaveat(target),
				})
				continue
			}
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
		item.Caveat = sandboxProfilePredictionCaveat(target)
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
		resolvedTarget := sandboxProfileEnforcementTargetRequest{
			Implementation: string(target.implementation),
			Harness:        target.harness.Name,
			Platform:       target.platform,
			Sandbox:        mode,
		}
		// Only the COMPOSED policy decides whether this target is refused: that is
		// the policy a launch actually carries.
		predicted, predictErr := harness.PredictAccessEnforcement(
			target.harness, target.implementation, axes,
			mode, target.platform,
		)
		if predictErr != nil {
			if refusal := sandboxProfilePredictionRefusal(predictErr); refusal != nil {
				response.Targets = append(response.Targets, sandboxProfileDraftEnforcementTarget{
					Target: resolvedTarget, ResolvedBy: resolvedBy,
					Predicted: false, Refusal: refusal,
				})
				continue
			}
			writeError(w, http.StatusBadRequest, "invalid_arg", predictErr.Error())
			return
		}
		// The draft-ONLY axes (the draft without its includes) feed nothing but the
		// authoring-level network rows. No launch ever uses that policy, so a
		// capability conflict here must NOT refuse the target: the composed policy
		// above is what a launch carries, and it already passed. Refusing on this
		// evaluation would claim a launch refusal the launch path would not make —
		// reachable whenever an include contributes a deny that removes the
		// offending grant from the composed policy. The authored rows are simply
		// omitted; the composed verdict stands.
		draftPredicted, predictErr := harness.PredictAccessEnforcement(
			target.harness, target.implementation, draftAxes,
			mode, target.platform,
		)
		draftOnlyRefused := false
		if predictErr != nil {
			if sandboxProfilePredictionRefusal(predictErr) == nil {
				writeError(w, http.StatusBadRequest, "invalid_arg", predictErr.Error())
				return
			}
			draftOnlyRefused = true
		}
		described, contextAxes, contextRefusals, contextNetworkEntries, describeErr :=
			describePredictedDraftSandboxProfile(
				flattened, contexts, target, mode,
				harness.DescribePredictedAccess(axes, predicted),
			)
		if describeErr != nil {
			writeError(w, http.StatusBadRequest, "invalid_sandbox_profile", describeErr.Error())
			return
		}
		// Every context refusing makes the TARGET refused — there is no surviving
		// verdict to aggregate. It does NOT make the individual refusals
		// redundant: distinct contexts routinely refuse for distinct reasons,
		// because the resolver refusal embeds the offending selector and resolver
		// path. Reporting only the first would force the operator to fix it,
		// re-preview, and discover the next — which is precisely the fix-and-
		// re-preview cost this ticket exists to remove, one level down. So the
		// per-context lists stay populated and every distinct reason is carried.
		targetRefusal := onlyContextRefusals(contextRefusals)
		// The display cap truncates the per-context lists, but a refusal must not
		// be what gets dropped. The aggregate axes deliberately summarize only the
		// SURVIVING contexts, so a refused context contributes nothing there
		// either — truncating it away as well would leave the omitted assignments
		// with no representation at all, while the editor tells the operator they
		// "are still included in the overall safety check". Carry them separately.
		omittedRefusals := []*sandboxProfileEnforcementRefusal{}
		for _, refusal := range contextRefusals[min(len(contextRefusals), len(response.Contexts)):] {
			if refusal != nil {
				omittedRefusals = append(omittedRefusals, refusal)
			}
		}
		if len(contextRefusals) > len(response.Contexts) {
			contextRefusals = contextRefusals[:len(response.Contexts)]
		}
		if len(contextAxes) > len(response.Contexts) {
			contextAxes = contextAxes[:len(response.Contexts)]
		}
		if len(contextNetworkEntries) > len(response.Contexts) {
			contextNetworkEntries = contextNetworkEntries[:len(response.Contexts)]
		}
		response.Targets = append(response.Targets, sandboxProfileDraftEnforcementTarget{
			Target:     resolvedTarget,
			ResolvedBy: resolvedBy,
			Predicted:  targetRefusal == nil,
			Refusal:    targetRefusal,
			Axes:       described,
			NetworkEntries: draftOnlyNetworkEntries(
				draftOnlyRefused, draftAxes.Network, draftPredicted, body.Draft.Network,
			),
			ContextAxes:           contextAxes,
			ContextRefusals:       contextRefusals,
			OmittedRefusals:       omittedRefusals,
			ContextNetworkEntries: contextNetworkEntries,
		})
	}
	writeJSON(w, http.StatusOK, response)
}

// onlyContextRefusals reports the first refusal when EVERY effective assignment
// context refused, and nil as soon as one context survived with real axes.
func onlyContextRefusals(
	refusals []*sandboxProfileEnforcementRefusal,
) *sandboxProfileEnforcementRefusal {
	if len(refusals) == 0 {
		return nil
	}
	for _, refusal := range refusals {
		if refusal == nil {
			return nil
		}
	}
	return refusals[0]
}

// draftOnlyNetworkEntries suppresses the authored network rows when the
// draft-only evaluation refused. Those rows describe per-entry capability
// against a policy no launch uses; rendering them from a refused evaluation
// would show verdicts that were never computed.
func draftOnlyNetworkEntries(
	refused bool,
	rules sandboxpolicy.NetworkRules,
	caps harness.PredictedAccessEnforcement,
	authored *sandboxpolicy.NetworkRules,
) []harness.PredictedNetworkEntry {
	if refused {
		return nil
	}
	return predictedDraftNetworkEntries(rules, caps, authored)
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
	denyRows := harness.DescribePredictedNetworkDenyEntries(
		denyEntries, caps)
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
	flattened, _, err := flattenSandboxProfileForDraftInspection(draft, registry)
	return flattened, err
}

// flattenSandboxProfileForDraftInspection is deliberately limited to the
// editor's inspection-only evaluator. A missing include cannot contribute its
// rules, so the returned policy is necessarily incomplete; the composition
// notice makes that fact explicit instead of turning the whole preview into an
// opaque request error. Persistence and launch resolution keep using strict
// graph validation and strict sandboxpolicy.Flatten.
func flattenSandboxProfileForDraftInspection(
	profile *db.SandboxProfile,
	registry map[string]*sandboxpolicy.Profile,
) (sandboxpolicy.Profile, []sandboxpolicy.AccessNotice, error) {
	missing := map[string]struct{}{}
	root := sandboxProfileDBToPolicy(profile)
	value, notices, err := sandboxpolicy.FlattenWithNotices(
		*root,
		func(name string) (*sandboxpolicy.Profile, error) {
			if included := registry[name]; included != nil {
				return included, nil
			}
			missing[name] = struct{}{}
			// An empty stand-in lets inspection continue without pretending the
			// unresolved layer contributed any authority.
			return &sandboxpolicy.Profile{Name: name}, nil
		},
	)
	if err != nil {
		return sandboxpolicy.Profile{}, nil, err
	}
	names := make([]string, 0, len(missing))
	for name := range missing {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		notices = sandboxpolicy.MergeAccessNotices(notices, sandboxpolicy.AccessNotice{
			Class:  sandboxpolicy.AccessNoticeClassComposition,
			Axis:   "includes",
			Reason: sandboxpolicy.AccessNoticeReasonMissingInclude,
			Effect: sandboxpolicy.AccessNoticeEffectPreviewIncomplete,
			Detail: fmt.Sprintf(
				"included sandbox profile %q was not found in registry; its rules are absent from this preview",
				name,
			),
		})
	}
	return value, notices, nil
}

// defaultSandboxProfilePredictionTarget is the preview's launch target when the
// operator has overridden none of the target controls. It shares
// resolveLaunchDefaults with the spawn dialog so the preview and the dialog can
// never disagree about what a launch would resolve, then applies the two
// adjustments that belong to PREDICTION rather than to launch resolution: under
// stacked the harness's own built-in mode is what gets predicted, and OpenCode's
// implementation-specific mode has to be derived.
func defaultSandboxProfilePredictionTarget(groupName string) (sandboxProfileEnforcementTargetRequest, string, error) {
	defaults, fail, err := resolveLaunchDefaults(groupName, "", "")
	if fail != nil {
		return sandboxProfileEnforcementTargetRequest{}, "", fmt.Errorf("%s", fail.Msg)
	}
	if err != nil {
		return sandboxProfileEnforcementTargetRequest{}, "", err
	}
	sandboxMode := defaults.sandbox
	if defaults.implementation == sandboxpolicy.ImplementationStacked {
		sandboxMode = predictedBuiltinMode(defaults.harness.Name)
	}
	sandboxMode, err = harness.ResolveSandboxImplementationMode(
		defaults.harness, sandboxMode, defaults.implementation)
	if err != nil {
		return sandboxProfileEnforcementTargetRequest{}, "", err
	}
	return sandboxProfileEnforcementTargetRequest{
		Implementation: string(defaults.implementation),
		Harness:        defaults.harness.Name,
		Platform:       runtime.GOOS,
		Sandbox:        sandboxMode,
	}, defaults.resolvedBy, nil
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
			value, notices, flattenErr := flattenSandboxProfileForDraftInspection(profile, registry)
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
			`implementation: off, harness-builtin, tclaude-layer, stacked; `+
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
	return harness.ResolveSandboxImplementationMode(
		target.harness, mode, target.implementation)
}
