package agentd

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/process/engine"
	"github.com/tofutools/tclaude/pkg/claude/process/executor"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// process_program_bounds.go narrows program steps to their run's scope
// (tcl-mm9q5n). Scope only ever narrows what a performer may do, so a program
// step in an agent-scoped run runs where that agent works and inside the
// sandbox it runs under; in a group-scoped run it takes the group's default
// spawn profile. See "Where program steps run" in docs/processes.md.
//
// The bounds are resolved by the run's owner when it plans the attempt — a
// few SQLite reads, no filesystem work — and recorded on the attempt's
// evidence. The sandbox itself is rendered on the program's worker, right
// before it starts. Either step failing fails the attempt closed: a scope
// whose bounds cannot be enforced never runs its program unconfined instead.

// processProgramLayerWrap renders argv to run in cwd inside tclaude's own
// sandbox for snapshot. Tests swap it to stay off the host's bwrap/Seatbelt.
var processProgramLayerWrap = wrapProcessProgramWithTclaudeLayer

// processProgramBoundsResolver is the resolver a run's owner installs.
func processProgramBoundsResolver(runID string) executor.ProgramBoundsResolver {
	return func(engine.Command) *executor.ProgramBounds {
		return resolveProcessProgramBounds(runID)
	}
}

// resolveProcessProgramBounds is where a program step of runID runs now. Nil
// is an unscoped run: unchanged, a plain child of the daemon.
func resolveProcessProgramBounds(runID string) *executor.ProgramBounds {
	scope, err := db.GetProcessRunScope(runID)
	if err != nil {
		return &executor.ProgramBounds{Sandbox: executor.ProgramTclaudeLayer,
			Error: "read the run's scope: " + err.Error()}
	}
	switch scope.Kind {
	case db.ProcessRunScopeAgent:
		return agentProgramBounds(scope.AgentID)
	case db.ProcessRunScopeGroup:
		return groupProgramBounds(scope.GroupID)
	default:
		return nil
	}
}

// agentProgramBounds runs a program in the bound agent's working directory,
// inside the sandbox its current generation was launched with, enforced by
// tclaude's own layer whichever layer confines the agent itself.
func agentProgramBounds(agentID string) *executor.ProgramBounds {
	bounds := &executor.ProgramBounds{Scope: db.ProcessRunScopeAgent, Sandbox: executor.ProgramTclaudeLayer}
	bound, err := db.GetAgent(agentID)
	if err != nil {
		bounds.Error = "load the bound agent: " + err.Error()
		return bounds
	}
	if bound == nil || !bound.Active() || bound.CurrentConvID == "" {
		bounds.Error = "the bound agent is retired"
		return bounds
	}
	recorded, err := recordedSeanceLaunchForConv(bound.CurrentConvID)
	if err != nil {
		bounds.Error = "load the agent's launch record: " + err.Error()
		return bounds
	}
	bounds.Cwd = recorded.cwd
	if bounds.Cwd == "" {
		bounds.Error = "the agent's working directory is unknown"
		return bounds
	}
	row := recorded.row
	if row == nil {
		bounds.Error = "the agent's launch record is missing"
		return bounds
	}
	implementation, err := sandboxpolicy.NormalizeImplementation(row.SandboxImplementation)
	if err != nil {
		bounds.Error = err.Error()
		return bounds
	}
	if reason := agentProgramUnconfinedReason(implementation, recorded.harness, recorded.sandbox, row.OSSandboxState); reason != "" {
		bounds.Sandbox, bounds.Reason = executor.ProgramUnconfined, reason
		return bounds
	}
	snapshot := row.EffectiveSandbox
	if snapshot == nil {
		if implementation.UsesTclaudeLayer() {
			bounds.Error = "the agent's sandbox snapshot is not recorded"
			return bounds
		}
		// A harness-sandboxed agent with no tclaude profile: the layer's own
		// defaults stand in for the harness's sandbox.
		empty := sandboxpolicy.EmptySnapshot()
		snapshot = &empty
	}
	bounds.Profiles = appliedSandboxProfileNames(snapshot)
	bounds.Reason = fmt.Sprintf("the agent's sandbox (%s)", implementation) + resourceLimitsNotice(snapshot)
	bounds.Wrap = layerProgramWrap(bounds.Cwd, snapshot)
	return bounds
}

// agentProgramUnconfinedReason says why an agent runs with no OS sandbox at
// all, or "" when it is confined. Scope never widens, and there is nothing to
// narrow to, so such an agent's program steps run unconfined in its cwd.
func agentProgramUnconfinedReason(implementation sandboxpolicy.Implementation, harnessName, harnessMode, osSandboxState string) string {
	if implementation.OmitsOSConfinement() {
		return fmt.Sprintf("the agent runs with --sandbox-impl %s", implementation)
	}
	if implementation.UsesTclaudeLayer() {
		return ""
	}
	if sandboxProfilesDisabled(harnessName, harnessMode) {
		return fmt.Sprintf("the agent runs with --sandbox %s", harnessMode)
	}
	switch strings.TrimSpace(osSandboxState) {
	case "off", "unconfigured":
		return "the agent's own sandbox is off"
	}
	return ""
}

// groupProgramBounds runs a program as the group's default spawn profile
// would launch an agent: in the group's default working directory, inside
// the group's sandbox profiles. A group with no default spawn profile keeps
// the unscoped behaviour, disclosed as unconfined.
func groupProgramBounds(groupID int64) *executor.ProgramBounds {
	bounds := &executor.ProgramBounds{Scope: db.ProcessRunScopeGroup, Sandbox: executor.ProgramTclaudeLayer}
	g, err := db.GetAgentGroupByID(groupID)
	if err != nil {
		bounds.Error = "load the group: " + err.Error()
		return bounds
	}
	if g == nil {
		bounds.Error = "the group is gone"
		return bounds
	}
	if strings.TrimSpace(g.DefaultProfile) == "" {
		bounds.Sandbox, bounds.Reason = executor.ProgramUnconfined, "the group has no default spawn profile"
		return bounds
	}
	// Not groupDefaultProfile: a spawn may proceed past an unreadable or
	// missing default, but a program must not run unconfined because of one.
	profile, err := db.ResolveSpawnProfile(g.DefaultProfile)
	if err != nil {
		bounds.Error = fmt.Sprintf("load the group's default spawn profile %q: %v", g.DefaultProfile, err)
		return bounds
	}
	if profile == nil {
		bounds.Error = fmt.Sprintf("the group's default spawn profile %q no longer exists", g.DefaultProfile)
		return bounds
	}
	bounds.Cwd = strings.TrimSpace(g.DefaultCwd)
	implementation, err := sandboxpolicy.NormalizeImplementation(profile.SandboxImplementation)
	if err != nil {
		bounds.Error = err.Error()
		return bounds
	}
	if implementation.OmitsOSConfinement() {
		bounds.Sandbox = executor.ProgramUnconfined
		bounds.Reason = fmt.Sprintf("the group's default spawn profile %q runs with --sandbox-impl %s", profile.Name, implementation)
		return bounds
	}
	if !implementation.UsesTclaudeLayer() && sandboxProfilesDisabled(profile.Harness, profile.Sandbox) {
		bounds.Sandbox = executor.ProgramUnconfined
		bounds.Reason = fmt.Sprintf("the group's default spawn profile %q runs with --sandbox %s", profile.Name, profile.Sandbox)
		return bounds
	}
	if bounds.Cwd == "" {
		bounds.Error = fmt.Sprintf("group %q has no default working directory to confine its program steps to", g.Name)
		return bounds
	}
	snapshot, err := db.ResolveEffectiveSandboxSnapshot(g.ID, "")
	if err != nil {
		bounds.Error = "resolve the group's sandbox profiles: " + err.Error()
		return bounds
	}
	bounds.Profiles = appliedSandboxProfileNames(&snapshot)
	bounds.Reason = fmt.Sprintf("the group's default spawn profile %q", profile.Name) + resourceLimitsNotice(&snapshot)
	bounds.Wrap = layerProgramWrap(bounds.Cwd, &snapshot)
	return bounds
}

// resourceLimitsNotice discloses that a profile's resource limits do not
// reach program steps: tclaude's layer confines access here, but the cgroup a
// launch applies limits through is not set up for a program attempt yet.
func resourceLimitsNotice(snapshot *sandboxpolicy.Snapshot) string {
	if snapshot == nil || !snapshot.Effective.ResourceLimits.Enabled() {
		return ""
	}
	return "; its resource limits are not applied to program steps"
}

func appliedSandboxProfileNames(snapshot *sandboxpolicy.Snapshot) []string {
	if snapshot == nil {
		return nil
	}
	names := make([]string, 0, len(snapshot.Applied))
	for _, applied := range snapshot.Applied {
		if name := strings.TrimSpace(applied.Name); name != "" {
			names = append(names, name)
		}
	}
	return names
}

func layerProgramWrap(cwd string, snapshot *sandboxpolicy.Snapshot) func([]string) ([]string, error) {
	return func(argv []string) ([]string, error) {
		return processProgramLayerWrap(cwd, snapshot, argv)
	}
}

// wrapProcessProgramWithTclaudeLayer renders argv as a command run in cwd
// inside tclaude's own sandbox, the same boundary a non-interactive spawn or a
// séance gets. It runs under the shell launch identity: a program is no coding
// harness, so it gets no harness's credentials or state directories.
func wrapProcessProgramWithTclaudeLayer(cwd string, snapshot *sandboxpolicy.Snapshot, argv []string) ([]string, error) {
	resolved, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return nil, fmt.Errorf("resolve working directory %q: %w", cwd, err)
	}
	if snapshot == nil {
		empty := sandboxpolicy.EmptySnapshot()
		snapshot = &empty
	}
	validated, err := sandboxpolicy.RevalidateSnapshot(*snapshot)
	if err != nil {
		return nil, fmt.Errorf("the recorded sandbox is no longer valid: %w", err)
	}
	launch, err := session.SandboxSnapshotForOneShotLaunch(&validated)
	if err != nil {
		return nil, fmt.Errorf("the recorded sandbox is no longer valid: %w", err)
	}
	spec, err := session.BuildTclaudeLayerLaunchSpec(session.TclaudeLayerLaunchInput{
		HarnessName: harness.ShellName, Cwd: resolved, Snapshot: launch,
	})
	if err != nil {
		return nil, err
	}
	posture, err := session.TclaudeLayerNetworkPosture(spec.Effective)
	if err != nil {
		return nil, err
	}
	root, err := session.TclaudeLayerRootPosture(posture, spec.Effective)
	if err != nil {
		return nil, err
	}
	if err := session.PrepareTclaudeLayerHarnessState(spec); err != nil {
		return nil, err
	}
	binary, _, err := session.ResolveTclaudeLayerServerForEngine(posture, root, spec.Contract.NetworkEngine)
	if err != nil {
		return nil, err
	}
	quoted := make([]string, len(argv))
	for i, arg := range argv {
		quoted[i] = clcommon.ShellQuoteArg(arg)
	}
	wrapped, err := session.WrapTclaudeLayerServerSpec(binary, spec, "exec "+strings.Join(quoted, " "))
	if err != nil {
		return nil, err
	}
	return []string{"/bin/sh", "-c", wrapped}, nil
}

// processProgramBoundsView is one program attempt's recorded bounds on the
// read model.
type processProgramBoundsView struct {
	executor.ProgramBounds
	// Attempt is the attempt the bounds were recorded for; absent on a
	// reissue, which re-runs the outstanding attempt.
	Attempt int `json:"attempt,omitempty"`
	// Sequence orders recordings across the nodes a projection folds together.
	Sequence int64 `json:"-"`
}

// processRunProgramBounds reads each program node's latest recorded bounds.
func processRunProgramBounds(runID string) (map[string]processProgramBoundsView, error) {
	events, err := db.ListLatestProcessRunNodeEvents(runID, "program_prepared", "program_reissued")
	if err != nil || len(events) == 0 {
		return nil, err
	}
	out := make(map[string]processProgramBoundsView, len(events))
	for _, event := range events {
		var payload struct {
			Command engine.Command          `json:"command"`
			Bounds  *executor.ProgramBounds `json:"bounds"`
		}
		// Lenient by design: prepared and reissued payloads carry different
		// sibling fields, and only the bounds are read here.
		if err := json.Unmarshal(event.PayloadJSON, &payload); err != nil || payload.Bounds == nil {
			// An unscoped attempt records no bounds; a payload this reader
			// cannot decode is evidence, not state, and never fails the read.
			continue
		}
		out[event.NodeID] = processProgramBoundsView{
			ProgramBounds: *payload.Bounds, Attempt: payload.Command.Attempt, Sequence: event.Sequence,
		}
	}
	return out, nil
}
