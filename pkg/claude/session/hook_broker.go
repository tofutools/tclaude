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
// The marker is the load-bearing signal. The probe below it is a backstop
// only, and it is deliberately not the primary: inside the sandbox
// `~/.tclaude/data` is an EMPTY WRITABLE tmpfs (the mount plan hides a
// protected root with --tmpfs), so db.Open() happily creates and migrates
// a complete phantom database there. Nothing fails; reads just come back
// empty and writes evaporate when the pane exits. That is why the dual
// path cannot be "try the database, fall back on error" — there is no
// error to catch — and why the absence of the database file, not a failed
// write, is what the probe looks at.
//
// The probe's own blind spot, stated plainly: any in-sandbox tclaude
// command that opens the database creates the phantom file, after which
// the probe reports "reachable" for the rest of the pane's life. It
// therefore covers only the narrow case of a launch that reached the
// sandbox without the marker, and it covers it only until something else
// opens the database. TCL-758 (protected roots remounted read-only) makes
// the hidden path reject writes outright and retires the guesswork.
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
	if _, inTaskMode := taskSignalPath(); inTaskMode {
		if os.Getenv(HookBrokerEnvVar) == HookBrokerAgentd {
			slog.Warn("hook broker: `tclaude task run` inside a tclaude-layer pane is not supported; "+
				"applying this hook directly, which cannot reach the real database",
				"module", "hooks")
		}
		return false
	}
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
}

// BrokeredHookResponse carries back whatever the hook would have written
// to its own stdout — today only the PreCompact gate's decision document.
type BrokeredHookResponse struct {
	Stdout string `json:"stdout,omitempty"`
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
	req := BrokeredHookRequest{
		Input:             input,
		ClaimedSessionID:  os.Getenv("TCLAUDE_SESSION_ID"),
		ExitGeneration:    os.Getenv("TCLAUDE_EXIT_GENERATION"),
		AutoCompactWindow: os.Getenv(harness.AutoCompactWindowEnvVar),
	}
	body, err := json.Marshal(req)
	if err != nil {
		slog.Warn("hook broker: failed to encode event", "error", err, "module", "hooks")
		return nil
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
		if errors.Is(err, errHookBrokerRefused) {
			slog.Error("hook broker: agentd refused this session's hook events; "+
				"the agent's status, ledgers and directory tracking will not update",
				"event", input.HookEventName, "error", err, "module", "hooks")
			return nil
		}
		slog.Warn("hook broker: agentd unreachable, dropping event",
			"event", input.HookEventName, "error", err, "module", "hooks")
		return nil
	}
	if resp.Stdout != "" {
		if _, err := io.WriteString(stdout, resp.Stdout); err != nil {
			slog.Warn("hook broker: failed to relay hook decision to stdout",
				"error", err, "module", "hooks")
		}
	}
	return nil
}

// hookBrokerBodyBudget is the client's own ceiling, kept below the
// daemon's so a trim happens here rather than as a rejection there.
const hookBrokerBodyBudget = (1 << 20) - 4096

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
