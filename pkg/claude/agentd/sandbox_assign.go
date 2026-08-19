package agentd

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// Assigning a sandbox implementation to an EXISTING agent is the durable
// counterpart to choosing one at spawn. Until this endpoint the choice was
// frozen at birth: relaunchProfileForSpawn recorded it and only the reversible
// dashboard unlock ever wrote it again, so an agent created before an
// implementation existed could never be moved onto it — the case that motivated
// this, an agent predating `resource-only` whose operator wants it inside a
// per-agent cgroup.
//
// The write is posture-shaped rather than field-shaped: the implementation
// decides what the harness's own sandbox may be, so it carries the recorded
// harness-builtin mode and that mode's attribution with it (see
// db.AssignAgentSandboxImplementation). Nothing else about the agent changes.
//
// It refuses on a LIVE agent. The recorded posture is what the next launch
// replays, so writing it under a running pane would leave the durable record
// asserting containment the live process does not have, with no event to
// reconcile the two — and the operator's own next step, waking the agent, is
// what applies it. Stop the agent, assign, wake.

// handleAgentSandboxImplementation serves GET (read the durable posture) and
// POST (assign a new implementation) on /v1/agent/{selector}/sandbox-impl.
func handleAgentSandboxImplementation(w http.ResponseWriter, r *http.Request, convID string) {
	switch r.Method {
	case http.MethodGet, http.MethodPost:
	default:
		writeError(w, http.StatusMethodNotAllowed, "method", "GET or POST only")
		return
	}
	// requirePermission, not requireCrossAgentPermission: that gate confers the
	// slug structurally on an owner of any group containing the target, and this
	// capability must not be owner-conferred. A lead can already spawn into its
	// own group, where the sandbox-lineage guard caps the child at the lead's own
	// posture; letting the same lead reassign an existing member to an
	// implementation with no confinement would walk around that cap. The slug is
	// operator policy, like sandbox-profiles.manage — humans pass, an agent needs
	// a real grant.
	if _, ok := requirePermission(w, r, PermAgentSandboxImplementation); !ok {
		return
	}
	if r.Method == http.MethodGet {
		writeSandboxImplementationResponse(w, convID, "")
		return
	}
	assignAgentSandboxImplementation(w, r, convID)
}

// dashboardSandboxImplementationAgent is the cookie-authenticated half of the
// same operation, for the dashboard's picker dialog: GET loads the durable
// posture the dialog renders, POST assigns.
//
// It carries no slug check because the dashboard human IS the operator this
// capability is reserved for — checkDashboardAuth in handleDashboardAgentsAPI is
// the consent layer, exactly as it is for the sandbox-restart action beside it.
// The /v1 route keeps requirePermission for agent callers.
func dashboardSandboxImplementationAgent(w http.ResponseWriter, r *http.Request, convSelector string) {
	resolved, _, err := agent.ResolveSelector(convSelector)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "resolve agent: "+err.Error())
		return
	}
	if r.Method == http.MethodGet {
		writeSandboxImplementationResponse(w, resolved.ConvID, "")
		return
	}
	assignAgentSandboxImplementation(w, r, resolved.ConvID)
}

// assignAgentSandboxImplementation validates and records one posture change.
//
// The check order is deliberate: the requested VALUE is judged first so a
// misspelling is a 400 rather than a report on whatever the agent happens to be
// doing; then the state conflicts an operator must resolve (not an agent, not
// running, not unlocked); then the launch gates, which are the expensive ones
// and the only ones that need the resolved chain.
func assignAgentSandboxImplementation(w http.ResponseWriter, r *http.Request, convID string) {
	var body struct {
		// Implementation is the sandbox implementation to record. Required:
		// there is no "clear" spelling, because every launch resolves to some
		// implementation and harness-builtin is how an operator asks for the
		// compatibility default back.
		Implementation string `json:"implementation"`
		// Sandbox optionally pins the harness-builtin MODE recorded alongside
		// it. Omitted, the agent's recorded mode is carried forward — unless the
		// implementation being replaced FORCED that mode, in which case it is an
		// artifact of the old implementation rather than an operator's choice and
		// the harness's own default is used instead.
		Sandbox string `json:"sandbox,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "decode request: "+err.Error())
		return
	}
	requested := strings.TrimSpace(body.Implementation)
	if requested == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg",
			fmt.Sprintf("implementation is required (%s, %s, %s, %s, or %s)",
				sandboxpolicy.ImplementationHarnessBuiltin, sandboxpolicy.ImplementationTclaudeLayer,
				sandboxpolicy.ImplementationStacked, sandboxpolicy.ImplementationResourceOnly,
				sandboxpolicy.ImplementationOff))
		return
	}
	// Judge the VALUE before the state conflicts below, so a misspelling is a 400
	// rather than whatever the agent happens to be doing right now.
	if _, err := sandboxpolicy.NormalizeImplementation(requested); err != nil {
		writeError(w, http.StatusBadRequest,
			"invalid_"+sandboxImplementationField, err.Error())
		return
	}

	agentID, err := db.AgentIDForConv(convID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db", err.Error())
		return
	}
	if agentID == "" {
		writeError(w, http.StatusConflict, "not_agent",
			"assigning a sandbox implementation requires a stable agent identity; conversation "+
				short8(convID)+" is not enrolled as an agent")
		return
	}

	// Serialize against the launch paths on the lock a resume takes, so an
	// assignment cannot land between a wake's liveness check and its launch.
	launchLock := resumeLaunchLock(convID)
	launchLock.Lock()
	defer launchLock.Unlock()
	if err := requireCurrentAgentGeneration(agentID, convID); err != nil {
		writeError(w, http.StatusConflict, "stale_generation", err.Error())
		return
	}
	if isConvOnline(convID) {
		writeError(w, http.StatusConflict, "agent_online",
			"this agent is running, and a sandbox implementation is applied by the launch that follows it; stop the agent, assign, then wake it")
		return
	}

	current, err := durableRelaunchConfigForConv(convID)
	if err != nil {
		writeError(w, http.StatusConflict, "relaunch_profile", err.Error())
		return
	}
	if current.TemporaryHarnessBuiltinMode {
		writeError(w, http.StatusConflict, "temporary_sandbox_override",
			db.ErrTemporarySandboxOverrideActive.Error())
		return
	}
	h, err := harness.Resolve(current.Harness)
	if err != nil {
		writeError(w, http.StatusConflict, "harness", err.Error())
		return
	}

	// Harness applicability first: an implementation this harness cannot run is
	// a bad request whatever the host looks like.
	implementation, err := validateAssignedSandboxImplementation(h, requested)
	if err != nil {
		writeError(w, sandboxImplementationValidationStatus(err),
			"invalid_"+sandboxImplementationField, err.Error())
		return
	}
	mode, fail := assignedHarnessBuiltinMode(h, current, body.Sandbox, implementation)
	if fail != nil {
		writeError(w, fail.Status, fail.Kind, fail.Msg)
		return
	}

	// Then every gate a relaunch applies to an already-resolved posture, against
	// the chain that relaunch will actually resolve. Running them here is what
	// makes the assignment actionable: their refusals name a missing tool or an
	// unrepresentable rule, and an operator reading one now can pick a different
	// implementation instead of discovering at wake time that the agent no longer
	// starts — with no relaunch-side override to rescue it.
	if fail := sandboxImplementationPostureFailure(h.Name, implementation); fail != nil {
		writeError(w, fail.Status, fail.Kind, fail.Msg)
		return
	}
	if fail := sandboxImplementationHostFailure(h.Name, implementation); fail != nil {
		writeError(w, fail.Status, fail.Kind, fail.Msg)
		return
	}
	planned, _, err := resolveCurrentSandboxChainForConv(convID)
	if err != nil {
		writeError(w, http.StatusConflict, "sandbox_profile_changed", err.Error())
		return
	}
	if fail := sandboxProfileCapabilityFailure(
		h.Name, mode, planned, implementation); fail != nil {
		writeError(w, fail.Status, fail.Kind, fail.Msg)
		return
	}
	// The gate that carries ValidateResourceLimitTarget, among others. Without it
	// an agent whose chain authors a ceiling could be assigned an implementation
	// that cannot carry one, and every later wake would fail closed — the
	// dashboard's allow-unenforced control is a fresh-spawn control, so nothing
	// would let the agent start again short of another assignment. false for
	// allowUnenforcedSandbox for the same reason: the relaunch this posture is
	// for has no such override either.
	if _, fail := planSandboxProfileAccessForLaunch(
		h.Name, mode, planned, implementation,
		session.ModelTransportLaunchContext{Model: current.Model, Cwd: current.Cwd},
		false,
	); fail != nil {
		writeError(w, fail.Status, fail.Kind, fail.Msg)
		return
	}
	if err := preflightAssignedResourceCgroup(convID, planned, implementation); err != nil {
		writeError(w, http.StatusUnprocessableEntity,
			sandboxImplementationUnavailableKind, err.Error())
		return
	}

	previous := current.SandboxImplementation
	if err := db.AssignAgentSandboxImplementation(
		agentID, implementation, mode, db.AssignedSandboxImplementationSource); err != nil {
		if errors.Is(err, db.ErrTemporarySandboxOverrideActive) {
			writeError(w, http.StatusConflict, "temporary_sandbox_override", err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "io",
			"persist sandbox implementation: "+err.Error())
		return
	}
	writeSandboxImplementationResponse(w, convID, previous)
}

// assignedHarnessBuiltinMode decides which harness-builtin mode the new posture
// records, and it is the one place the "undo my change" path can go wrong.
//
// An explicit request wins. Otherwise the agent's recorded mode is carried
// forward — except when the implementation being REPLACED forced that mode, in
// which case it says nothing about what the operator wanted. Carrying it is how
// `set <agent> harness-builtin` on an agent currently running `off`,
// `resource-only` or `tclaude-layer` would record harness-builtin paired with the
// harness's own OFF mode: an agent with no confinement at all, chosen by nobody,
// under a command the operator issued to restore confinement. The harness default
// is what a fresh daemon spawn would pick, which is the honest answer to "no mode
// was ever chosen for this posture".
func assignedHarnessBuiltinMode(
	h *harness.Harness,
	current *durableRelaunchConfig,
	requestedMode string,
	implementation string,
) (string, *spawnFailure) {
	requestedMode = strings.TrimSpace(requestedMode)
	if requestedMode == "" {
		requestedMode = current.NormalSandbox
		replaced, err := sandboxpolicy.NormalizeImplementation(current.SandboxImplementation)
		if err != nil || sandboxImplementationForcesLaunchMode(replaced) {
			defaulted, defaultErr := harness.ResolveHarnessBuiltinMode(h, "")
			if defaultErr != nil {
				return "", &spawnFailure{http.StatusUnprocessableEntity,
					"invalid_sandbox", defaultErr.Error()}
			}
			requestedMode = defaulted
		}
	}
	return resolveLaunchHarnessBuiltinMode(h, requestedMode, implementation)
}

// preflightAssignedResourceCgroup fails the ASSIGNMENT when the boundary the
// operator is selecting cannot be created on this host.
//
// The check has to happen here because it cannot happen usefully later. A launch
// refuses a cgroup it cannot create only when the operator is choosing the
// posture right then; every relaunch of an already-recorded boundary with no
// ceiling deliberately degrades to a notice instead, because refusing there
// would strand an agent whose only override is a fresh-spawn control (see
// session.ResourceCgroupFailureAction). An assignment IS the operator choosing —
// but the launch it takes effect on is a relaunch, which would swallow the
// refusal. So the choice is validated at the moment it is made.
//
// The probe is the real thing: it creates the cgroup a launch would create and
// removes it again, so a delegation this host cannot provide is reported with
// the same actionable error the launch would have raised. It runs against the
// chain the relaunch will RESOLVE rather than the recorded one, because a ceiling
// added to the global or group profile since the last launch changes the answer:
// accounting-only degrades where a ceiling fails closed.
func preflightAssignedResourceCgroup(
	convID string,
	planned *sandboxpolicy.Snapshot,
	implementation string,
) error {
	normalized, err := sandboxpolicy.NormalizeImplementation(implementation)
	if err != nil {
		return err
	}
	var limits sandboxpolicy.ResourceLimits
	if planned != nil {
		// A launch that already carries the operator's unenforced override skips
		// the boundary entirely (prepareManagedServerResourceCgroup, and the pane
		// seam's equivalent), so probing one here would refuse an assignment for a
		// cgroup the launch is never going to ask for.
		if hasResourceLimitOverride(planned.Effective.AccessNotices) {
			return nil
		}
		limits = planned.Effective.ResourceLimits
	}
	// Only a boundary the relaunch would fail without. An implementation that
	// merely asks for one opportunistically relaunches perfectly well on a host
	// with no delegation — refusing the assignment there would deny the operator
	// the confinement they came for over counters that are a bonus.
	if !sandboxpolicy.ResourceCgroupRequired(limits, normalized) {
		return nil
	}
	// A probe-only identity. The launch derives its cgroup name from the session
	// id it is about to create, and colliding with that name would let this
	// cleanup remove a live boundary.
	_, cleanup, err := prepareResourceCgroup("sandbox-impl-assign-preflight:"+convID, limits)
	if err != nil {
		return fmt.Errorf(
			"sandbox implementation %s needs a per-agent cgroup this host cannot create, so assigning it would leave the agent relaunching with no boundary: %w",
			normalized, err)
	}
	cleanup()
	return nil
}

// sandboxImplementationAssignmentWire is the durable posture as both the read
// and the write report it. It names the implementation, the harness-builtin mode
// it implies and that mode's attribution together, because those three are one
// answer to "what will this agent relaunch under".
type sandboxImplementationAssignmentWire struct {
	ConvID         string `json:"conv_id"`
	AgentID        string `json:"agent_id,omitempty"`
	Harness        string `json:"harness,omitempty"`
	Implementation string `json:"sandbox_implementation"`
	// Previous is the implementation this assignment replaced. Empty on a read.
	Previous string `json:"previous_sandbox_implementation,omitempty"`
	Sandbox  string `json:"sandbox,omitempty"`
	Source   string `json:"sandbox_source,omitempty"`
	// TemporarySandbox reports the reversible dashboard unlock, which suspends
	// the durable posture rather than replacing it.
	TemporarySandbox bool `json:"temporary_sandbox_active,omitempty"`
	Online           bool `json:"online"`
	// ResourceCgroup reports whether the recorded posture asks for a per-agent
	// cgroup at all, so a reader can tell an assignment that bought a boundary
	// from one that only changed confinement.
	ResourceCgroup bool `json:"resource_cgroup"`
	// ResourceCgroupRequired separates a boundary the relaunch cannot proceed
	// without — the one preflightAssignedResourceCgroup just proved this host
	// can create — from one the launch merely attempts and discloses when it
	// cannot. Only the first is a promise a reader may repeat.
	ResourceCgroupRequired bool `json:"resource_cgroup_required,omitempty"`
}

// writeSandboxImplementationResponse renders the posture a relaunch would use.
// previous is the implementation an assignment just replaced, and is empty on a
// read — the difference is what lets a client report a change rather than a
// state.
func writeSandboxImplementationResponse(w http.ResponseWriter, convID, previous string) {
	agentID, err := db.AgentIDForConv(convID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db", err.Error())
		return
	}
	posture, err := durableRelaunchConfigForConv(convID)
	if err != nil {
		writeError(w, http.StatusConflict, "relaunch_profile", err.Error())
		return
	}
	// The chain a relaunch would resolve, not the recorded one, so the reported
	// answer matches the launch this posture is for.
	limits := sandboxpolicy.ResourceLimits{}
	if planned, _, planErr := resolveCurrentSandboxChainForConv(convID); planErr == nil && planned != nil {
		limits = planned.Effective.ResourceLimits
	}
	implementation, normErr := sandboxpolicy.NormalizeImplementation(posture.SandboxImplementation)
	writeJSON(w, http.StatusOK, sandboxImplementationAssignmentWire{
		ConvID:           convID,
		AgentID:          agentID,
		Harness:          posture.Harness,
		Implementation:   posture.SandboxImplementation,
		Previous:         previous,
		Sandbox:          posture.NormalSandbox,
		Source:           posture.NormalSandboxSource,
		TemporarySandbox: posture.TemporaryHarnessBuiltinMode,
		Online:           isConvOnline(convID),
		ResourceCgroup: normErr == nil &&
			resourceCgroupRequested(limits, implementation),
		ResourceCgroupRequired: normErr == nil &&
			sandboxpolicy.ResourceCgroupRequired(limits, implementation),
	})
}
