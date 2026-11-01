package agentd

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/groupexport"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// groups_export_posture.go decides how an imported member launches. An archive
// has no say in it: the member takes what a spawn into the landing group would
// resolve (its spawn profile tiers, the install default, then the group and
// global sandbox profiles), and a posture this node cannot meet refuses the
// import through the same gates that refuse a spawn. There is no fall back to
// a wider posture, and nothing is recorded for a refused member.
//
// A group import always creates its group, so at import the group contributes
// no tier of its own beyond the name. Later group assignments still reach the
// member: the recorded snapshot names the group, and every relaunch recomposes
// the sandbox profile chain from it.

// landingPosture is the launch posture a landing resolves to.
type landingPosture struct {
	// SandboxImplementation names who owns OS-level confinement.
	SandboxImplementation string `json:"sandbox_implementation"`
	// Sandbox is the harness's own sandbox mode under that implementation.
	Sandbox string `json:"sandbox,omitempty"`
	// SandboxProfiles names the sandbox profiles the launch composes.
	SandboxProfiles []string `json:"sandbox_profiles,omitempty"`
	// ResolvedBy names the tiers that chose this, as a spawn's provenance does.
	ResolvedBy string `json:"resolved_by,omitempty"`
	// OpenCodeState says which store an OpenCode member's conversation goes
	// in: private or ambient.
	OpenCodeState string `json:"opencode_state,omitempty"`
	// Refused is why this node cannot launch the member under this posture.
	// The dry run reports it; the import refuses.
	Refused string `json:"refused,omitempty"`

	refusal      *spawnFailure
	mode         string
	modeSource   string
	snapshot     sandboxpolicy.Snapshot
	privateAgentID string // the pre-minted agent owning private OpenCode state
}

func (p *landingPosture) usesPrivateOpenCodeState() bool {
	return p != nil && p.OpenCodeState == db.OpenCodeStatePrivate
}

// groupPostureRefusal is the import's refusal of postures this node cannot
// meet, carrying the kind the spawn gate refused with.
type groupPostureRefusal struct {
	status int
	kind   string
	msg    string
}

func (e *groupPostureRefusal) Error() string { return e.msg }

// resolveLandingPosture resolves and gates the posture of a member landing on
// harnessName in g. A nil g is the group this import is about to create, which
// has no tiers yet. requestedImplementation is the explicit tier; only an agent
// bundle's retained confinement sets it (see receiverSandboxProfile).
//
// A refusal is reported on the posture, because the dry run has to show it.
// The returned error is storage.
func resolveLandingPosture(
	g *db.AgentGroup,
	harnessName, requestedImplementation, model, cwd string,
	environment []sandboxpolicy.EnvironmentEntry,
) (*landingPosture, error) {
	groupName := ""
	var groupID int64
	if g != nil {
		groupName, groupID = g.Name, g.ID
	}
	defaults, fail, err := resolveLaunchDefaults(groupName, "", harnessName)
	if err != nil {
		return nil, err
	}
	if fail != nil {
		return &landingPosture{Refused: fail.Msg, refusal: fail}, nil
	}
	h := defaults.harness
	implementation, source := string(defaults.implementation), defaults.implementationSource
	if requestedImplementation != "" {
		implementation, source = requestedImplementation, agent.ProvExplicit
	}
	posture := &landingPosture{
		SandboxImplementation: implementation, Sandbox: defaults.sandbox,
		ResolvedBy: defaults.resolvedBy, modeSource: defaults.sandboxSource,
	}
	refuse := func(fail *spawnFailure) (*landingPosture, error) {
		posture.Refused, posture.refusal = fail.Msg, fail
		return posture, nil
	}
	if fail := withInstallDefaultRemedy(sandboxImplementationHostFailure(h.Name, implementation), source); fail != nil {
		return refuse(fail)
	}
	if fail := withInstallDefaultRemedy(sandboxImplementationPostureFailure(h.Name, implementation), source); fail != nil {
		return refuse(fail)
	}
	nativeMode, fail := resolveSandboxImplementationMode(h, defaults.sandbox, implementation)
	if fail != nil {
		return refuse(fail)
	}
	if posture.mode, fail = resolveLaunchHarnessBuiltinMode(h, defaults.sandbox, implementation); fail != nil {
		return refuse(fail)
	}
	posture.Sandbox = posture.mode

	snapshot := sandboxpolicy.OmittedProfilesSnapshot()
	if !sandboxProfilesDisabled(h.Name, nativeMode, implementation) {
		if snapshot, err = db.ResolveEffectiveSandboxSnapshot(groupID, ""); err != nil {
			return refuse(&spawnFailure{http.StatusBadRequest, "invalid_sandbox_profile", err.Error()})
		}
	}
	if _, err := sandboxpolicy.MergeEnvironment(snapshot.Effective.Environment, environment); err != nil {
		return refuse(&spawnFailure{http.StatusBadRequest, "invalid_environment", err.Error()})
	}
	snapshot.LaunchEnvironment = append([]sandboxpolicy.EnvironmentEntry(nil), environment...)
	snapshot.RefreshGroupEnvironment = true
	snapshot.ResolutionGroupID = groupID
	if fail := sandboxProfileCapabilityFailure(h.Name, nativeMode, &snapshot, implementation); fail != nil {
		return refuse(fail)
	}
	if _, fail := planSandboxProfileAccessForLaunch(h.Name, nativeMode, &snapshot, implementation,
		session.ModelTransportLaunchContext{Model: model, Cwd: cwd}, false); fail != nil {
		return refuse(fail)
	}
	for _, applied := range snapshot.Applied {
		posture.SandboxProfiles = append(posture.SandboxProfiles, applied.Name)
	}
	posture.snapshot = snapshot
	if h.Name == harness.OpenCodeName {
		posture.OpenCodeState = "ambient"
		if isTclaudeLayer(implementation) {
			posture.OpenCodeState = db.OpenCodeStatePrivate
		}
	}
	return posture, nil
}

func isTclaudeLayer(implementation string) bool {
	normalized, err := sandboxpolicy.NormalizeImplementation(implementation)
	return err == nil && normalized == sandboxpolicy.ImplementationTclaudeLayer
}

// planGroupPostures resolves every member's landing posture.
func planGroupPostures(ls []*groupMemberLanding, exp *groupexport.Export, cwd string) error {
	for _, l := range ls {
		posture, err := resolveLandingPosture(nil, l.Harness, "", l.Model, cwd, exp.Group.Environment)
		if err != nil {
			return fmt.Errorf("resolve the posture of %s: %w", l.Title, err)
		}
		l.Posture = posture
	}
	return nil
}

// refusedPostures refuses the import when any member's posture cannot be met.
// The refusal names each member and takes the status and kind of the first.
func refusedPostures(ls []*groupMemberLanding) *groupPostureRefusal {
	var lines []string
	var first *spawnFailure
	for _, l := range ls {
		if l.Posture.refusal == nil {
			continue
		}
		if first == nil {
			first = l.Posture.refusal
		}
		lines = append(lines, l.Title+": "+l.Posture.Refused)
	}
	if first == nil {
		return nil
	}
	sort.Strings(lines)
	return &groupPostureRefusal{first.Status, first.Kind,
		"import: refused rather than landing a member outside its sandbox posture. " + strings.Join(lines, "; ")}
}

// installPrivateOpenCodeLanding installs a member's conversation into private
// state for the agent identity it is about to take, the way a tclaude-layer
// spawn of an imported agent does. The allocation and root are removed when the
// import rolls back, since no agent ever owned them.
func installPrivateOpenCodeLanding(l *groupMemberLanding, cwd string) (string, func(), error) {
	agentID := db.NewAgentID()
	allocation, err := allocatePrivateOpenCodeState(agentID)
	if err != nil {
		return "", nil, err
	}
	discard := func() {
		if err := os.RemoveAll(allocation.StateRoot); err != nil {
			slog.Warn("import: remove unused OpenCode state", "agent", agentID, "error", err)
		}
		if err := db.DeleteOpenCodeAgentStateAllocation(agentID); err != nil {
			slog.Warn("import: forget unused OpenCode state", "agent", agentID, "error", err)
		}
	}
	spec, err := openCodeTclaudeLayerLaunchSpec(
		l.Posture.SandboxImplementation, cwd, nil, &l.Posture.snapshot, agentID)
	if err != nil {
		discard()
		return "", nil, &groupPostureRefusal{http.StatusUnprocessableEntity,
			"unsupported_sandbox_profile_network", fmt.Sprintf("import: %s: %v", l.Title, err)}
	}
	if err = prepareOpenCodeTclaudeLayerState(spec); err != nil {
		discard()
		return "", nil, err
	}
	scoped, err := openCodeHistoryForAgent(l.dst, agentID)
	if err != nil {
		discard()
		return "", nil, err
	}
	id, undo, err := importBundleHistory(scoped, l.history, cwd)
	if err != nil {
		discard()
		return "", nil, err
	}
	l.Posture.privateAgentID = agentID
	return id, func() {
		undo()
		discard()
	}, nil
}

// memberPostures is what the import transaction records for each member.
func memberPostures(ls []*groupMemberLanding, exp *groupexport.Export, convRemap map[string]string) map[string]db.GroupImportMemberPosture {
	out := make(map[string]db.GroupImportMemberPosture, len(ls))
	for i, l := range ls {
		relaunch := baselineRelaunchProfile(l.Harness)
		implementation, mode, source := l.Posture.SandboxImplementation, l.Posture.mode, l.Posture.modeSource
		relaunch.SandboxImplementation = &implementation
		relaunch.HarnessBuiltinMode = &mode
		relaunch.HarnessBuiltinModeSource = &source
		out[convRemap[exp.Convs[i].ConvID]] = db.GroupImportMemberPosture{
			AgentID: l.Posture.privateAgentID, Relaunch: relaunch, Sandbox: l.Posture.snapshot,
		}
	}
	return out
}
