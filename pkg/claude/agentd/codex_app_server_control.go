package agentd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/codexappserver"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

const codexAppServerPollInterval = 50 * time.Millisecond

var (
	codexAppServerMutationTimeout = 15 * time.Second
	codexAppServerCallTimeout     = 5 * time.Second
)

var (
	errCodexControlStarting     = errors.New("codex app-server control is still starting")
	errCodexControlBusy         = errors.New("codex thread is busy awaiting human interaction")
	errCodexControlDisconnected = errors.New("codex app-server control is disconnected")
	errCodexControlUnsupported  = errors.New("codex thread state does not support this operation")
	errCodexControlAmbiguous    = errors.New("codex app-server mutation outcome is ambiguous")
)

type codexThreadStatus struct {
	Type        string   `json:"type"`
	ActiveFlags []string `json:"activeFlags"`
}

type codexCompactionStage struct {
	followUp         string
	followUpClientID string
	beforeCount      int
	started          bool
	committed        bool
}

func (h *codexAppServerHandle) readThread(ctx context.Context) (codexappserver.Thread, codexThreadStatus, error) {
	thread, err := h.client.ReadThread(ctx, codexappserver.ThreadReadParams{
		ThreadID: h.runtime.ThreadID, IncludeTurns: true,
	})
	if err != nil {
		return codexappserver.Thread{}, codexThreadStatus{}, err
	}
	var status codexThreadStatus
	if err := json.Unmarshal(thread.Status, &status); err != nil || status.Type == "" {
		return codexappserver.Thread{}, codexThreadStatus{}, fmt.Errorf(
			"%w: decode thread status %s", codexappserver.ErrProtocol, thread.Status)
	}
	return thread, status, nil
}

func codexThreadAwaitingHuman(status codexThreadStatus) bool {
	for _, flag := range status.ActiveFlags {
		if flag == "waitingOnApproval" || flag == "waitingOnUserInput" {
			return true
		}
	}
	return false
}

func codexActiveTurn(thread codexappserver.Thread) (codexappserver.Turn, bool) {
	for i := len(thread.Turns) - 1; i >= 0; i-- {
		if thread.Turns[i].Status == "inProgress" {
			return thread.Turns[i], true
		}
	}
	return codexappserver.Turn{}, false
}

func codexCallAmbiguous(err error) bool {
	var callErr *codexappserver.CallError
	return errors.As(err, &callErr) && callErr.Ambiguous
}

// codexThreadContainsMessage checks both Codex's client id projection and the
// exact framed input. The latter is the durable compatibility key: inbox
// framing already contains the stable message id, including for records made
// before clientUserMessageId was populated.
func codexThreadContainsMessage(thread codexappserver.Thread, clientID, text string) bool {
	quotedText, _ := json.Marshal(text)
	for _, turn := range thread.Turns {
		for _, item := range turn.Items {
			var projection struct {
				ClientID *string `json:"clientId"`
			}
			if json.Unmarshal(item, &projection) == nil && projection.ClientID != nil {
				if clientID != "" && *projection.ClientID == clientID {
					return true
				}
				// Client-scoped items belong to one exact operation. Their text
				// must not make a later operation with the same prose look settled.
				continue
			}
			if text != "" && bytes.Contains(item, quotedText) {
				return true
			}
		}
	}
	return false
}

func codexAppServerMessageID(messageID int64) string {
	return "tclaude-agent-message-" + strconv.FormatInt(messageID, 10)
}

func readyCodexAppServerHandle(convID string) (*codexAppServerHandle, error) {
	handle := codexAppServerHandleForConv(convID)
	var runtime *db.CodexAppServerRuntime
	if handle != nil {
		runtime, _ = db.GetCodexAppServerRuntime(handle.runtime.Generation)
	} else {
		runtime, _ = dbCodexRuntime(convID)
	}
	if handle != nil && runtime != nil && runtime.State == db.CodexAppServerReady &&
		runtime.Generation == handle.runtime.Generation && runtime.ThreadID == handle.runtime.ThreadID {
		return handle, nil
	}
	if runtime != nil && (runtime.State == db.CodexAppServerWarming || runtime.State == db.CodexAppServerRecovering) {
		return nil, errCodexControlStarting
	}
	return nil, errCodexControlDisconnected
}

func sendCodexAppServerMessage(convID string, messageID int64, text string) error {
	handle, err := readyCodexAppServerHandle(convID)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), codexAppServerMutationTimeout)
	defer cancel()
	handle.mutations.Lock()
	defer handle.mutations.Unlock()
	return handle.sendMessageLocked(ctx, codexAppServerMessageID(messageID), text, true)
}

// dbCodexRuntime is a tiny seam kept here to make route-state classification
// explicit without teaching callers about the durable runtime table.
func dbCodexRuntime(convID string) (*db.CodexAppServerRuntime, error) {
	return db.GetCodexAppServerRuntimeByConvID(convID)
}

func (h *codexAppServerHandle) sendMessageLocked(
	ctx context.Context, clientID, text string, retryProvenAbsent bool,
) error {
	thread, status, err := h.readThread(ctx)
	if err != nil {
		return err
	}
	if codexThreadContainsMessage(thread, clientID, text) {
		return nil
	}
	if codexThreadAwaitingHuman(status) {
		return errCodexControlBusy
	}
	var mutationErr error
	callCtx, cancelCall := context.WithTimeout(ctx, codexAppServerCallTimeout)
	defer cancelCall()
	switch status.Type {
	case "idle":
		_, mutationErr = h.client.StartTurn(callCtx, codexappserver.TurnStartParams{
			ThreadID: h.runtime.ThreadID, Input: []codexappserver.UserInput{codexappserver.TextInput(text)},
			ClientUserMessageID: &clientID,
		})
	case "active":
		active, ok := codexActiveTurn(thread)
		if !ok {
			return errCodexControlBusy
		}
		_, mutationErr = h.client.SteerTurn(callCtx, codexappserver.TurnSteerParams{
			ThreadID: h.runtime.ThreadID, ExpectedTurnID: active.ID,
			Input:               []codexappserver.UserInput{codexappserver.TextInput(text)},
			ClientUserMessageID: &clientID,
		})
	case "notLoaded", "systemError":
		return fmt.Errorf("%w: thread status %s", errCodexControlUnsupported, status.Type)
	default:
		return fmt.Errorf("%w: unknown thread status %q", errCodexControlUnsupported, status.Type)
	}
	if mutationErr == nil {
		return nil
	}

	// Busy/stale-turn errors are expected when the human wins the race after
	// our snapshot. A fresh snapshot decides whether the message landed; an
	// absent message remains queued. Ambiguous transport completion uses the
	// same proof, and retries at most once when absence is positively observed.
	reconciled, _, readErr := h.readThread(ctx)
	if readErr == nil && codexThreadContainsMessage(reconciled, clientID, text) {
		return nil
	}
	if codexCallAmbiguous(mutationErr) {
		if readErr != nil {
			return fmt.Errorf("%w: %v; reconciliation failed: %v", errCodexControlAmbiguous, mutationErr, readErr)
		}
		if retryProvenAbsent {
			return h.sendMessageLocked(ctx, clientID, text, false)
		}
		return fmt.Errorf("%w: %v", errCodexControlAmbiguous, mutationErr)
	}
	return mutationErr
}

func renameCodexAppServerThread(convID, title string) error {
	handle, handleErr := readyCodexAppServerHandle(convID)
	if handleErr != nil {
		return handleErr
	}
	ctx, cancel := context.WithTimeout(context.Background(), codexAppServerMutationTimeout)
	defer cancel()
	handle.mutations.Lock()
	defer handle.mutations.Unlock()
	thread, _, err := handle.readThread(ctx)
	if err != nil {
		return err
	}
	if thread.Name != nil && *thread.Name == title {
		return nil
	}
	callCtx, cancelCall := context.WithTimeout(ctx, codexAppServerCallTimeout)
	err = handle.client.SetThreadName(callCtx, handle.runtime.ThreadID, title)
	cancelCall()
	if err == nil {
		return nil
	}
	if !codexCallAmbiguous(err) {
		return err
	}
	thread, _, readErr := handle.readThread(ctx)
	if readErr == nil && thread.Name != nil && *thread.Name == title {
		return nil
	}
	return fmt.Errorf("%w: rename: %v", errCodexControlAmbiguous, err)
}

// forkCodexAppServerThread creates the clone identity exactly once while the
// source is verified and idle. thread/fork has no idempotency key, so an
// ambiguous transport result is surfaced and never replayed.
func forkCodexAppServerThread(convID, cwd string) (string, error) {
	handle, handleErr := readyCodexAppServerHandle(convID)
	if handleErr != nil {
		return "", handleErr
	}
	ctx, cancel := context.WithTimeout(context.Background(), codexAppServerMutationTimeout)
	defer cancel()
	handle.mutations.Lock()
	defer handle.mutations.Unlock()
	thread, status, err := handle.readThread(ctx)
	if err != nil {
		return "", err
	}
	if status.Type != "idle" || codexThreadAwaitingHuman(status) {
		return "", errCodexControlBusy
	}
	params := codexappserver.ThreadForkParams{ThreadID: handle.runtime.ThreadID}
	if cwd != "" {
		params.Cwd = &cwd
	}
	if len(thread.Turns) > 0 {
		last := thread.Turns[len(thread.Turns)-1]
		if last.Status == "inProgress" {
			return "", errCodexControlBusy
		}
		params.LastTurnID = &last.ID
	}
	callCtx, cancelCall := context.WithTimeout(ctx, codexAppServerCallTimeout)
	forked, err := handle.client.ForkThread(callCtx, params)
	cancelCall()
	if err != nil {
		if codexCallAmbiguous(err) {
			return "", fmt.Errorf("%w: thread/fork may have created a clone; refusing to retry: %v",
				errCodexControlAmbiguous, err)
		}
		return "", err
	}
	return forked.ID, nil
}

func compactCodexAppServerThread(convID, followUp string) error {
	handle, handleErr := readyCodexAppServerHandle(convID)
	if handleErr != nil {
		return handleErr
	}
	ctx, cancel := context.WithTimeout(context.Background(), codexAppServerMutationTimeout)
	defer cancel()
	handle.mutations.Lock()
	defer handle.mutations.Unlock()
	stage := handle.compact
	if stage != nil && stage.followUp != followUp {
		return fmt.Errorf("%w: a prior compaction has a pending follow-up", errCodexControlBusy)
	}
	if stage == nil {
		before, status, err := handle.readThread(ctx)
		if err != nil {
			return err
		}
		if status.Type != "idle" {
			return errCodexControlBusy
		}
		handle.nextOpID++
		stage = &codexCompactionStage{
			followUp: followUp, beforeCount: codexCompactionCount(before),
			followUpClientID: "tclaude-compact-follow-up-" + handle.runtime.Generation + "-" +
				strconv.FormatUint(handle.nextOpID, 10),
		}
		handle.compact = stage
	}
	if !stage.started {
		// Set before the call: either a response or an ambiguous write means this
		// stage may have started and must only be observed from now on.
		stage.started = true
		callCtx, cancelCall := context.WithTimeout(ctx, codexAppServerCallTimeout)
		compactErr := handle.client.StartCompaction(callCtx, handle.runtime.ThreadID)
		cancelCall()
		if compactErr != nil && !codexCallAmbiguous(compactErr) {
			handle.compact = nil // the server definitively rejected before effect
			return compactErr
		}
		// Compaction has no idempotency key. An ambiguous call is never replayed;
		// the durable thread snapshot below is its only reconciliation path.
	}
	if !stage.committed {
		for {
			thread, current, readErr := handle.readThread(ctx)
			if readErr != nil {
				return fmt.Errorf("%w: compact reconciliation: %v", errCodexControlAmbiguous, readErr)
			}
			if current.Type == "idle" && codexCompactionCount(thread) > stage.beforeCount {
				stage.committed = true
				break
			}
			select {
			case <-ctx.Done():
				return fmt.Errorf("%w: compaction did not settle before deadline", errCodexControlAmbiguous)
			case <-time.After(codexAppServerPollInterval):
			}
		}
	}
	if stage.followUp == "" {
		handle.compact = nil
		return nil
	}
	if err := handle.sendMessageLocked(ctx, stage.followUpClientID, stage.followUp, true); err != nil {
		// Compaction is committed. Retain the exact follow-up identity so a
		// retry resumes this stage and cannot compact or submit twice.
		return fmt.Errorf("compaction committed; follow-up pending: %w", err)
	}
	handle.compact = nil
	return nil
}

func codexCompactionCount(thread codexappserver.Thread) int {
	count := 0
	for _, turn := range thread.Turns {
		for _, item := range turn.Items {
			if bytes.Contains(item, []byte(`"type":"contextCompaction"`)) {
				count++
			}
		}
	}
	return count
}

func interruptCodexAppServerThread(convID string) error {
	handle, handleErr := readyCodexAppServerHandle(convID)
	if handleErr != nil {
		return handleErr
	}
	ctx, cancel := context.WithTimeout(context.Background(), codexAppServerMutationTimeout)
	defer cancel()
	handle.mutations.Lock()
	defer handle.mutations.Unlock()
	thread, status, err := handle.readThread(ctx)
	if err != nil {
		return err
	}
	if status.Type != "active" {
		return fmt.Errorf("%w: no active turn to interrupt", errCodexControlUnsupported)
	}
	turn, ok := codexActiveTurn(thread)
	if !ok {
		return fmt.Errorf("%w: no active turn to interrupt", errCodexControlUnsupported)
	}
	callCtx, cancelCall := context.WithTimeout(ctx, codexAppServerCallTimeout)
	err = handle.client.InterruptTurn(callCtx, handle.runtime.ThreadID, turn.ID)
	cancelCall()
	if err == nil {
		return nil
	}
	if !codexCallAmbiguous(err) {
		return err
	}
	after, _, readErr := handle.readThread(ctx)
	if readErr == nil {
		if active, activeOK := codexActiveTurn(after); !activeOK || active.ID != turn.ID {
			return nil
		}
	}
	return fmt.Errorf("%w: interrupt turn %s: %v", errCodexControlAmbiguous, turn.ID, err)
}

func codexControlFailure(err error) slashFailure {
	switch {
	case errors.Is(err, errCodexControlStarting):
		return slashFailureStarting
	case errors.Is(err, errCodexControlBusy):
		return slashFailureBusy
	case errors.Is(err, errCodexControlUnsupported):
		return slashFailureUnsupported
	case errors.Is(err, errCodexControlDisconnected):
		return slashFailureDisconnected
	default:
		return slashFailureControl
	}
}
