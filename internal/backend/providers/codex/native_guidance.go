package codex

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

type nativeHookInput struct {
	SessionID     string `json:"session_id"`
	ThreadID      string `json:"thread_id"`
	HookEventName string `json:"hook_event_name"`
}

type codexNativeNormalizer struct {
	forkReceipt string
	mu          sync.Mutex
	nativeID    string
}

func newCodexNativeNormalizer(nativeID string) *codexNativeNormalizer {
	return &codexNativeNormalizer{nativeID: nativeID}
}

func (n *codexNativeNormalizer) Matches(value string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.refreshForkIdentity()
	return n.nativeID != "" && value == n.nativeID
}

func (n *codexNativeNormalizer) SetForkReceipt(path string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.forkReceipt, n.nativeID = path, ""
}
func (n *codexNativeNormalizer) refreshForkIdentity() {
	if n.forkReceipt != "" && n.nativeID == "" {
		n.nativeID, _ = readForkReceipt(n.forkReceipt)
	}
}

func (n *codexNativeNormalizer) Set(nativeID string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.nativeID = nativeID
}

func (n *codexNativeNormalizer) Normalize(raw ports.RawNativeCallback) (ports.NormalizedNativeEvent, string, error) {
	var input nativeHookInput
	if len(raw.Body) == 0 || len(raw.Body) > 1<<20 || !json.Valid(raw.Body) || json.Unmarshal(raw.Body, &input) != nil {
		return ports.NormalizedNativeEvent{}, "", fmt.Errorf("invalid Codex native hook payload")
	}
	correlation := input.SessionID
	if correlation == "" {
		correlation = input.ThreadID
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.refreshForkIdentity()
	if n.forkReceipt == "" && n.nativeID == "" && input.HookEventName == "SessionStart" && correlation != "" {
		n.nativeID = correlation
	}
	if correlation == "" || correlation != n.nativeID {
		return ports.NormalizedNativeEvent{}, "", fmt.Errorf("codex native hook session does not match execution")
	}
	kind := ""
	switch input.HookEventName {
	case "SessionStart":
		kind = "session_start"
	case "UserPromptSubmit":
		kind = "user_prompt"
	default:
		return ports.NormalizedNativeEvent{}, "", fmt.Errorf("unsupported Codex native hook event %q", input.HookEventName)
	}
	digest := sha256.Sum256(append([]byte(fmt.Sprintf("%d\x00", raw.ReceivedAt.UnixNano())), raw.Body...))
	return ports.NormalizedNativeEvent{
		EventID: "codex:" + hex.EncodeToString(digest[:]), Kind: kind,
		OccurredAt: raw.ReceivedAt, ObservedAt: raw.ReceivedAt,
		NativeCorrelation: correlation, Payload: append(json.RawMessage(nil), raw.Body...),
		Timing: model.StandingOrderSameContinuation,
	}, input.HookEventName, nil
}

func encodeCodexGuidance(nativeKind, guidance string) ([]byte, error) {
	if strings.TrimSpace(guidance) == "" {
		return nil, fmt.Errorf("codex native guidance is empty")
	}
	return json.Marshal(map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName": nativeKind, "additionalContext": guidance,
	}})
}
