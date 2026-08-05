package agentd

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

type sandboxProfilePlanRequest struct {
	Agent          string `json:"agent,omitempty"`
	Group          string `json:"group,omitempty"`
	SandboxProfile string `json:"sandbox_profile,omitempty"`
	Cwd            string `json:"cwd,omitempty"`
	For            string `json:"for,omitempty"`
}

type sandboxProfilePlanTarget struct {
	Implementation string `json:"implementation"`
	Harness        string `json:"harness"`
	Platform       string `json:"platform"`
	ResolvedBy     string `json:"resolved_by,omitempty"`
}

type sandboxProfilePlanResponse struct {
	Source          string                         `json:"source"`
	Agent           string                         `json:"agent,omitempty"`
	Cwd             string                         `json:"cwd"`
	Target          sandboxProfilePlanTarget       `json:"target"`
	Profiles        []sandboxpolicy.AppliedProfile `json:"profiles"`
	PolicyRecorded  bool                           `json:"policy_recorded"`
	ProfilesOmitted bool                           `json:"profiles_omitted,omitempty"`
	RecordedAxes    *sandboxpolicy.ResolvedAxes    `json:"recorded_axes,omitempty"`
	Notices         []sandboxpolicy.AccessNotice   `json:"notices"`
	PredictedAxes   *harness.PredictedAccessAxes   `json:"predicted_axes,omitempty"`
	Plan            session.SandboxPlanDescription `json:"plan"`
}

// handleSandboxProfilePlan is read-only inspection. The hypothetical branch
// terminates at PredictAccessEnforcement + DescribePredictedAccess; it never
// obtains the opaque token required by PlanAccessEnforcement.
func handleSandboxProfilePlan(w http.ResponseWriter, r *http.Request) {
	if _, ok := requirePermission(w, r, PermSandboxProfilesManage); !ok {
		return
	}
	var body sandboxProfilePlanRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	if strings.TrimSpace(body.Agent) != "" {
		if strings.TrimSpace(body.Group) != "" || strings.TrimSpace(body.SandboxProfile) != "" ||
			strings.TrimSpace(body.Cwd) != "" || strings.TrimSpace(body.For) != "" {
			writeError(w, http.StatusBadRequest, "invalid_arg",
				"--agent cannot be combined with --group, --sandbox-profile, --cwd, or --for")
			return
		}
		response, err := recordedSandboxProfilePlan(strings.TrimSpace(body.Agent))
		writeSandboxProfilePlanResult(w, response, err)
		return
	}
	response, err := hypotheticalSandboxProfilePlan(body)
	writeSandboxProfilePlanResult(w, response, err)
}

func writeSandboxProfilePlanResult(w http.ResponseWriter, response sandboxProfilePlanResponse, err error) {
	if err == nil {
		writeJSON(w, http.StatusOK, response)
		return
	}
	status := http.StatusBadRequest
	code := "invalid_arg"
	if errors.Is(err, agent.ErrAmbiguous) {
		status = http.StatusConflict
		code = "ambiguous"
	}
	writeError(w, status, code, err.Error())
}

func recordedSandboxProfilePlan(selector string) (sandboxProfilePlanResponse, error) {
	// Inspection must not trigger ResolveSelector's miss-time project rescan
	// and index writes. Known agents/session rows are already SQLite-backed.
	resolved, _, err := agent.ResolveSelectorCached(selector)
	if err != nil {
		return sandboxProfilePlanResponse{}, err
	}
	row, err := db.FindSessionByConvID(resolved.ConvID)
	if err != nil {
		return sandboxProfilePlanResponse{}, err
	}
	if row == nil {
		return sandboxProfilePlanResponse{}, fmt.Errorf("no recorded session row for %q", selector)
	}
	implementation, err := sandboxpolicy.NormalizeImplementation(row.SandboxImplementation)
	if err != nil {
		return sandboxProfilePlanResponse{}, err
	}
	harnessName := strings.TrimSpace(row.Harness)
	if harnessName == "" {
		harnessName = harness.DefaultName
	}
	response := sandboxProfilePlanResponse{
		Source: "recorded",
		Agent:  resolved.AgentID,
		Cwd:    row.Cwd,
		Target: sandboxProfilePlanTarget{
			Implementation: string(implementation),
			Harness:        harnessName,
			Platform:       runtime.GOOS,
		},
		Profiles: []sandboxpolicy.AppliedProfile{},
		Notices:  []sandboxpolicy.AccessNotice{},
		Plan: session.SandboxPlanDescription{
			Applicable: false,
			Reason:     "selected implementation has no outer mount plan",
			Entries:    []session.SandboxPlanEntry{},
			Aliases:    []sandboxpolicy.MountAlias{},
		},
	}
	if implementation.UsesTclaudeLayer() {
		response.Plan.Reason = "effective sandbox snapshot was not recorded"
	}
	if row.EffectiveSandbox == nil {
		return response, nil
	}
	response.PolicyRecorded = true
	response.ProfilesOmitted = row.EffectiveSandbox.ProfilesOmitted
	response.Profiles = append(response.Profiles, row.EffectiveSandbox.Applied...)
	response.Notices = append(response.Notices, row.EffectiveSandbox.Effective.AccessNotices...)
	axes, err := sandboxpolicy.PlannedEffectiveAccessAxes(row.EffectiveSandbox.Effective)
	if err != nil {
		return sandboxProfilePlanResponse{}, err
	}
	response.RecordedAxes = &axes
	if !implementation.UsesTclaudeLayer() {
		return response, nil
	}
	response.Plan, err = session.DescribeRecordedEffectivePlan(
		row.EffectiveSandbox.Effective)
	return response, err
}

func hypotheticalSandboxProfilePlan(body sandboxProfilePlanRequest) (sandboxProfilePlanResponse, error) {
	groupID := int64(0)
	if groupName := strings.TrimSpace(body.Group); groupName != "" {
		group, err := db.GetAgentGroupByName(groupName)
		if err != nil {
			return sandboxProfilePlanResponse{}, err
		}
		if group == nil {
			return sandboxProfilePlanResponse{}, fmt.Errorf("no such group %q", groupName)
		}
		groupID = group.ID
	}
	snapshot, err := db.ResolveEffectiveSandboxSnapshot(
		groupID, strings.TrimSpace(body.SandboxProfile))
	if err != nil {
		return sandboxProfilePlanResponse{}, err
	}
	requested := sandboxProfileEnforcementTargetRequest{}
	resolvedBy := ""
	if raw := strings.TrimSpace(body.For); raw != "" {
		target, err := parseSandboxProfileEnforcementTarget(raw)
		if err != nil {
			return sandboxProfilePlanResponse{}, err
		}
		requested = sandboxProfileEnforcementTargetRequest{
			Implementation: string(target.implementation),
			Harness:        target.harness.Name,
			Platform:       target.platform,
		}
	} else {
		requested, resolvedBy, err = defaultSandboxProfilePredictionTarget(body.Group)
		if err != nil {
			return sandboxProfilePlanResponse{}, err
		}
	}
	rawTarget := strings.Join([]string{
		requested.Implementation, requested.Harness, requested.Platform,
	}, "/")
	target, err := parseSandboxProfileEnforcementTarget(rawTarget)
	if err != nil {
		return sandboxProfilePlanResponse{}, err
	}
	axes, err := sandboxpolicy.EffectiveAccessAxes(snapshot.Effective)
	if err != nil {
		return sandboxProfilePlanResponse{}, err
	}
	mode, err := resolveSandboxProfilePredictionMode(target, requested.Sandbox)
	if err != nil {
		return sandboxProfilePlanResponse{}, err
	}
	prediction, err := harness.PredictAccessEnforcement(
		target.harness, target.implementation, axes,
		mode, target.platform)
	if err != nil {
		return sandboxProfilePlanResponse{}, err
	}
	policy := sandboxpolicy.Profile{
		Filesystem:       append([]sandboxpolicy.FilesystemGrant(nil), snapshot.Effective.Filesystem...),
		Environment:      append([]sandboxpolicy.EnvironmentEntry(nil), snapshot.Effective.Environment...),
		AgentDirectories: append([]string(nil), snapshot.Effective.AgentDirectories...),
		NetworkAccess:    snapshot.Effective.NetworkAccess,
		Network:          snapshot.Effective.Network,
		UnixSockets:      snapshot.Effective.UnixSockets,
	}
	described := describePredictedSandboxProfile(
		policy, target, mode,
		harness.DescribePredictedAccess(axes, prediction),
	)
	planReason := ""
	if !target.implementation.UsesTclaudeLayer() {
		// Name the implementation actually selected. Hardcoding harness-builtin
		// here misreported `off`, and would now misreport `resource-only` too:
		// the reason the operator reads should be about the posture they chose.
		planReason = fmt.Sprintf("%s has no outer mount plan", target.implementation)
	}
	response := sandboxProfilePlanResponse{
		Source: "hypothetical",
		Cwd:    strings.TrimSpace(body.Cwd),
		Target: sandboxProfilePlanTarget{
			Implementation: string(target.implementation),
			Harness:        target.harness.Name,
			Platform:       target.platform,
			ResolvedBy:     resolvedBy,
		},
		Profiles:       append([]sandboxpolicy.AppliedProfile(nil), snapshot.Applied...),
		PolicyRecorded: true,
		Notices: append([]sandboxpolicy.AccessNotice(nil),
			snapshot.Effective.AccessNotices...),
		PredictedAxes: &described,
		Plan: session.SandboxPlanDescription{
			Applicable: target.implementation.UsesTclaudeLayer(),
			Reason:     planReason,
			Entries:    []session.SandboxPlanEntry{},
			Aliases:    []sandboxpolicy.MountAlias{},
		},
	}
	if !target.implementation.UsesTclaudeLayer() {
		return response, nil
	}
	if target.platform != runtime.GOOS {
		response.Plan.Applicable = false
		response.Plan.Reason = fmt.Sprintf(
			"mount plan inspection requires daemon host platform %s; access prediction for %s is shown above",
			runtime.GOOS, target.platform)
		return response, nil
	}
	cwd := strings.TrimSpace(body.Cwd)
	if cwd == "" {
		return sandboxProfilePlanResponse{}, errors.New("--cwd is required for a hypothetical plan")
	}
	gitCommonDir, err := harness.GitCommonDir(cwd)
	if err != nil {
		return sandboxProfilePlanResponse{}, err
	}
	gitDir, err := harness.GitDir(cwd)
	if err != nil {
		return sandboxProfilePlanResponse{}, err
	}
	home, _ := os.UserHomeDir()
	spec, err := session.BuildTclaudeLayerLaunchSpec(session.TclaudeLayerLaunchInput{
		HarnessName: target.harness.Name,
		Cwd:         cwd,
		GitWriteDirs: harness.GitWorktreeWriteDirsForIdentity(
			gitCommonDir, gitDir, home),
		Snapshot: &snapshot,
	})
	if err != nil {
		return sandboxProfilePlanResponse{}, err
	}
	response.Plan, err = session.DescribeTclaudeLayerPlan(
		spec, snapshot.Effective)
	return response, err
}
