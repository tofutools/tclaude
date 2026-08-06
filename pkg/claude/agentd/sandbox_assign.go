package agentd

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
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

func assignAgentSandboxImplementation(w http.ResponseWriter, r *http.Request, convID string) {
	var body struct {
		// Implementation is the sandbox implementation to record. Required:
		// there is no "clear" spelling, because every launch resolves to some
		// implementation and harness-builtin is how an operator asks for the
		// compatibility default back.
		Implementation string `json:"implementation"`
		// Sandbox optionally pins the harness-builtin MODE recorded alongside
		// it. Omitted, the agent's recorded mode is carried through the same
		// resolution a launch applies — which for the no-confinement
		// implementations replaces it with the harness's own off mode.
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
	implementation, err := validateSandboxImplementationForHarness(h, requested)
	if err != nil {
		writeError(w, sandboxImplementationValidationStatus(err),
			"invalid_"+sandboxImplementationField, err.Error())
		return
	}
	requestedMode := strings.TrimSpace(body.Sandbox)
	if requestedMode == "" {
		requestedMode = current.NormalSandbox
	}
	mode, fail := resolveLaunchHarnessBuiltinMode(h, requestedMode, implementation)
	if fail != nil {
		writeError(w, fail.Status, fail.Kind, fail.Msg)
		return
	}

	// Then the gates a relaunch applies to an already-resolved value. Running
	// them here is what makes the assignment actionable: their refusals name a
	// missing tool or an unrepresentable rule, and an operator reading one now
	// can pick a different implementation instead of discovering at wake time
	// that the agent no longer starts.
	if fail := sandboxImplementationPostureFailure(h.Name, implementation); fail != nil {
		writeError(w, fail.Status, fail.Kind, fail.Msg)
		return
	}
	if fail := sandboxImplementationHostFailure(h.Name, implementation); fail != nil {
		writeError(w, fail.Status, fail.Kind, fail.Msg)
		return
	}
	recorded, err := db.AgentEffectiveSandboxConfigForConv(convID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db",
			"read recorded effective sandbox: "+err.Error())
		return
	}
	if fail := sandboxProfileCapabilityFailure(
		h.Name, mode, recorded, implementation); fail != nil {
		writeError(w, fail.Status, fail.Kind, fail.Msg)
		return
	}
	if err := preflightAssignedResourceCgroup(convID, recorded, implementation); err != nil {
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
// the same actionable error the launch would have raised. A ceiling authored in
// the agent's recorded chain is probed too, since assigning an implementation
// that carries it is also when that limit first becomes reachable.
func preflightAssignedResourceCgroup(
	convID string,
	recorded *sandboxpolicy.Snapshot,
	implementation string,
) error {
	normalized, err := sandboxpolicy.NormalizeImplementation(implementation)
	if err != nil {
		return err
	}
	var limits sandboxpolicy.ResourceLimits
	if recorded != nil {
		limits = recorded.Effective.ResourceLimits
	}
	if !sandboxpolicy.ResourceCgroupRequested(limits, normalized) {
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
}

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
	limits := sandboxpolicy.ResourceLimits{}
	if recorded, recErr := db.AgentEffectiveSandboxConfigForConv(convID); recErr == nil && recorded != nil {
		limits = recorded.Effective.ResourceLimits
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
			sandboxpolicy.ResourceCgroupRequested(limits, implementation),
	})
}
