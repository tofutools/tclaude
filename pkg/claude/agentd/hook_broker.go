package agentd

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// --- /v1/whoami/hook ---
//
// The agentd side of TCL-754's hook broker. A `tclaude-layer` agent runs
// behind a mount namespace that hides ~/.tclaude/data, so its hook
// callbacks cannot write the conversation database the way every other
// launch does. Instead they POST the parsed hook event here and the
// daemon — which is on the host, holds the real database, the real hook
// lock, the operator's notification config and the tmux socket — applies
// it on their behalf.
//
// Three properties make this safe to leave ungated by a permission slug:
//
//  1. The caller's session id only selects a candidate row. The daemon applies
//     the event only after the live pane's launch generation and host PID
//     ancestry prove that candidate owns the Unix-socket caller.
//  2. The effect is exactly what the same caller would have achieved by
//     writing the database directly, which is what every harness-builtin
//     agent does unmediated today. Brokering removes a capability (reaching
//     the database) rather than adding one.
//  3. An agent applying its own hook events is infrastructure, in the same
//     class as /v1/whoami itself.
//
// What it does add is a path from inside a sandbox to the daemon's pane
// injection machinery, since ApplyHook can inject `/rename` after a
// /clear. That is handled explicitly below.

// hookBrokerApplyTimeout bounds how long one brokered event may occupy a
// daemon goroutine. Deliberately under the client's own 20s give-up, so
// the daemon frees the goroutine while the client is still listening.
const hookBrokerApplyTimeout = 15 * time.Second

const hookBrokerAckTTL = time.Minute

type pendingHookAck struct {
	sessionID string
	commit    func()
	release   func()
	timer     *time.Timer
}

var hookAckRegistry = struct {
	sync.Mutex
	pending map[string]*pendingHookAck
}{pending: make(map[string]*pendingHookAck)}

func registerHookAck(sessionID string, commit, release func()) (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	token := hex.EncodeToString(raw[:])
	entry := &pendingHookAck{sessionID: sessionID, commit: commit, release: release}
	// Publish while holding the same lock the expiry callback takes. If the
	// process is suspended for longer than the TTL immediately after starting
	// the timer, the callback must wait until the entry is visible rather than
	// observe no entry and leave a subsequently published lock owner immortal.
	hookAckRegistry.Lock()
	entry.timer = time.AfterFunc(hookBrokerAckTTL, func() {
		var release func()
		hookAckRegistry.Lock()
		if hookAckRegistry.pending[token] == entry {
			delete(hookAckRegistry.pending, token)
			release = entry.release
		}
		hookAckRegistry.Unlock()
		if release != nil {
			release()
		}
	})
	hookAckRegistry.pending[token] = entry
	hookAckRegistry.Unlock()
	return token, nil
}

func resolveHookAck(sessionID, token string, delivered bool) bool {
	hookAckRegistry.Lock()
	entry := hookAckRegistry.pending[token]
	if entry == nil || entry.sessionID != sessionID {
		hookAckRegistry.Unlock()
		return false
	}
	delete(hookAckRegistry.pending, token)
	_ = entry.timer.Stop()
	hookAckRegistry.Unlock()
	defer func() {
		if entry.release != nil {
			entry.release()
		}
	}()
	if delivered && entry.commit != nil {
		entry.commit()
	}
	return true
}

// safeBrokeredConvID rejects a conversation id that is not a single
// path-safe segment.
//
// The conv-id is caller-controlled and is not merely stored: later freshness
// and auto-name paths join it into a transcript pathname, and filepath.Join
// cleans ".." segments. On the direct path that resolves inside the agent's
// own sandbox and buys it nothing; brokered, the daemon resolves it on the
// host, which turns an unvalidated field into a host-side read of any
// uuid-shaped file the agent can name.
//
// The check is a path-segment rule rather than a uuid shape on purpose:
// conv-id formats differ per harness, and this closes the traversal
// without taking a bet on any of them.
func safeBrokeredConvID(convID string) bool {
	if convID == "" {
		return true // absent is fine; the hook path handles it
	}
	if convID == "." || convID == ".." || strings.Contains(convID, "..") {
		return false
	}
	return !strings.ContainsAny(convID, `/\`) && convID == strings.TrimSpace(convID)
}

// handleWhoamiHook applies one hook event on behalf of the calling agent.
func handleWhoamiHook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
		return
	}
	const endpoint = "/v1/whoami/hook"

	p := peerFromContext(r.Context())
	switch classify(p) {
	case classAgent, classAgentUnknown, classUnconfirmed:
		// Both are fine here, and classAgentUnknown deliberately so: a
		// brokered SessionStart is often the event that first establishes
		// the conv-id. classUnconfirmed is also provisional here: a
		// version-named Claude process has no legacy harness-name match, but
		// the generation-bound pane proof below can still authenticate it.
		// No event is applied unless that proof or legacy identity succeeds.
	case classUnidentified:
		if checkBrokerRate(endpoint, brokerPreIdentityKey, brokerPreIdentityRatePerSecond).Reject {
			writeError(w, http.StatusTooManyRequests, "rate", "too many unplaceable requests")
			return
		}
		writeUnidentified(w)
		return
	default:
		writeUnconfirmed(w, r)
		return
	}
	if p.PID == 0 {
		writeUnidentified(w)
		return
	}

	// Resolve the ordinary candidate before parsing, but do not charge its
	// per-agent bucket yet: a reused pid can make this row provisional until
	// the request claim is checked against the live pane below. The shared,
	// high-capacity guard bounds pre-proof body parsing; the final row is charged
	// exactly once after identity settles.
	row, harnessPID := hookSessionRowForPID(p.PID)
	preProofKey := brokerPreIdentityKey
	if row != nil {
		preProofKey = brokerPreIdentityKeyForRow(row.ID)
	}
	if checkBrokerRate(endpoint, preProofKey, brokerPreIdentityRatePerSecond).Reject {
		if row == nil {
			brokerRefusals.recordUnplaceable("hook: caller could not be placed")
		}
		writeError(w, http.StatusTooManyRequests, "rate", "too many requests before identity verification")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, brokerMaxBody+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "body", "could not read request body")
		return
	}
	if len(body) > brokerMaxBody {
		resolvedID := ""
		if row != nil {
			resolvedID = row.ID
		}
		logBrokerBodyOverCap(endpoint, resolvedID, len(body))
		writeError(w, http.StatusRequestEntityTooLarge, "body", "hook payload too large")
		return
	}
	var req session.BrokeredHookRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "body", "malformed hook payload")
		return
	}
	claimed := strings.TrimSpace(req.ClaimedSessionID)
	proofKey := ""
	if row != nil {
		proofKey = row.ID
	}
	if checkBrokerProofRate(endpoint, brokerProofKeyForRow(proofKey)).Reject {
		writeError(w, http.StatusTooManyRequests, "rate", "too many identity proof attempts")
		return
	}
	provedRow, provedHarnessPID, layerClaim := proveTclaudeLayerCaller(p.PID, claimed)
	switch {
	case layerClaim && provedRow != nil:
		row, harnessPID = provedRow, provedHarnessPID
	case layerClaim:
		if row != nil {
			brokerRefusals.recordClaimMismatch(row.ID,
				"hook: claimed tclaude-layer session failed live-pane proof")
		} else {
			brokerRefusals.recordUnplaceable("hook: tclaude-layer caller failed live-pane proof")
		}
		writeError(w, http.StatusForbidden, "auth", "claimed tclaude-layer session does not own this caller")
		return
	case row == nil:
		brokerRefusals.recordUnplaceable("hook: caller could not be placed")
		writeError(w, http.StatusForbidden, "auth",
			"could not resolve a session row for this caller; refusing to apply its hook")
		return
	case isTclaudeLayerRow(row):
		brokerRefusals.recordUnplaceable("hook: tclaude-layer callback omitted its session claim")
		writeError(w, http.StatusForbidden, "auth",
			"tclaude-layer hook callback requires a proved session claim")
		return
	case claimed != "" && claimed != row.ID:
		slog.Warn("hook broker: rejecting event whose claimed session id disagrees with the resolved row",
			"caller_pid", p.PID, "claimed_session", claimed, "resolved_session", row.ID,
			"event", req.Input.HookEventName, "module", "hooks")
		// Identity DID resolve here, so the refusal is attributed to the
		// row the DAEMON concluded — never to the claimed one, which is
		// the caller's own string. See broker_refusals.go.
		brokerRefusals.recordClaimMismatch(row.ID, "hook: claimed session id disagrees with the resolved row")
		writeError(w, http.StatusForbidden, "auth",
			"claimed session id does not match the session resolved for this caller")
		return
	}
	if checkBrokerRate(endpoint, row.ID, brokerRatePerSecond).Reject {
		writeError(w, http.StatusTooManyRequests, "rate", "too many hook events")
		return
	}
	if req.AckToken != "" {
		if !resolveHookAck(row.ID, req.AckToken, !req.RelayFailed) {
			writeError(w, http.StatusConflict, "ack",
				"hook delivery acknowledgement is invalid, expired, or already consumed")
			return
		}
		writeJSON(w, http.StatusOK, session.BrokeredHookResponse{})
		return
	}

	if !safeBrokeredConvID(req.Input.ConvID) {
		writeError(w, http.StatusBadRequest, "body",
			"conversation id must be a single path-safe segment")
		return
	}

	sanitizeBrokeredHookInput(&req.Input, row.ConvID)

	amb := session.BrokeredHookAmbient(session.BrokeredHookContext{
		RowTmuxSession:    row.TmuxSession,
		RowCwd:            row.Cwd,
		HarnessPID:        harnessPID,
		ExitGeneration:    req.ExitGeneration,
		AutoCompactWindow: req.AutoCompactWindow,
	})

	// Bound the work this request may hold the daemon for. The hook path
	// takes a per-session file lock, and that lock file is NOT inside a
	// protected root, so a wrapped agent can hold it on purpose. Without a
	// deadline each such request would park a daemon goroutine that no
	// client disconnect can free — a sandbox could pin goroutines and file
	// descriptors until agentd stops serving everyone. The hook callback's
	// own client gives up at 20s; expiring first means we answer rather
	// than write into a round trip nobody is reading.
	ctx, cancel := context.WithTimeout(r.Context(), hookBrokerApplyTimeout)
	defer cancel()

	resp, err := session.PrepareHookEvent(ctx, req.Input, row.ID, amb)
	releaseOwned := true
	defer func() {
		if releaseOwned {
			resp.Release()
		}
	}()
	if err != nil {
		slog.Warn("hook broker: applying brokered hook event failed",
			"session", row.ID, "event", req.Input.HookEventName, "error", err, "module", "hooks")
		if ctx.Err() != nil {
			writeError(w, http.StatusServiceUnavailable, "hook",
				"timed out applying hook event; the session's hook lock is held")
			return
		}
		writeError(w, http.StatusInternalServerError, "hook", "failed to apply hook event")
		return
	}
	if req.Input.HookEventName == "SessionEnd" && req.Input.Reason != "clear" && req.Input.Reason != "resume" {
		revokeRouteHelperCredentials(row.ConvID, req.ExitGeneration)
	}
	var stdout bytes.Buffer
	if err := resp.Write(&stdout, req.Input.HookEventName); err != nil {
		writeError(w, http.StatusInternalServerError, "hook", "failed to encode hook response")
		return
	}
	if req.Input.HookEventName == "PermissionRequest" {
		// The event names the tool being gated, which is the whole condition
		// auto-permit keys off. Answering happens here, on the host, because a
		// sandboxed pane can reach neither tmux nor the database. See
		// auto_permit.go.
		maybeAnswerAutoPermit(row, req.Input.ToolName)
	}
	if req.Input.HookEventName == "UserPromptSubmit" {
		if harnessName, cwd, ok := brokeredAutoNameTarget(row.ID, req.Input.ConvID); ok {
			scheduleAutoName(req.Input.ConvID, harnessName, cwd, req.Input.Prompt)
		}
	}
	var ackToken string
	if resp.HasCommit() || resp.HasRelease() {
		ackToken, err = registerHookAck(row.ID, resp.Commit, resp.Release)
		if err != nil {
			slog.Warn("hook broker: could not register delivery acknowledgement",
				"session", row.ID, "event", req.Input.HookEventName, "error", err, "module", "hooks")
			writeError(w, http.StatusInternalServerError, "hook",
				"could not prepare hook delivery acknowledgement")
			return
		}
		// The registry now owns Release and runs it after a successful or
		// failed acknowledgement, or when the token expires.
		releaseOwned = false
	}
	writeJSON(w, http.StatusOK, session.BrokeredHookResponse{
		Stdout: stdout.String(), AckToken: ackToken,
	})
}

// brokeredAutoNameTarget re-resolves the row after dispatch and proves the
// submitted conversation is now the caller's own. Dispatch intentionally
// returns nil when it ignores a foreign-conversation hook; without this
// separate attribution check, that ignored payload could still name the
// foreign actor.
func brokeredAutoNameTarget(sessionID, submittedConvID string) (harnessName, cwd string, ok bool) {
	state, err := session.LoadSessionState(sessionID)
	if err != nil || state == nil || strings.TrimSpace(submittedConvID) == "" ||
		state.ConvID != submittedConvID {
		return "", "", false
	}
	return state.Harness, state.Cwd, true
}

// sanitizeBrokeredHookInput clamps the payload field that becomes a
// host-side capability when the daemon acts on it instead of the agent.
// (The other such field, ConvID, is rejected outright by
// safeBrokeredConvID rather than clamped — see there.)
//
// TranscriptPath is a caller-supplied filesystem path that the Codex
// telemetry projection opens and scans, and whose directory is stored as
// the conversation's project dir. On the direct path the reader is the
// agent's own hook process, so naming a file it could not already open
// buys it nothing. Brokered, the reader is the daemon on the HOST, and the
// same string becomes two distinct capabilities: read any host file whose
// basename looks like a rollout, and read a PEER AGENT's Codex transcript
// — the second being a cross-agent leak the direct path never had.
//
// ownConvID is the conv-id of the session the daemon resolved for this
// caller, so the path is accepted only when it is that session's own
// rollout, really resolving inside the Codex sessions tree. See
// harness.IsCodexRolloutPathForConv for the three checks.
//
// Cost of the ownership check: a session whose row has not learned its
// conv-id yet (its very first event) drops the path and loses that one
// event's rollout projection. It self-heals on the next event, and the
// projection's by-id fallback covers the same ground, so this is a
// rounding error against the leak it closes.
//
// No size cap is imposed here: the projection scans the rollout line by
// line (tail-first for live .jsonl), never slurping it into memory, so
// file size bounds nothing worth bounding.
//
// Deliberately NOT clamped: the free-text fields (Prompt,
// LastAssistantMessage, Message, ErrorMessage, ToolInput, ToolResponse).
// They are stored and rendered, never executed or resolved, and the direct
// path already carries the same values into the same code. Diverging the
// two paths here would buy nothing and cost the shared-implementation
// property the broker is built on.
//
// The pane-injection sink is NOT handled here, because it is already
// handled at the only place it can be handled correctly — see
// hook_broker_injection_test.go for the standing proof.
func sanitizeBrokeredHookInput(in *session.HookCallbackInput, ownConvID string) {
	if in.TranscriptPath == "" {
		return
	}
	home, err := os.UserHomeDir()
	if err != nil || !harness.IsCodexRolloutPathForConv(home, ownConvID, in.TranscriptPath) {
		slog.Debug("hook broker: dropping a transcript path that is not this session's own rollout",
			"path", in.TranscriptPath, "conv_id", ownConvID, "module", "hooks")
		in.TranscriptPath = ""
	}
}
