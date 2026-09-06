package agentd

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/platform/conversation"
	"github.com/tofutools/tclaude/pkg/claude/platform/execution"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// admitManagedHookConversation is the tclaude-layer adapter between an
// authenticated broker request and the portable binding store. It is called
// before PrepareHookEvent, so a non-admitted observation cannot alter current
// session or conversation state.
func admitManagedHookConversation(
	row *db.SessionRow,
	callerPID int,
	harnessPID int,
	req session.BrokeredHookRequest,
) (conversation.Decision, error) {
	ref, ok, err := managedConversationReference(row, req.Input.ConvID, req.ExitGeneration)
	if err != nil {
		return conversation.Decision{}, err
	}
	if !ok {
		return conversation.Decision{Outcome: conversation.Ambiguous, Reason: "managed harness state namespace is not durably known"}, nil
	}
	if ref.Value == "" {
		return conversation.Decision{Outcome: conversation.Ambiguous, Reason: "managed hook did not name a conversation"}, nil
	}
	transition := managedHookTransition(req.Input)
	return admitManagedConversation(row, callerPID, harnessPID, req.ExitGeneration, ref, transition, true)
}

// admitManagedStatuslineConversation replays an already-current admission.
// Statusline cadence can confirm a binding but cannot classify a conversation
// transition, so a changed or first reference waits for a genuine hook.
func admitManagedStatuslineConversation(
	row *db.SessionRow,
	callerPID int,
	harnessPID int,
	exitGeneration, convID string,
) (conversation.Decision, error) {
	ref, ok, err := managedConversationReference(row, convID, exitGeneration)
	if err != nil {
		return conversation.Decision{}, err
	}
	if !ok {
		return conversation.Decision{Outcome: conversation.Ambiguous, Reason: "managed harness state namespace is not durably known"}, nil
	}
	if ref.Value == "" {
		return conversation.Decision{Outcome: conversation.Ambiguous, Reason: "managed statusline did not name a conversation"}, nil
	}
	return admitManagedConversation(row, callerPID, harnessPID, exitGeneration, ref, conversation.Unspecified, false)
}

func managedConversationReference(row *db.SessionRow, value, rawGeneration string) (conversation.Reference, bool, error) {
	harnessName := strings.TrimSpace(row.Harness)
	if harnessName == "" {
		harnessName = db.DefaultHarness
	}
	namespace, ok, err := managedConversationNamespace(row, harnessName, rawGeneration)
	if err != nil || !ok {
		return conversation.Reference{}, ok, err
	}
	return conversation.Reference{
		Harness:   harnessName,
		Namespace: namespace,
		Value:     strings.TrimSpace(value),
	}, true, nil
}

func managedConversationNamespace(row *db.SessionRow, harnessName, rawGeneration string) (string, bool, error) {
	executionID, err := execution.ParseID(strings.TrimSpace(rawGeneration))
	if err != nil {
		return "", false, nil
	}
	raw, err := db.SessionExecutionBoundary(row.ID)
	if err != nil || strings.TrimSpace(raw) == "" {
		return "", false, err
	}
	var boundary session.ExecutionBoundary
	if err := json.Unmarshal([]byte(raw), &boundary); err != nil {
		return "", false, nil
	}
	boundaryExecutionID, err := execution.ParseID(strings.TrimSpace(boundary.LaunchGeneration))
	if err != nil || boundaryExecutionID != executionID ||
		boundary.SandboxImplementation != string(sandboxpolicy.ImplementationTclaudeLayer) ||
		boundary.OuterLayerRenderInput == nil || boundary.StateStoreIdentity == nil ||
		strings.TrimSpace(boundary.Harness.Name) != harnessName {
		return "", false, nil
	}
	h, ok := harness.Get(harnessName)
	if !ok || h.StateStore == nil {
		return "", false, nil
	}
	contract := boundary.OuterLayerRenderInput.Contract
	if err := h.StateStore.ValidateStateStoreIdentity(harness.FrozenStateStoreContract{
		HarnessName: contract.HarnessName,
		StateRoot:   contract.StateRoot,
	}, *boundary.StateStoreIdentity); err != nil {
		return "", false, nil
	}
	return boundary.StateStoreIdentity.Namespace, true, nil
}

func managedHookTransition(input session.HookCallbackInput) conversation.Transition {
	if input.HookEventName != "SessionStart" {
		return conversation.Unspecified
	}
	switch strings.ToLower(strings.TrimSpace(input.Source)) {
	case "clear":
		return conversation.Clear
	case "compact":
		return conversation.Continue
	case "resume":
		return conversation.Resume
	case "startup", "":
		// Startup proves neither continuity nor replacement once a current
		// binding exists. It is sufficient only for the first reference, where
		// admitManagedConversation has no predecessor to classify against.
		return conversation.Unspecified
	default:
		return conversation.Unspecified
	}
}

func admitManagedConversation(
	row *db.SessionRow,
	callerPID int,
	harnessPID int,
	rawGeneration string,
	ref conversation.Reference,
	transition conversation.Transition,
	allowInitial bool,
) (conversation.Decision, error) {
	if row == nil {
		return conversation.Decision{Outcome: conversation.Ambiguous, Reason: "managed main process is unavailable"}, nil
	}
	executionID, err := execution.ParseID(strings.TrimSpace(rawGeneration))
	if err != nil {
		return conversation.Decision{Outcome: conversation.Ambiguous, Reason: "managed launch generation is absent or malformed"}, nil
	}
	attempt := execution.AttemptRef{ExecutionID: executionID, LegacySessionID: row.ID}
	identity, err := db.GetSessionExitLaunchIdentity(row.ID)
	if err != nil {
		return conversation.Decision{}, err
	}
	if identity.Generation != executionID.String() || identity.TmuxSession != row.TmuxSession {
		return conversation.Decision{Outcome: conversation.Historical, Reason: "hook belongs to a stale managed attempt"}, nil
	}
	pane, err := brokerLivePaneProbe(row.TmuxSession)
	if err != nil || pane.state != paneProbeLive || pane.panePID <= 1 || pane.paneID == "" ||
		pane.generation != executionID.String() || identity.PaneID != pane.paneID {
		return conversation.Decision{Outcome: conversation.Ambiguous, Reason: "live managed pane identity is unavailable"}, nil
	}
	mainPID, ok := managedMainProcessInLineage(row.Harness, callerPID, pane.panePID)
	if !ok || (harnessPID > 1 && harnessPID != mainPID) {
		return conversation.Decision{Outcome: conversation.Rejected, Reason: "caller is not a child of the sole managed harness process"}, nil
	}
	processInstance, ok := brokerProcessInstance(mainPID)
	if !ok || strings.TrimSpace(processInstance) == "" {
		return conversation.Decision{Outcome: conversation.Ambiguous, Reason: "main process instance is unavailable"}, nil
	}

	// Spawn initially records tmux's pane shell. Once the daemon has proved the
	// sole harness process between this socket peer and that exact pane, promote
	// the durable association before the store atomically rechecks it.
	bound, err := db.BindManagedAttemptMainPID(
		attempt, row.TmuxSession, pane.paneID, row.PID, mainPID,
	)
	if err != nil {
		return conversation.Decision{}, err
	}
	if !bound {
		return conversation.Decision{Outcome: conversation.Historical, Reason: "managed attempt association changed before admission"}, nil
	}

	current, found, err := db.CurrentConversationAdmission(executionID)
	if err != nil {
		return conversation.Decision{}, err
	}
	if found && current.Reference == ref {
		if current.Evidence.ProcessInstance != processInstance || current.Evidence.PID != mainPID {
			return conversation.Decision{Outcome: conversation.Rejected, Reason: "current binding belongs to a different process instance"}, nil
		}
		return db.AdmitConversationBinding(current)
	}
	if found && transition == conversation.Unspecified {
		return conversation.Decision{Outcome: conversation.Ambiguous, Reason: "changed conversation was not classified by a managed transition"}, nil
	}
	if !found && !allowInitial {
		return conversation.Decision{Outcome: conversation.Ambiguous, Reason: "managed observation cannot initialize a conversation binding"}, nil
	}
	if !found && transition == conversation.Unspecified {
		// The first trustworthy hook may follow a missed SessionStart. With no
		// predecessor selection there is nothing to confuse with a child rotation.
		transition = conversation.Continue
	}

	expected := conversation.Revision(0)
	if found {
		expected = current.ExpectedRevision + 1
	}
	a := conversation.Admission{
		Origin:           conversation.Managed,
		Attempt:          attempt,
		ExpectedRevision: expected,
		Reference:        ref,
		Transition:       transition,
		Evidence: conversation.Evidence{
			ID:              managedEvidenceID(executionID, ref, transition),
			ProcessInstance: processInstance,
			MainProcess:     true,
			Strength:        conversation.VerifiedMainProcess,
			Source:          conversation.HostAdapter,
			PID:             mainPID,
			TmuxSession:     row.TmuxSession,
			PaneID:          pane.paneID,
		},
	}
	return db.AdmitConversationBinding(a)
}

func managedMainProcessInLineage(harnessName string, callerPID, panePID int) (int, bool) {
	const maxAncestorHops = 256
	cur := callerPID
	count := 0
	observedHarnessPID := 0
	for range maxAncestorHops {
		if cur <= 1 {
			return 0, false
		}
		if cur == panePID {
			return observedHarnessPID, count == 1
		}
		if managedHarnessProcessAt(harnessName, cur) {
			count++
			if observedHarnessPID == 0 {
				observedHarnessPID = cur
			}
		}
		cur = procParent(cur)
	}
	return 0, false
}

func managedHarnessProcessAt(expectedHarness string, pid int) bool {
	name := procName(pid)
	if observed := harnessNameAt(pid, name); observed != "" {
		if expectedHarness == harness.DefaultName {
			return observed == "node" || observed == harness.DefaultName
		}
		return observed == expectedHarness
	}
	// Claude's native distribution executes a version-named binary (for
	// example 2.1.234). Treat that strict basename as a Claude runtime only
	// inside an already generation-bound Claude launch. A nested process can
	// add a second candidate, causing refusal, but cannot hide the outer main.
	if expectedHarness != db.DefaultHarness {
		return false
	}
	return isThreePartNumericVersion(name) || isThreePartNumericVersion(procExeName(pid))
}

func isThreePartNumericVersion(name string) bool {
	parts := strings.Split(name, ".")
	if len(parts) != 3 {
		return false
	}
	for _, part := range parts {
		if part == "" {
			return false
		}
		for _, r := range part {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	return true
}

func managedEvidenceID(
	executionID execution.ID,
	ref conversation.Reference,
	transition conversation.Transition,
) string {
	// The broker transport has no harness-issued event ID. This conservative
	// identity is therefore stable for the lifetime of the attempt and must not
	// include mutable store state such as ExpectedRevision. In particular, an
	// old clear replayed after a later clear must resolve to its original
	// history row rather than being laundered into a fresh transition.
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%s",
		executionID, ref.Harness, ref.Namespace, ref.Value, transition)))
	return "hook-" + hex.EncodeToString(sum[:16])
}
