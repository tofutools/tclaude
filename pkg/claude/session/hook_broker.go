package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// HookBrokerEnvVar marks a launch whose hook callbacks must be brokered
// through agentd instead of writing the database directly. The launch
// seam exports it for `tclaude-layer` spawns and resumes, next to the
// other launch-contract environment.
const HookBrokerEnvVar = "TCLAUDE_HOOK_BROKER"

// HookBrokerAgentd is the only recognised value of HookBrokerEnvVar.
const HookBrokerAgentd = "agentd"

// hookBrokerTimeout bounds a brokered hook round-trip. Hooks run in the
// harness's critical path, so a wedged daemon must not hold the agent's
// turn open. Generous enough to cover the daemon's own hook lock being
// held by a concurrent event on the same session.
const hookBrokerTimeout = 20 * time.Second

// brokerHookEvents reports whether this process must hand its hook events
// to agentd rather than apply them itself.
//
// The marker is the load-bearing signal, and the probe below it is a
// backstop for the one case the marker cannot cover: a launch that
// reached the sandbox without it.
//
// The probe looks at the ABSENCE OF THE DATABASE FILE rather than at a
// failed write, and that shape is worth keeping even though the reason
// for it has changed. It was originally the only workable test: the mount
// plan hid `~/.tclaude/data` behind a bwrap --tmpfs, which is EMPTY but
// WRITABLE, so db.Open() would happily create and migrate a complete
// phantom database inside the wall — nothing failed, reads just came back
// empty and writes evaporated with the pane. There was no error for a
// "try the database, fall back on failure" design to catch.
//
// TCL-758 has since made hidden protected roots read-only (the mount plan
// now emits --remount-ro over each hide), so a write inside the wall
// fails outright and no phantom database can be created. That closes the
// probe's old blind spot — an in-sandbox command opening the database
// used to create the file and make the probe report "reachable" for the
// rest of the pane's life — rather than making the probe wrong. Testing
// for absence remains the cheaper and more direct question, and it does
// not depend on which errno a future mount posture produces.
//
// Neither signal is a security boundary. Claiming to be brokered only
// routes the event to a daemon that authenticates the caller from its own
// recorded pids; declining to broker costs the caller nothing but its own
// telemetry, which lands in a database no one else reads.
func brokerHookEvents() bool {
	// `tclaude task run` drives a SEQUENCE of conversations under one
	// env-session and steers them through a signal file the runner watches.
	// That file is a host-side write whose path the sandbox would choose,
	// so the broker refuses to carry it — which means a brokered task hook
	// could not complete a task even if the rest went through. Running the
	// task runner inside a tclaude-layer pane is not a supported
	// combination; say so once, loudly, rather than half-working.
	//
	// This carve-out is HOOK-SPECIFIC and deliberately not part of
	// BrokerHostWrites: nothing else brokered touches the signal file, and
	// declining to broker there would only mean failing in a second place.
	if _, inTaskMode := taskSignalPath(); inTaskMode {
		if os.Getenv(HookBrokerEnvVar) == HookBrokerAgentd {
			slog.Warn("hook broker: `tclaude task run` inside a tclaude-layer pane is not supported; "+
				"applying this hook directly, which cannot reach the real database",
				"module", "hooks")
		}
		return false
	}
	return BrokerHostWrites()
}

// BrokerHostWrites reports whether this process must hand its
// host-touching writes to agentd rather than perform them itself.
//
// It is the shared answer to "is the conversation database reachable from
// in here", and every brokered surface must ask it rather than testing
// the marker alone. A launch cannot be half inside the wall: an agent
// whose hooks broker but whose status line writes directly would lose its
// context snapshot, model, effort, cost and location — and, downstream,
// the pre-compact guard — silently, into a read-only mount, with the
// warning going to a log under the hidden root.
//
// That case is not hypothetical. It is exactly what the probe below the
// marker exists for: a layer launch that arrived without the marker.
func BrokerHostWrites() bool {
	if os.Getenv(HookBrokerEnvVar) == HookBrokerAgentd {
		return true
	}
	return databaseAbsentButDaemonReachable()
}

func databaseAbsentButDaemonReachable() bool {
	path := db.DBPath()
	if path == "" {
		return false
	}
	if _, err := os.Stat(path); err == nil || !os.IsNotExist(err) {
		return false
	}
	// A genuine first run has no database either — but it has no daemon
	// either, since the daemon creates the database on startup. Requiring
	// both keeps first-run direct.
	for _, sock := range agentipc.ClientSocketPaths() {
		if agentipc.SocketReachable(sock) {
			return true
		}
	}
	return false
}

// BrokeredHookRequest is the body a sandboxed hook callback POSTs to
// agentd. It carries the parsed event plus the launch-environment values
// agentd cannot observe for itself; everything else about the caller's
// identity agentd resolves server-side from recorded host pids.
type BrokeredHookRequest struct {
	// Input is the harness's hook payload, already parsed.
	Input HookCallbackInput `json:"input"`

	// ClaimedSessionID is the caller's TCLAUDE_SESSION_ID. It is a
	// CROSS-CHECK ONLY: agentd resolves the real session row from the
	// caller's process ancestry and rejects the request if this disagrees.
	// It is never the authority — see the note at the launch seam about
	// TCLAUDE_SESSION_ID being caller-controlled compatibility state.
	ClaimedSessionID string `json:"claimed_session_id,omitempty"`

	// ExitGeneration is TCLAUDE_EXIT_GENERATION, the per-launch token that
	// lets a stale SessionEnd observation be rejected.
	ExitGeneration string `json:"exit_generation,omitempty"`

	// AutoCompactWindow is the raw pinned auto-compaction window. Parsed,
	// never used raw, by the PreCompact guard.
	AutoCompactWindow string `json:"auto_compact_window,omitempty"`

	// AckToken acknowledges that a prior response's stdout reached the
	// harness. It is opaque, short-lived, and bound server-side to the session
	// agentd resolves from this process; the client cannot mint ledger writes.
	AckToken string `json:"ack_token,omitempty"`
	// RelayFailed consumes AckToken without committing delivery. The callback
	// sends it when stdout relay fails so agentd can release a held delivery
	// lock immediately rather than waiting for token expiry.
	RelayFailed bool `json:"relay_failed,omitempty"`
}

// BrokeredHookResponse carries back whatever the hook would have written to
// its own stdout: a gate decision or standing-order context.
type BrokeredHookResponse struct {
	Stdout   string `json:"stdout,omitempty"`
	AckToken string `json:"ack_token,omitempty"`
}

// brokerHookEvent hands one parsed event to agentd and relays the
// daemon's hook stdout (the PreCompact decision, when there is one) to
// this process's stdout, so Claude Code sees the same bytes it would have
// seen from a direct callback.
//
// Failures are soft. A hook that exits non-zero disrupts the agent's
// turn, and no status update is worth that; an unreachable daemon costs
// this event's telemetry and nothing else. This is strictly better than
// the interim behaviour it replaces, where a tclaude-layer launch dropped
// every event unconditionally.
func brokerHookEvent(input HookCallbackInput, stdout io.Writer) error {
	brokerHookEventIfDelivered(input, stdout)
	return nil
}

// brokerHookEventIfDelivered is brokerHookEvent with the one fact its callers
// on the ORDINARY (unbrokered) path need: whether the daemon actually applied
// the event. A launch that hands over a single event — rather than all of them
// — must apply it itself when the daemon did not take it, or the event would be
// lost outright. Every failure remains soft; only the answer changes.
func brokerHookEventIfDelivered(input HookCallbackInput, stdout io.Writer) bool {
	req := BrokeredHookRequest{
		Input:             input,
		ClaimedSessionID:  os.Getenv("TCLAUDE_SESSION_ID"),
		ExitGeneration:    os.Getenv("TCLAUDE_EXIT_GENERATION"),
		AutoCompactWindow: os.Getenv(harness.AutoCompactWindowEnvVar),
	}
	body, err := json.Marshal(req)
	if err != nil {
		slog.Warn("hook broker: failed to encode event", "error", err, "module", "hooks")
		return false
	}
	body = trimOversizedHookBody(req, body)

	resp, err := postHookToDaemon(body)
	if err != nil {
		// A refusal is louder than a transport failure on purpose. An
		// unreachable daemon is transient and self-corrects; a refusal
		// means the daemon could not place this caller, or placed it on a
		// different session than it claims — most plausibly a stale row
		// whose recorded pid was reused. That does not self-correct: the
		// agent silently loses ALL hook telemetry for its whole life, so
		// the log has to be findable rather than blend into noise.
		if !brokerHookEvents() {
			// A launch that applies its own hooks and only hands over the
			// events the daemon must act on (see autoPermitNeedsDaemon). It
			// keeps its telemetry either way — the caller applies the event
			// locally — so neither outcome is the loud, findable failure the
			// always-brokered case below describes.
			slog.Debug("hook broker: daemon did not take this event; applying it locally",
				"event", input.HookEventName, "error", err, "module", "hooks")
			return false
		}
		if errors.Is(err, errHookBrokerRefused) {
			slog.Error("hook broker: agentd refused this session's hook events; "+
				"the agent's status, ledgers and directory tracking will not update",
				"event", input.HookEventName, "error", err, "module", "hooks")
			return false
		}
		slog.Warn("hook broker: agentd unreachable, dropping event",
			"event", input.HookEventName, "error", err, "module", "hooks")
		return false
	}
	if resp.Stdout != "" {
		if _, err := io.WriteString(stdout, resp.Stdout); err != nil {
			if resp.AckToken != "" {
				if ackErr := acknowledgeBrokeredHook(resp.AckToken, false); ackErr != nil {
					slog.Warn("hook broker: failed to abandon undelivered hook output",
						"error", ackErr, "module", "hooks")
				}
			}
			slog.Warn("hook broker: failed to relay hook decision to stdout",
				"error", err, "module", "hooks")
			// The daemon DID apply the event; only the reply was lost.
			return true
		}
	}
	if resp.AckToken != "" {
		if err := acknowledgeBrokeredHook(resp.AckToken, true); err != nil {
			// The output already reached the harness, so this cannot be
			// retried safely in-place. Leaving cadence open may repeat one
			// reminder at the next boundary; falsely claiming delivery would
			// instead risk permanent silence.
			slog.Warn("hook broker: failed to acknowledge relayed hook output",
				"error", err, "module", "hooks")
		}
	}
	return true
}

func acknowledgeBrokeredHook(token string, delivered bool) error {
	body, err := json.Marshal(BrokeredHookRequest{
		ClaimedSessionID: os.Getenv("TCLAUDE_SESSION_ID"),
		AckToken:         token,
		RelayFailed:      !delivered,
	})
	if err != nil {
		return err
	}
	_, err = postHookToDaemon(body)
	return err
}

// hookBrokerBodyBudget is the client's own ceiling, kept just below the
// daemon's so a trim happens here rather than as a rejection there.
//
// The daemon's cap is 10 MiB by operator direction: large tool payloads
// and messages should normally travel WHOLE, and trimming is meant to be
// the rare last resort that keeps a pathological event deliverable rather
// than a routine part of the path.
const hookBrokerBodyBudget = (10 << 20) - 4096

// trimOversizedHookBody keeps a large event deliverable instead of losing
// it whole.
//
// ToolInput and ToolResponse are raw per-tool JSON, so a Read or Write of
// a big file puts that file's content in the payload — well past any
// sane request ceiling. The direct path applies such an event with no
// size limit at all, so simply letting the daemon reject it would be a
// real parity break: the session would lose the event's status
// transition and last_hook stamp, not merely its tool detail.
//
// Dropping the two bulky fields costs only what reads them: the
// background-shell ledger and the working-directory tracker, both of
// which degrade to "no evidence from this event" — the same thing they
// see for any tool call that is not a background Bash or a file edit.
func trimOversizedHookBody(req BrokeredHookRequest, body []byte) []byte {
	if len(body) <= hookBrokerBodyBudget {
		return body
	}
	slog.Warn("hook broker: event exceeds the payload budget; dropping tool input/response to keep it deliverable",
		"event", req.Input.HookEventName, "tool", req.Input.ToolName,
		"bytes", len(body), "budget", hookBrokerBodyBudget, "module", "hooks")
	req.Input.ToolInput = nil
	req.Input.ToolResponse = nil
	req.Input.PayloadTrimmed = true
	trimmed, err := json.Marshal(req)
	if err != nil {
		return body
	}
	if len(trimmed) > hookBrokerBodyBudget {
		// Still too large: the free text itself is enormous. Clamp the two
		// unbounded strings rather than surrender the event.
		req.Input.Prompt = truncateForBroker(req.Input.Prompt)
		req.Input.LastAssistantMessage = truncateForBroker(req.Input.LastAssistantMessage)
		req.Input.Message = truncateForBroker(req.Input.Message)
		if clamped, err := json.Marshal(req); err == nil {
			return clamped
		}
	}
	return trimmed
}

func truncateForBroker(s string) string {
	const max = 8192
	if len(s) <= max {
		return s
	}
	return s[:max] + "… [truncated by the hook broker]"
}

// errHookBrokerRefused marks a daemon REFUSAL (the caller could not be
// placed, or claimed a session it does not own) as opposed to a transport
// failure. The two want very different operator attention — see the
// branch in brokerHookEvent.
var errHookBrokerRefused = errors.New("agentd refused the brokered hook event")

func postHookToDaemon(body []byte) (*BrokeredHookResponse, error) {
	socks := agentipc.ClientSocketPaths()
	if len(socks) == 0 {
		return nil, fmt.Errorf("no agentd socket path resolved")
	}
	client := &http.Client{
		Timeout: hookBrokerTimeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var lastErr error
				for _, sock := range socks {
					conn, err := (&net.Dialer{}).DialContext(ctx, "unix", sock)
					if err == nil {
						return conn, nil
					}
					lastErr = err
				}
				return nil, lastErr
			},
		},
	}
	req, err := http.NewRequest(http.MethodPost, "http://tclaude/v1/whoami/hook", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	httpResp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = httpResp.Body.Close() }()
	if httpResp.StatusCode != http.StatusOK {
		preview, _ := io.ReadAll(io.LimitReader(httpResp.Body, 512))
		err := fmt.Errorf("agentd returned %d: %s", httpResp.StatusCode, bytes.TrimSpace(preview))
		if httpResp.StatusCode == http.StatusForbidden || httpResp.StatusCode == http.StatusUnauthorized {
			return nil, fmt.Errorf("%w: %w", errHookBrokerRefused, err)
		}
		return nil, err
	}
	var out BrokeredHookResponse
	if err := json.NewDecoder(io.LimitReader(httpResp.Body, 1<<20)).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode broker response: %w", err)
	}
	return &out, nil
}
