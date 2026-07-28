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
	"github.com/tofutools/tclaude/pkg/claude/resumeprovenance"
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
	Source        string                         `json:"source"`
	Agent         string                         `json:"agent,omitempty"`
	Cwd           string                         `json:"cwd"`
	Target        sandboxProfilePlanTarget       `json:"target"`
	Profiles      []sandboxpolicy.AppliedProfile `json:"profiles"`
	RecordedAxes  *sandboxpolicy.ResolvedAxes    `json:"recorded_axes,omitempty"`
	Notices       []sandboxpolicy.AccessNotice   `json:"notices"`
	PredictedAxes *harness.PredictedAccessAxes   `json:"predicted_axes,omitempty"`
	Plan          session.SandboxPlanDescription `json:"plan"`
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
	resolved, _, err := agent.ResolveSelector(selector)
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
			Applicable: implementation.UsesTclaudeLayer(),
			Entries:    []session.SandboxPlanEntry{},
			Aliases:    []sandboxpolicy.MountAlias{},
		},
	}
	if row.EffectiveSandbox == nil {
		return response, nil
	}
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
	gitWriteDirs := recordedGitWriteDirs(row.ResumeProvenance)
	spec, err := session.BuildTclaudeLayerLaunchSpec(session.TclaudeLayerLaunchInput{
		HarnessName:  harnessName,
		Cwd:          row.Cwd,
		GitWriteDirs: gitWriteDirs,
		Snapshot:     row.EffectiveSandbox,
	})
	if err != nil {
		return sandboxProfilePlanResponse{}, err
	}
	response.Plan, err = session.DescribeTclaudeLayerPlan(spec)
	return response, err
}

func recordedGitWriteDirs(raw string) []string {
	provenance, err := resumeprovenance.Decode(raw)
	if err != nil || provenance.RepositoryState != resumeprovenance.RepositoryGit {
		return nil
	}
	home, _ := os.UserHomeDir()
	return harness.GitWorktreeWriteDirsForIdentity(
		provenance.Repository.CommonDir.Path, provenance.Repository.Dir.Path, home)
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
	prediction, err := harness.PredictAccessEnforcement(
		target.harness, target.implementation, axes,
		predictedBuiltinMode(target.harness.Name), target.platform)
	if err != nil {
		return sandboxProfilePlanResponse{}, err
	}
	described := harness.DescribePredictedAccess(axes, prediction)
	response := sandboxProfilePlanResponse{
		Source: "hypothetical",
		Cwd:    strings.TrimSpace(body.Cwd),
		Target: sandboxProfilePlanTarget{
			Implementation: string(target.implementation),
			Harness:        target.harness.Name,
			Platform:       target.platform,
			ResolvedBy:     resolvedBy,
		},
		Profiles:      append([]sandboxpolicy.AppliedProfile(nil), snapshot.Applied...),
		Notices:       []sandboxpolicy.AccessNotice{},
		PredictedAxes: &described,
		Plan: session.SandboxPlanDescription{
			Applicable: target.implementation.UsesTclaudeLayer(),
			Entries:    []session.SandboxPlanEntry{},
			Aliases:    []sandboxpolicy.MountAlias{},
		},
	}
	if !target.implementation.UsesTclaudeLayer() {
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
	response.Plan, err = session.DescribeTclaudeLayerPlan(spec)
	return response, err
}
