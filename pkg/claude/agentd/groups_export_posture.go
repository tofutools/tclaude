package agentd

import (
	"cmp"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

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

	refusal        *spawnFailure
	mode           string
	modeSource     string
	snapshot       sandboxpolicy.Snapshot
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
// has no tiers yet. requestedImplementation and requestedMode are the explicit
// tier; only what an agent bundle may keep sets them (see
// receiverSandboxProfile).
//
// A refusal is reported on the posture, because the dry run has to show it.
// The returned error is storage.
func resolveLandingPosture(
	g *db.AgentGroup,
	harnessName, requestedImplementation, requestedMode, model, cwd string,
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
	mode, modeSource, resolvedBy := defaults.sandbox, defaults.sandboxSource, defaults.resolvedBy
	if requestedImplementation != "" {
		implementation, source = requestedImplementation, agent.ProvExplicit
	}
	if requestedMode != "" {
		mode, modeSource = requestedMode, agent.ProvExplicit
	}
	if requestedImplementation != "" || requestedMode != "" {
		resolvedBy = joinProvenanceSources("the agent file", modeSource)
	}
	posture := &landingPosture{
		SandboxImplementation: implementation, Sandbox: mode,
		ResolvedBy: resolvedBy, modeSource: modeSource,
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
	nativeMode, fail := resolveSandboxImplementationMode(h, mode, implementation)
	if fail != nil {
		return refuse(fail)
	}
	if posture.mode, fail = resolveLaunchHarnessBuiltinMode(h, mode, implementation); fail != nil {
		return refuse(fail)
	}
	posture.Sandbox = posture.mode
	if home, err := os.UserHomeDir(); err == nil && harness.CodexSandboxCwdConflict(nativeMode, cwd, home) {
		return refuse(&spawnFailure{http.StatusBadRequest, "invalid_cwd", fmt.Sprintf(
			"refusing to land a %s agent in %q under sandbox %q: it would expose "+
				"~/.tclaude / ~/.codex / ~/.claude to the agent's writes; import into a project subdirectory",
			h.Name, cwd, nativeMode)})
	}

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
		session.ModelTransportLaunchContext{Model: model, Cwd: nearestExistingDir(cwd)}, false); fail != nil {
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

// nearestExistingDir is cwd or its closest existing ancestor. --into may only
// have named the landing directory so far, and the gates that run a harness
// probe need somewhere to run it, without a refused import leaving a directory
// behind.
func nearestExistingDir(cwd string) string {
	for cwd != "" {
		if info, err := os.Stat(cwd); err == nil && info.IsDir() {
			return cwd
		}
		parent := filepath.Dir(cwd)
		if parent == cwd {
			break
		}
		cwd = parent
	}
	return cwd
}

func isTclaudeLayer(implementation string) bool {
	normalized, err := sandboxpolicy.NormalizeImplementation(implementation)
	return err == nil && normalized == sandboxpolicy.ImplementationTclaudeLayer
}

// dropFileGroupEnvironment takes a group's environment out of its file: it would
// reach every spawn into the new group. The warning names the variables, never
// their values.
func dropFileGroupEnvironment(exp *groupexport.Export) []string {
	if len(exp.Group.Environment) == 0 {
		return nil
	}
	names := make([]string, len(exp.Group.Environment))
	for i, entry := range exp.Group.Environment {
		names[i] = entry.Name
	}
	exp.Group.Environment = nil
	return []string{fileFieldDropped("environment variables " + strings.Join(names, ", "))}
}

// dropFileAuthority takes out of a group's file what would become a live grant.
// Nothing here is a move the receiver owns: only a file reaches this path. A deny
// only takes away, so it stays. Scheduled jobs stay for a human to review, with
// none enabled and none speaking as the operator: an owner this file named could
// otherwise enable one. It runs before the import validates permission scopes, so
// a row that never lands cannot refuse the import.
func dropFileAuthority(exp *groupexport.Export) []string {
	titles := map[string]string{}
	for _, c := range exp.Convs {
		titles[c.ConvID] = c.Title
	}
	var granted fileGrants
	for _, p := range exp.Group.Permissions {
		granted.add("group permission", "", p.Slug)
	}
	exp.Group.Permissions = nil
	kept := exp.Permissions[:0:0]
	for _, p := range exp.Permissions {
		if p.Effect == db.PermEffectDeny {
			kept = append(kept, p)
			continue
		}
		granted.add("permission", cmp.Or(titles[p.ConvID], short8(p.ConvID)), p.Slug)
	}
	exp.Permissions = kept
	for _, s := range exp.SudoGrants {
		if fileSudoGrantLive(s) {
			granted.add("sudo grant", cmp.Or(titles[s.ConvID], short8(s.ConvID)), s.Slug)
		}
	}
	exp.SudoGrants = nil
	var warnings []string
	if len(granted) > 0 {
		warnings = append(warnings, "This file grants "+strings.Join(granted.lines(), "; ")+
			", which authority in a file does not carry, so none of it is applied. Grant what you want here yourself.")
	}
	var disabled, unowned []string
	for i := range exp.CronJobs {
		job := &exp.CronJobs[i]
		if job.Enabled != 0 {
			disabled = append(disabled, job.Name)
		}
		if job.OperatorAuthored {
			unowned = append(unowned, job.Name)
		}
		job.Enabled, job.RunImmediately, job.OperatorAuthored = 0, 0, false
	}
	if len(disabled) > 0 {
		warnings = append(warnings, "Scheduled jobs "+strings.Join(disabled, ", ")+
			" land disabled. Review them and enable the ones you want.")
	}
	if len(unowned) > 0 {
		warnings = append(warnings, "Scheduled jobs "+strings.Join(unowned, ", ")+
			" no longer run as the operator. Set their owner before enabling them.")
	}
	return warnings
}

// fileGrants lists what a file grants, a line per kind and holder, so that one
// agent's permissions read as one list. A group's export can hold dozens.
type fileGrants []*fileGrantLine

type fileGrantLine struct {
	kind, holder string
	slugs        []string
}

func (g *fileGrants) add(kind, holder, slug string) {
	slug = db.CanonicalPermissionSlug(slug)
	for _, line := range *g {
		if line.kind == kind && line.holder == holder {
			if !slices.Contains(line.slugs, slug) {
				line.slugs = append(line.slugs, slug)
			}
			return
		}
	}
	*g = append(*g, &fileGrantLine{kind, holder, []string{slug}})
}

func (g fileGrants) lines() []string {
	out := make([]string, len(g))
	for i, line := range g {
		out[i] = line.kind
		if len(line.slugs) > 1 {
			out[i] += "s"
		}
		out[i] += " " + strings.Join(line.slugs, ", ")
		if line.holder != "" {
			out[i] += " for " + line.holder
		}
	}
	return out
}

// fileSudoGrantLive reports whether a file's sudo row was still in force. An
// export carries ended rows too, and those are history rather than a grant lost.
func fileSudoGrantLive(s groupexport.SudoGrant) bool {
	if s.RevokedAt != "" || (s.UsesLimit > 0 && s.UsesLeft <= 0) {
		return false
	}
	expires, err := time.Parse(time.RFC3339, s.ExpiresAt)
	return err != nil || expires.After(time.Now())
}

// planGroupPostures resolves every member's landing posture.
func planGroupPostures(ls []*groupMemberLanding, exp *groupexport.Export, cwd string) error {
	for _, l := range ls {
		posture, err := resolveLandingPosture(nil, l.Harness, "", "", l.Model, cwd, exp.Group.Environment)
		if err != nil {
			return fmt.Errorf("resolve the posture of %s: %w", l.Title, err)
		}
		if l.history == nil {
			// Configuration only: no conversation goes in either store.
			posture.OpenCodeState = ""
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
	discard, err := claimPrivateOpenCodeState(agentID)
	if err != nil {
		return "", nil, err
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
