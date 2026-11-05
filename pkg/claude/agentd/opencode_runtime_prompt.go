package agentd

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/opencodeapi"
)

type openCodePromptAcceptanceUncertainError struct {
	err error
}

func (e *openCodePromptAcceptanceUncertainError) Error() string {
	return e.err.Error()
}

func (e *openCodePromptAcceptanceUncertainError) Unwrap() error {
	return e.err
}

func sendOpenCodePrompt(launch *openCodeLaunch, cwd, prompt, model, effort string) error {
	return sendOpenCodePromptWithMessageID(launch, cwd, prompt, model, effort, "")
}

func sendOpenCodePromptWithMessageID(
	launch *openCodeLaunch,
	cwd, prompt, model, effort, messageID string,
) error {
	if launch == nil || prompt == "" {
		return nil
	}
	body := map[string]any{
		"parts": []map[string]string{{"type": "text", "text": prompt}},
	}
	if provider, modelID, ok := strings.Cut(model, "/"); ok && provider != "" && modelID != "" {
		body["model"] = map[string]string{"providerID": provider, "modelID": modelID}
	}
	if effort != "" {
		body["variant"] = effort
	}
	if messageID != "" {
		body["messageID"] = messageID
	}
	endpoint := launch.ServerURL + "/session/" + url.PathEscape(launch.ConvID) +
		"/prompt_async?directory=" + url.QueryEscape(cwd)
	runtime := db.OpenCodeRuntime{
		PID: launch.PID, ServerURL: launch.ServerURL, Password: launch.Password,
		Transport: launch.Transport, ControlSocketPath: launch.ControlSocketPath,
		ControlSocketDevice: launch.ControlSocketDevice,
		ControlSocketInode:  launch.ControlSocketInode,
	}
	request, err := openCodeRequest(http.MethodPost, endpoint, runtime, body)
	if err != nil {
		return err
	}
	response, err := opencodeapi.Do(openCodeHTTPClient, request, runtime)
	if err != nil {
		return &openCodePromptAcceptanceUncertainError{
			err: fmt.Errorf("submit OpenCode launch prompt: %w", err),
		}
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		return fmt.Errorf("submit OpenCode launch prompt: HTTP %d", response.StatusCode)
	}
	return nil
}

// sendOpenCodeNudge delivers a queued inbox nudge to the conversation owned by
// the managed OpenCode server. OpenCode's tmux pane is only an attach client;
// typing untrusted message content into that pane can miss the TUI entirely and
// reach its foreground shell. The server prompt endpoint is the authoritative
// input channel and preserves the nudge as one user turn.
//
// A missing or unhealthy runtime is a delivery failure, not permission to fall
// back to send-keys. Returning an error lets the shared nudge queue release its
// durable claim and retry later without losing the inbox row. A runtime that
// stopped is reconciled once before the prompt is attempted.
//
// Delivery is at-least-once: if prompt_async accepts the turn but its response
// or the queue completion stamp is lost, retry may submit it again. The framed
// message ID is the recipient's deduplication cue.
func sendOpenCodeNudge(convID, nudge string) error {
	runtime, err := readyOpenCodeRuntime(convID)
	if err != nil {
		return err
	}
	return sendOpenCodePrompt(&openCodeLaunch{
		SessionID:           runtime.SessionID,
		ConvID:              runtime.ConvID,
		ServerURL:           runtime.ServerURL,
		Password:            runtime.Password,
		PID:                 runtime.PID,
		Transport:           runtime.Transport,
		ControlSocketPath:   runtime.ControlSocketPath,
		ControlSocketDevice: runtime.ControlSocketDevice,
		ControlSocketInode:  runtime.ControlSocketInode,
	}, runtime.Cwd, nudge, "", "")
}

func openCodePromptMessageExists(
	runtime db.OpenCodeRuntime,
	messageID string,
) (bool, error) {
	endpoint := runtime.ServerURL + "/session/" + url.PathEscape(runtime.ConvID) +
		"/message/" + url.PathEscape(messageID) +
		"?directory=" + url.QueryEscape(runtime.Cwd)
	request, err := openCodeRequest(http.MethodGet, endpoint, runtime, nil)
	if err != nil {
		return false, err
	}
	response, err := opencodeapi.Do(openCodeHTTPClient, request, runtime)
	if err != nil {
		return false, fmt.Errorf("reconcile OpenCode prompt message: %w", err)
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf(
			"reconcile OpenCode prompt message: HTTP %d", response.StatusCode)
	}
}

// sendOpenCodeStandingOrderNudge wraps prompt submission in a durable origin
// handshake. The SSE projector activates it only when OpenCode publishes an
// assistant message whose parent is the exact submitted user-message ID, then
// suppresses standing-order evaluation until Stop. Unrelated queued prompts
// and late events from an old conversation generation cannot steal the marker.
func sendOpenCodeStandingOrderNudge(
	convID, targetAgent string,
	origin *db.StandingOrderAgentMessageOrigin,
	nudge string,
) error {
	if origin == nil {
		return fmt.Errorf("standing-order OpenCode delivery requires trusted provenance")
	}
	if targetAgent == "" {
		var err error
		targetAgent, err = db.AgentIDForConv(convID)
		if err != nil {
			return fmt.Errorf("resolve standing-order target agent: %w", err)
		}
	}
	if targetAgent == "" {
		return fmt.Errorf("standing-order OpenCode delivery requires a stable target agent")
	}
	runtime, err := readyOpenCodeRuntime(convID)
	if err != nil {
		return err
	}
	exists, err := openCodePromptMessageExists(*runtime, origin.OpenCodeMessageID)
	if err != nil {
		return err
	}
	if exists {
		// A previous prompt_async attempt was accepted even if its response was
		// lost. Never resubmit the same ID: OpenCode may reject the duplicate
		// while the original remains queued. Reconcile its assistant from
		// history so a completed turn does not create a phantom 24-hour pending
		// marker when only the inbox delivery stamp was lost.
		historyState, historyErr := openCodePromptTurnHistory(
			*runtime, origin.OpenCodeMessageID)
		if historyErr != nil {
			return historyErr
		}
		current, getErr := db.GetStandingOrderTurnOrigin(
			targetAgent, convID, time.Now())
		if getErr != nil {
			return getErr
		}
		if current != nil &&
			(current.MessageID != origin.MessageID ||
				current.OpenCodeMessageID != origin.OpenCodeMessageID) {
			return fmt.Errorf("another standing-order turn is pending or active")
		}
		if historyState == openCodePromptTurnCompleted {
			if current != nil && current.State == db.StandingOrderTurnOriginActive {
				return db.CompleteStandingOrderTurnOrigin(targetAgent, convID)
			}
			if current != nil {
				return db.CancelPendingStandingOrderTurnOrigin(
					targetAgent, convID, origin.MessageID,
					origin.OpenCodeMessageID)
			}
			return nil
		}
		if current == nil {
			if armErr := db.ArmStandingOrderTurnOrigin(
				targetAgent, convID, origin.MessageID, origin.OpenCodeMessageID,
				time.Now(), standingOrderOriginActiveTTL); armErr != nil {
				return fmt.Errorf("restore accepted standing-order turn origin: %w", armErr)
			}
			current = &db.StandingOrderTurnOrigin{
				TargetAgent: targetAgent, TargetConv: convID,
				MessageID: origin.MessageID, OpenCodeMessageID: origin.OpenCodeMessageID,
				State: db.StandingOrderTurnOriginPending,
			}
		}
		if historyState == openCodePromptTurnActive &&
			current.State == db.StandingOrderTurnOriginPending {
			_, activateErr := db.ActivateStandingOrderTurnOrigin(
				targetAgent, convID, origin.OpenCodeMessageID,
				time.Now(), standingOrderOriginActiveTTL)
			return activateErr
		}
		if current.State == db.StandingOrderTurnOriginPending {
			if refreshErr := db.RefreshPendingStandingOrderTurnOrigin(
				targetAgent, convID, origin.MessageID, origin.OpenCodeMessageID,
				time.Now(), standingOrderOriginActiveTTL); refreshErr != nil {
				return refreshErr
			}
		}
		return nil
	}
	if err := db.ArmStandingOrderTurnOrigin(
		targetAgent, convID, origin.MessageID, origin.OpenCodeMessageID,
		time.Now(), standingOrderOriginPendingTTL); err != nil {
		return fmt.Errorf("arm standing-order turn origin: %w", err)
	}
	err = sendOpenCodePromptWithMessageID(&openCodeLaunch{
		SessionID:           runtime.SessionID,
		ConvID:              runtime.ConvID,
		ServerURL:           runtime.ServerURL,
		Password:            runtime.Password,
		PID:                 runtime.PID,
		Transport:           runtime.Transport,
		ControlSocketPath:   runtime.ControlSocketPath,
		ControlSocketDevice: runtime.ControlSocketDevice,
		ControlSocketInode:  runtime.ControlSocketInode,
	}, runtime.Cwd, nudge, "", "", origin.OpenCodeMessageID)
	if err != nil {
		var uncertain *openCodePromptAcceptanceUncertainError
		if errors.As(err, &uncertain) {
			// The server may have accepted and persisted the user message before
			// the transport failed. A successful exact lookup turns the attempt
			// into a confirmed delivery; lookup failure keeps the short pending
			// marker and the durable inbox row for a later reconciliation.
			if accepted, lookupErr := openCodePromptMessageExists(
				*runtime, origin.OpenCodeMessageID); lookupErr == nil && accepted {
				_ = db.RefreshPendingStandingOrderTurnOrigin(
					targetAgent, convID, origin.MessageID,
					origin.OpenCodeMessageID, time.Now(),
					standingOrderOriginActiveTTL)
				return nil
			}
		} else {
			if cancelErr := db.CancelPendingStandingOrderTurnOrigin(
				targetAgent, convID, origin.MessageID,
				origin.OpenCodeMessageID); cancelErr != nil {
				slog.Warn("OpenCode standing-order origin: cancel failed prompt handshake",
					"agent", targetAgent, "message", origin.MessageID,
					"error", cancelErr)
			}
		}
		return err
	}
	// A successful response proves acceptance. Give a reminder queued behind a
	// racing human prompt the same generous bound as an active turn. If SSE
	// already promoted (or even completed) the marker, the refresh correctly
	// misses and no delivery retry is needed.
	if refreshErr := db.RefreshPendingStandingOrderTurnOrigin(
		targetAgent, convID, origin.MessageID, origin.OpenCodeMessageID,
		time.Now(), standingOrderOriginActiveTTL); refreshErr != nil {
		slog.Debug("OpenCode standing-order origin: accepted prompt already advanced",
			"agent", targetAgent, "message", origin.MessageID, "error", refreshErr)
	}
	return nil
}

// sendOpenCodeTUICommand publishes a command through the managed server's TUI
// event API. Unlike tmux send-keys, command dispatch is independent of prompt
// mode and user keybinding customizations. expectedSessionID binds lifecycle
// sends to the selected process generation; empty is allowed for non-lifecycle
// callers that already selected by conversation.
func sendOpenCodeTUICommand(
	convID, expectedSessionID string,
	command openCodeTUICommand,
) error {
	runtime, err := readyOpenCodeRuntime(convID)
	if err != nil {
		return err
	}
	if expectedSessionID != "" && runtime.SessionID != expectedSessionID {
		return fmt.Errorf(
			"managed OpenCode runtime session changed for conversation %s", convID,
		)
	}
	// Health reconciliation can take long enough for an idle session to start
	// working or present a permission/question dialog after its caller's first
	// status check. Re-read immediately before the POST so a stale selection
	// cannot dispatch compact/exit into a newly non-idle TUI.
	if beforeOpenCodeTUICommandStatusCheckForTest != nil {
		beforeOpenCodeTUICommandStatusCheckForTest()
	}
	sess := aliveSessionForConv(convID)
	if sess == nil || sess.ID != runtime.SessionID {
		return fmt.Errorf("managed OpenCode session changed for conversation %s", convID)
	}
	if openCodeControlInputBlocked(sess.Status) {
		return fmt.Errorf("managed OpenCode TUI is %s; retry when idle", sess.Status)
	}
	body := map[string]any{
		"type": "tui.command.execute",
		"properties": map[string]string{
			"command": string(command),
		},
	}
	endpoint := runtime.ServerURL + "/tui/publish?directory=" +
		url.QueryEscape(runtime.Cwd)
	request, err := openCodeRequest(http.MethodPost, endpoint, *runtime, body)
	if err != nil {
		return fmt.Errorf("build OpenCode TUI command request: %w", err)
	}
	response, err := opencodeapi.Do(openCodeHTTPClient, request, *runtime)
	if err != nil {
		return fmt.Errorf("publish OpenCode TUI command: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("publish OpenCode TUI command: HTTP %d", response.StatusCode)
	}
	return nil
}
