package agentd

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

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
// A plain assignment refuses on a LIVE agent. The recorded posture is what the
// next launch replays, so writing it under a running pane would leave the
// durable record asserting containment the live process does not have, with no
// event to reconcile the two. Asking for `restart` closes that gap instead: the
// daemon runs every gate first, then stops the idle agent, records the posture
// and resumes the same conversation under it — the stop/assign/wake sequence as
// one operation, like the sandbox unlock restart beside it.

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
	if r.Method == http.MethodPost {
		r = withApprovalFacts(r, func(body []byte) string { return sandboxMoveOutApprovalFacts(convID, body) })
	}
	if _, ok := requirePermission(w, r, PermAgentSandboxImplementation); !ok {
		return
	}
	if r.Method == http.MethodGet {
		writeSandboxImplementationResponse(w, convID, "", nil, "")
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
		writeSandboxImplementationResponse(w, resolved.ConvID, "", nil, "")
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
		// Restart lets a LIVE agent take the assignment: once every gate has
		// passed, the idle agent is stopped, the posture recorded, and the same
		// conversation resumed under it. Ignored for an agent that is already
		// stopped, which the assignment leaves stopped.
		Restart bool `json:"restart,omitempty"`
		// ConfirmMoveOut accepts what moving an OpenCode agent out of private
		// state does. Without it such an assignment is refused with those facts,
		// so every client shows the same ones.
		ConfirmMoveOut bool `json:"confirm_move_out,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "decode request: "+err.Error())
		return
	}
	requested := strings.TrimSpace(body.Implementation)
	if requested == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg",
			fmt.Sprintf("implementation is required (%s, %s, %s, or %s)",
				sandboxpolicy.ImplementationHarnessBuiltin, sandboxpolicy.ImplementationTclaudeLayer,
				sandboxpolicy.ImplementationResourceOnly, sandboxpolicy.ImplementationOff))
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
	online := isConvOnline(convID)
	var liveSession *db.SessionRow
	if online {
		if !body.Restart {
			writeError(w, http.StatusConflict, "agent_online",
				"this agent is running, and a sandbox implementation is applied by the launch that follows it; "+
					"ask for a restart to apply it now, or stop the agent, assign, then wake it")
			return
		}
		liveSession = pickAliveSession(convID)
		if failure := agentRestartIdleFailure(liveSession, time.Now()); failure != "" {
			writeError(w, http.StatusConflict, "agent_not_idle", failure)
			return
		}
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
	// Last of the refusals, so the operator is not asked to accept a move the
	// assignment would refuse anyway, and before a running agent is stopped.
	if !body.ConfirmMoveOut && !isTclaudeLayer(implementation) {
		if fail := openCodeMoveOutConfirmation(h, agentID, convID, current.Cwd); fail != nil {
			writeError(w, fail.Status, fail.Kind, fail.Msg)
			return
		}
	}

	previous := current.SandboxImplementation
	var note string
	persist := func() *spawnFailure {
		commit, undo, moveNote, fail := moveOpenCodeStateForAssignment(h, agentID, convID, current.Cwd, implementation, planned)
		if fail != nil {
			return fail
		}
		note = moveNote
		// Only a move out that finished returns this note, and the posture and
		// the allocation it gives up must change together.
		assign := db.AssignAgentSandboxImplementation
		if moveNote == openCodeMoveOutNote {
			assign = db.AssignAgentSandboxImplementationLeavingOpenCodeState
		}
		err := assign(agentID, implementation, mode, db.AssignedSandboxImplementationSource)
		if err != nil {
			undo()
		}
		switch {
		case err == nil:
			commit()
			return nil
		case errors.Is(err, db.ErrTemporarySandboxOverrideActive):
			return &spawnFailure{
				Status: http.StatusConflict, Kind: "temporary_sandbox_override", Msg: err.Error()}
		default:
			return &spawnFailure{
				Status: http.StatusInternalServerError, Kind: "io",
				Msg: "persist sandbox implementation: " + err.Error()}
		}
	}
	if !online {
		if fail := persist(); fail != nil {
			writeError(w, fail.Status, fail.Kind, fail.Msg)
			return
		}
		writeSandboxImplementationResponse(w, convID, previous, nil, note)
		return
	}

	// Restart path. Every gate above has passed, so the only refusals left are
	// the ones a stop/resume can hit. An agent whose Claude subscription token
	// is gone could not come back, so do not stop it.
	if err := claudeSubscriptionRelaunchBlocked(agentID); err != nil {
		writeError(w, http.StatusConflict, claudeSubscriptionLostCode, err.Error())
		return
	}
	// Park attached clients across the stop/resume gap; the deferred finish
	// returns them to a still-live old pane on any error path.
	clientHandoff := beginAgentRestartTmuxHandoff(liveSession.TmuxSession)
	defer clientHandoff.finishForConv(convID)

	stopped := escalateShutdownUnderLaunchLock(convID, shutdownGrace)
	if stopped.Outcome == shutdownFailed {
		writeError(w, http.StatusInternalServerError, "shutdown_failed",
			"could not stop the idle agent before changing its sandbox implementation: "+stopped.Detail)
		return
	}
	if err := requireCurrentAgentGeneration(agentID, convID); err != nil {
		writeError(w, http.StatusConflict, "stale_generation",
			"agent generation changed while stopping; sandbox implementation was not changed: "+err.Error())
		return
	}
	if fail := persist(); fail != nil {
		// The posture did not change, so bring the agent back under the old one
		// before reporting the persistence failure.
		resume := resumeOneConvUnderLaunchLock(convID, false, nil)
		clientHandoff.finishForConv(convID)
		if resume.Action != "resumed" && resume.Action != "skipped:already_online" {
			fail.Msg += "; restoring the stopped agent also failed: " + resume.Detail
		}
		writeError(w, fail.Status, fail.Kind, fail.Msg)
		return
	}
	resume := resumeOneConvUnderLaunchLock(convID, false, nil)
	switchedClients := clientHandoff.finishForConv(convID)
	if resume.Action != "resumed" {
		msg := "sandbox implementation " + implementation + " was recorded, but the agent could not be restarted: " +
			resume.Detail + "; use the normal wake action to retry"
		if note != "" {
			msg += "; " + note
		}
		writeError(w, http.StatusInternalServerError, "restart_failed", msg)
		return
	}
	writeSandboxImplementationResponse(w, convID, previous, &sandboxImplementationRestart{
		Action: resume.Action, SwitchedClients: switchedClients,
	}, note)
}

const openCodeMoveNote = "the conversation moved into private state, and its todo list did not: " +
	"OpenCode's export does not carry it, so it was deleted with the shared copy"

const openCodeMoveOutNote = "the agent's conversations moved to the shared OpenCode store, where agents " +
	"outside tclaude's sandbox can read them, and their todo lists were deleted: OpenCode's export does not carry them"

// openCodeMoveOutConfirmKind is the refusal a client answers by asking the
// operator and sending confirm_move_out.
const openCodeMoveOutConfirmKind = "opencode_move_out_confirm"

// openCodeMoveOutFacts is what an operator accepts by moving count conversations
// out of private state.
func openCodeMoveOutFacts(count int) string {
	conversations := fmt.Sprintf("%d conversations", count)
	if count == 1 {
		conversations = "1 conversation"
	}
	return "This moves " + conversations + " out of this OpenCode agent's private state to the shared " +
		"OpenCode store. " + openCodeMoveOutCommonFacts
}

// moveOutFactsTimeout bounds counting the conversations for a popup.
const moveOutFactsTimeout = 10 * time.Second

const openCodeMoveOutCommonFacts = "Agents outside tclaude's sandbox can read them once moved. Their todo lists are " +
	"deleted, because OpenCode's export does not carry them. Private conversation storage is something " +
	"only OpenCode has here: other harnesses keep conversations in a state root shared per harness, " +
	"so moving out puts this agent where those already are."

// openCodeMoveOutConfirmation refuses an assignment that would move an OpenCode
// agent out of private state, naming what the move does. Nil when nothing would
// move.
func openCodeMoveOutConfirmation(h *harness.Harness, agentID, convID, cwd string) *spawnFailure {
	if h.Name != harness.OpenCodeName {
		return nil
	}
	refuse := func(err error) *spawnFailure {
		return &spawnFailure{http.StatusInternalServerError, "opencode_state_read", err.Error()}
	}
	_, private, err := openCodeStateToMoveOut(h, agentID, convID, cwd)
	if err != nil {
		return refuse(err)
	}
	if private == nil {
		return nil
	}
	conversations, err := harness.OpenCodeConversations(private.History, cwd)
	if err != nil {
		return refuse(err)
	}
	return &spawnFailure{http.StatusConflict, openCodeMoveOutConfirmKind, openCodeMoveOutFacts(len(conversations))}
}

// sandboxMoveOutApprovalFacts is what an agent's --ask-human popup says when
// approving it would move an OpenCode agent out of private state. Only a request
// that confirms the move can do that: without confirm_move_out it is refused,
// with these facts, after the human approves.
func sandboxMoveOutApprovalFacts(convID string, body []byte) string {
	var req struct {
		Implementation string `json:"implementation"`
		ConfirmMoveOut bool   `json:"confirm_move_out"`
	}
	if !decodeLikeHandler(body, &req) || !req.ConfirmMoveOut {
		return ""
	}
	// A failed read is not a move that won't happen: the human still hears
	// what it does.
	uncounted := func(cause string) string {
		return "This may move conversations out of an OpenCode agent's private state, but tclaude could not count them: " +
			cause + ". " + openCodeMoveOutCommonFacts
	}
	agentID, err := db.AgentIDForConv(convID)
	if err != nil {
		return uncounted(err.Error())
	}
	if agentID == "" {
		return ""
	}
	current, err := durableRelaunchConfigForConv(convID)
	if err != nil {
		return uncounted(err.Error())
	}
	h, err := harness.Resolve(current.Harness)
	if err != nil {
		return uncounted(err.Error())
	}
	if h.Name != harness.OpenCodeName {
		return ""
	}
	implementation, err := validateAssignedSandboxImplementation(h, strings.TrimSpace(req.Implementation))
	if err != nil || isTclaudeLayer(implementation) {
		return ""
	}
	// Counting runs OpenCode, with a human-free timeout of days, and no popup
	// may wait on it. A count that outlives this finishes unobserved.
	counted := make(chan *spawnFailure, 1)
	go func() { counted <- openCodeMoveOutConfirmation(h, agentID, convID, current.Cwd) }()
	select {
	case fail := <-counted:
		switch {
		case fail == nil:
			return ""
		case fail.Kind == openCodeMoveOutConfirmKind:
			return fail.Msg
		}
		return uncounted(fail.Msg)
	case <-time.After(moveOutFactsTimeout):
		return uncounted("OpenCode did not answer in time")
	}
}

// openCodeStateToMoveOut returns the private state an assignment off tclaude's
// sandbox moves, and a nil history when it moves none: no allocation, or one that
// does not hold the conversation.
func openCodeStateToMoveOut(
	h *harness.Harness, agentID, convID, cwd string,
) (*db.OpenCodeAgentStateAllocation, *harness.Harness, error) {
	allocation, err := db.GetOpenCodeAgentStateAllocation(agentID)
	if err != nil || allocation == nil || allocation.Mode != db.OpenCodeStatePrivate {
		return nil, nil, err
	}
	private, err := openCodeHistoryForAgent(h, agentID)
	if err != nil {
		return nil, nil, err
	}
	held, err := harness.OpenCodeHoldsConversation(private.History, convID, cwd)
	if err != nil || !held {
		return nil, nil, err
	}
	return allocation, private, nil
}

// openCodePrivateStateRefusal refuses a launch off tclaude's sandbox while the
// OpenCode conversation is in private state. Any other implementation reads the
// shared store, where the conversation is not, so the agent could not resume. An
// allocation that does not hold the conversation strands nothing. An assignment
// does not need this, since it moves the conversation out.
func openCodePrivateStateRefusal(h *harness.Harness, agentID, convID, cwd, implementation string) *spawnFailure {
	if h.Name != harness.OpenCodeName || isTclaudeLayer(implementation) {
		return nil
	}
	allocation, err := db.GetOpenCodeAgentStateAllocation(agentID)
	if err != nil {
		return &spawnFailure{http.StatusInternalServerError, "io", err.Error()}
	}
	if allocation == nil || allocation.Mode != db.OpenCodeStatePrivate {
		return nil
	}
	private, err := openCodeHistoryForAgent(h, agentID)
	if err == nil {
		var held bool
		if held, err = harness.OpenCodeHoldsConversation(private.History, convID, cwd); err == nil && !held {
			return nil
		}
	}
	if err != nil {
		return &spawnFailure{http.StatusInternalServerError, "opencode_state_read", err.Error()}
	}
	return &spawnFailure{http.StatusConflict, "opencode_state_private",
		"this OpenCode agent's conversation is in private state, which " + implementation +
			" cannot read, so the agent could not resume under it; keep tclaude-layer, " +
			"or assign the implementation you want, which moves the conversation to the shared store"}
}

// moveOpenCodeStateForAssignment gives an OpenCode agent with no state
// allocation the private state tclaude's sandbox needs: its conversation is in
// the shared store, which the sandboxed server cannot read. The agent must be
// stopped. The copy is complete and verified when this returns and the shared
// copy is still there, so commit removes it once the posture is recorded and
// undo leaves everything as it was. An allocation that does not hold the
// conversation is a move that never finished, or a conversation started under
// another posture, and gets the same copy. Any other implementation takes the
// conversation back out, see moveOpenCodeStateOut.
func moveOpenCodeStateForAssignment(
	h *harness.Harness, agentID, convID, cwd, implementation string, planned *sandboxpolicy.Snapshot,
) (commit, undo func(), note string, fail *spawnFailure) {
	commit, undo = func() {}, func() {}
	if h.Name != harness.OpenCodeName {
		return commit, undo, "", nil
	}
	if !isTclaudeLayer(implementation) {
		return moveOpenCodeStateOut(h, agentID, convID, cwd)
	}
	refuse := func(err error) (func(), func(), string, *spawnFailure) {
		return func() {}, func() {}, "", &spawnFailure{http.StatusInternalServerError, "opencode_state_move",
			"move the conversation into private state: " + err.Error()}
	}
	existing, err := db.GetOpenCodeAgentStateAllocation(agentID)
	if err != nil {
		return commit, undo, "", &spawnFailure{http.StatusInternalServerError, "io", err.Error()}
	}
	if existing != nil {
		private, err := openCodeHistoryForAgent(h, agentID)
		if err != nil {
			return refuse(err)
		}
		inPrivate, err := harness.OpenCodeHoldsConversation(private.History, convID, cwd)
		if err != nil {
			return refuse(err)
		}
		inShared, err := harness.OpenCodeHoldsConversation(h.History, convID, cwd)
		if err != nil {
			return refuse(err)
		}
		if inPrivate || !inShared {
			return commit, undo, "", nil
		}
	}
	if err := stopOpenCodeServerForMove(convID); err != nil {
		return refuse(err)
	}
	allocation, err := allocatePrivateOpenCodeState(agentID)
	if err != nil {
		return refuse(err)
	}
	discard := func() {
		if existing != nil {
			return
		}
		if err := os.RemoveAll(allocation.StateRoot); err != nil {
			slog.Warn("sandbox assignment: remove unused OpenCode state", "agent", agentID, "error", err)
		}
		if err := db.DeleteOpenCodeAgentStateAllocation(agentID); err != nil {
			slog.Warn("sandbox assignment: forget unused OpenCode state", "agent", agentID, "error", err)
		}
	}
	spec, err := openCodeTclaudeLayerLaunchSpec(implementation, cwd, nil, planned, agentID)
	if err == nil {
		err = prepareOpenCodeTclaudeLayerState(spec)
	}
	var private *harness.Harness
	if err == nil {
		private, err = openCodeHistoryForAgent(h, agentID)
	}
	if err == nil {
		err = harness.CopyOpenCodeConversation(h.History, private.History, convID, cwd)
	}
	if err != nil {
		discard()
		return refuse(err)
	}
	return func() {
		// The agent reads its private copy from here on, so what this leaves
		// behind is a stale duplicate, not a broken agent.
		if err := harness.DeleteOpenCodeConversation(h.History, convID, cwd); err != nil {
			slog.Warn("sandbox assignment: shared OpenCode copy left behind", "conv", short8(convID), "error", err)
		}
	}, discard, openCodeMoveNote, nil
}

// stopOpenCodeServerForMove stops a conversation's server, which holds its store
// until the reaper's next tick: what it writes after the export would be lost.
func stopOpenCodeServerForMove(convID string) error {
	runtime, err := db.GetOpenCodeRuntimeByConvID(convID)
	if err != nil || runtime == nil {
		return err
	}
	return stopOpenCodeRuntime(runtime.SessionID)
}

// moveOpenCodeStateOut gives up an OpenCode agent's private state for an
// implementation that reads the shared store. Every conversation in it moves and
// keeps its id, older ones from before a reincarnation too, because the state
// goes: tclaude reads an agent's conversations from its allocation while it has
// one. The agent must be stopped. The copies are complete and verified when this
// returns and the private state is still there, so commit removes it once the
// posture is recorded and its allocation forgotten, and undo leaves everything as
// it was.
func moveOpenCodeStateOut(
	h *harness.Harness, agentID, convID, cwd string,
) (commit, undo func(), note string, fail *spawnFailure) {
	commit, undo = func() {}, func() {}
	refuse := func(err error) (func(), func(), string, *spawnFailure) {
		return func() {}, func() {}, "", &spawnFailure{http.StatusInternalServerError, "opencode_state_move",
			"move the conversation out of private state: " + err.Error()}
	}
	allocation, private, err := openCodeStateToMoveOut(h, agentID, convID, cwd)
	if err != nil {
		return refuse(err)
	}
	if private == nil {
		return commit, undo, "", nil
	}
	if err := stopOpenCodeServerForMove(convID); err != nil {
		return refuse(err)
	}
	conversations, err := harness.OpenCodeConversations(private.History, cwd)
	if err != nil {
		return refuse(err)
	}
	var copied []string
	discard := func() {
		for _, id := range copied {
			if err := harness.DeleteOpenCodeConversation(h.History, id, cwd); err != nil {
				slog.Warn("sandbox assignment: shared OpenCode copy left behind", "conv", short8(id), "error", err)
			}
		}
	}
	for _, id := range conversations {
		inShared, err := harness.OpenCodeHoldsConversation(h.History, id, cwd)
		switch {
		case err != nil:
		case inShared:
			// An import would merge into what is there, and the private store
			// is the sandboxed agent's to write. Only a true duplicate is fine.
			var same bool
			if same, err = harness.OpenCodeSameConversation(private.History, h.History, id, cwd); err == nil && !same {
				err = fmt.Errorf("the shared store already holds a different conversation %s", id)
			}
		default:
			copied = append(copied, id)
			err = harness.CopyOpenCodeConversation(private.History, h.History, id, cwd)
		}
		if err != nil {
			discard()
			return refuse(err)
		}
	}
	return func() {
		// The assignment has forgotten the allocation: a directory nothing
		// points at is only unused.
		if err := os.RemoveAll(allocation.StateRoot); err != nil {
			slog.Warn("sandbox assignment: private OpenCode state left behind", "agent", agentID, "error", err)
		}
	}, discard, openCodeMoveOutNote, nil
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
	// Restart reports the stop/resume an assignment with restart performed on a
	// live agent. Absent on a read and on an offline assignment.
	Restart         string `json:"restart,omitempty"`
	SwitchedClients int    `json:"switched_clients,omitempty"`
	// Note is what an operator should know about what the assignment did.
	Note string `json:"note,omitempty"`
}

// sandboxImplementationRestart is what a restarting assignment adds to the
// posture it reports.
type sandboxImplementationRestart struct {
	Action          string
	SwitchedClients int
}

// writeSandboxImplementationResponse renders the posture a relaunch would use.
// previous is the implementation an assignment just replaced, and is empty on a
// read — the difference is what lets a client report a change rather than a
// state.
func writeSandboxImplementationResponse(
	w http.ResponseWriter, convID, previous string, restart *sandboxImplementationRestart, note string,
) {
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
	var restartAction string
	var switchedClients int
	if restart != nil {
		restartAction, switchedClients = restart.Action, restart.SwitchedClients
	}
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
		Restart:         restartAction,
		SwitchedClients: switchedClients,
		Note:            note,
	})
}
