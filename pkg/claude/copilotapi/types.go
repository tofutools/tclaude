package copilotapi

import (
	"encoding/json"
	"time"
)

// Notification method names pushed by the server.
const (
	MethodSessionEvent     = "session.event"
	MethodSessionLifecycle = "session.lifecycle"
)

// Lifecycle event types carried in [LifecycleNotification.Type].
const (
	LifecycleSessionCreated    = "session.created"
	LifecycleSessionDeleted    = "session.deleted"
	LifecycleSessionUpdated    = "session.updated"
	LifecycleSessionForeground = "session.foreground"
	LifecycleSessionBackground = "session.background"
)

// ConnectParams are the arguments to the `connect` handshake.
type ConnectParams struct {
	// Token is required only when the server was started with
	// COPILOT_CONNECTION_TOKEN, which the TUI+server path never does.
	Token string `json:"token,omitempty"`
}

// ConnectResult is the handshake reply.
type ConnectResult struct {
	OK              bool   `json:"ok"`
	ProtocolVersion int    `json:"protocolVersion"`
	Version         string `json:"version"`
}

// PingParams are the arguments to `ping`.
type PingParams struct {
	Message string `json:"message,omitempty"`
}

// PingResult is the `ping` reply. Message is the request's Message prefixed
// with "pong: ".
type PingResult struct {
	Message         string `json:"message"`
	Timestamp       string `json:"timestamp"`
	ProtocolVersion int    `json:"protocolVersion"`
}

// CreateSessionParams are the arguments to `session.create`.
//
// The real method accepts well over eighty fields. These are the ones needed
// to bring up a session that renders in the TUI and streams events; add more
// only alongside a consumer that needs them.
type CreateSessionParams struct {
	// SessionID is chosen by the caller. The server echoes it back and
	// rejects a reply that disagrees.
	SessionID string `json:"sessionId"`
	// WorkingDirectory is the session's cwd. Copilot resolves workspace and
	// repository context from it.
	WorkingDirectory string `json:"workingDirectory,omitempty"`
	// ClientName identifies the driving client in Copilot's own telemetry.
	ClientName string `json:"clientName,omitempty"`
	// Streaming enables incremental assistant deltas on the event stream.
	Streaming bool `json:"streaming,omitempty"`
	// Model selects a model; empty uses the session default.
	Model string `json:"model,omitempty"`
}

// SessionInfo is returned by both `session.create` and
// `session.getForeground`.
type SessionInfo struct {
	SessionID     string          `json:"sessionId"`
	WorkspacePath string          `json:"workspacePath"`
	Capabilities  json.RawMessage `json:"capabilities,omitempty"`
}

// SetForegroundResult is the `session.setForeground` reply.
//
// Failure arrives as a successful JSON-RPC response carrying Success false,
// not as an error object, so the result must be inspected.
type SetForegroundResult struct {
	Success bool   `json:"success"`
	Error   string `json:"error,omitempty"`
}

// Delivery modes for [SendParams.Mode].
//
// Measured against Copilot CLI 1.0.78, because the names alone suggest a
// stronger difference than the one that exists:
//
//   - SendModeEnqueue (the default) appends to the session's queue. It shows up
//     in `session.queue.pendingItems` under `items`, and runs after the turn in
//     flight and after anything already queued — including what the human typed
//     into the pane.
//   - SendModeImmediate does NOT interrupt the running turn. It lands in the
//     same snapshot's separate `steeringMessages` lane and runs before the
//     queued items once the current turn unwinds, so it is a queue-jump rather
//     than an interjection. Interrupting is `session.abort`'s job, not this
//     one's.
//
// Agent-to-agent delivery therefore wants the default: a message that overtook
// the human's own queued input would reorder a conversation nobody asked to
// reorder.
const (
	SendModeEnqueue   = "enqueue"
	SendModeImmediate = "immediate"
)

// SendParams are the arguments to `session.send`.
type SendParams struct {
	SessionID string `json:"sessionId"`
	// Prompt is the user message text.
	Prompt string `json:"prompt"`
	// DisplayPrompt, when set, is shown in the TUI timeline instead of
	// Prompt. Useful for hiding coordination scaffolding from the human.
	DisplayPrompt string `json:"displayPrompt,omitempty"`
	// Mode selects delivery. Empty means the server default, "enqueue".
	Mode string `json:"mode,omitempty"`
}

// SendResult is the `session.send` reply.
type SendResult struct {
	MessageID string `json:"messageId"`
}

// CompactTriggerManual attributes a compaction to an explicit request, which
// is what every compaction tclaude drives is. The server records it on the
// persisted `session.compaction_start` / `session.compaction_complete` events;
// omitting it leaves the compaction attributed to nobody.
const CompactTriggerManual = "manual"

// CompactParams are the arguments to `session.history.compact`.
type CompactParams struct {
	SessionID string `json:"sessionId"`
	// CustomInstructions focuses the summary. Capped at 4000 characters by the
	// server.
	CustomInstructions string `json:"customInstructions,omitempty"`
	// Trigger is attribution metadata; see [CompactTriggerManual].
	Trigger string `json:"trigger,omitempty"`
}

// CompactResult is the `session.history.compact` reply.
type CompactResult struct {
	Success bool `json:"success"`
	// TokensRemoved is the net change and CAN BE NEGATIVE: compaction replaces
	// history with a generated summary, and on a short session the summary is
	// larger than what it replaced. Do not present it as an unsigned saving.
	TokensRemoved   int `json:"tokensRemoved"`
	MessagesRemoved int `json:"messagesRemoved"`
	// SummaryContent is the generated summary that replaced the history. It can
	// run to thousands of characters, so it is worth logging deliberately
	// rather than by including the whole result.
	SummaryContent string                `json:"summaryContent,omitempty"`
	ContextWindow  *CompactContextWindow `json:"contextWindow,omitempty"`
}

// CompactContextWindow is the post-compaction context breakdown returned
// alongside a compaction result.
type CompactContextWindow struct {
	TokenLimit           int `json:"tokenLimit"`
	CurrentTokens        int `json:"currentTokens"`
	MessagesLength       int `json:"messagesLength"`
	SystemTokens         int `json:"systemTokens"`
	ConversationTokens   int `json:"conversationTokens"`
	ToolDefinitionTokens int `json:"toolDefinitionsTokens"`
}

// ContextInfoParams are the arguments to `session.metadata.contextInfo`.
// Both limits are required; pass zero to accept the runtime defaults.
type ContextInfoParams struct {
	SessionID        string `json:"sessionId"`
	PromptTokenLimit int    `json:"promptTokenLimit"`
	OutputTokenLimit int    `json:"outputTokenLimit"`
	SelectedModel    string `json:"selectedModel,omitempty"`
}

// ContextInfo is the token breakdown for a session's context window.
type ContextInfo struct {
	ModelName            string `json:"modelName"`
	SystemTokens         int    `json:"systemTokens"`
	ConversationTokens   int    `json:"conversationTokens"`
	ToolDefinitionTokens int    `json:"toolDefinitionsTokens"`
	MCPToolsTokens       int    `json:"mcpToolsTokens"`
	TotalTokens          int    `json:"totalTokens"`
	PromptTokenLimit     int    `json:"promptTokenLimit"`
	// CompactionThreshold is the token count at which background compaction
	// starts.
	CompactionThreshold int `json:"compactionThreshold"`
	// Limit is PromptTokenLimit plus the model's full output token limit.
	Limit        int `json:"limit"`
	BufferTokens int `json:"bufferTokens"`
}

// contextInfoResult is the raw `session.metadata.contextInfo` envelope. The
// inner field is null until the session has cached a system prompt and tool
// metadata, which it has not immediately after creation.
type contextInfoResult struct {
	ContextInfo *ContextInfo `json:"contextInfo"`
}

// UsageMetrics is the `session.usage.getMetrics` reply.
type UsageMetrics struct {
	TotalPremiumRequestCost float64                `json:"totalPremiumRequestCost"`
	TotalUserRequests       int                    `json:"totalUserRequests"`
	TotalNanoAIU            float64                `json:"totalNanoAiu,omitempty"`
	TotalAPIDurationMs      int64                  `json:"totalApiDurationMs"`
	SessionStartTime        string                 `json:"sessionStartTime"`
	CodeChanges             CodeChanges            `json:"codeChanges"`
	ModelMetrics            map[string]ModelMetric `json:"modelMetrics"`
	// TokenDetails breaks the session total down by token type, keyed by the
	// server's own type names.
	TokenDetails         map[string]TokenDetail `json:"tokenDetails,omitempty"`
	CurrentModel         string                 `json:"currentModel,omitempty"`
	LastCallInputTokens  int                    `json:"lastCallInputTokens"`
	LastCallOutputTokens int                    `json:"lastCallOutputTokens"`
}

// CodeChanges totals the edits a session has made.
type CodeChanges struct {
	LinesAdded         int      `json:"linesAdded"`
	LinesRemoved       int      `json:"linesRemoved"`
	FilesModifiedCount int      `json:"filesModifiedCount"`
	FilesModified      []string `json:"filesModified,omitempty"`
}

// ModelMetric is one model's slice of a session's usage.
//
// Token counts are nested under Usage and request counts under Requests. They
// are not flat fields on this object, which is easy to get wrong: a flattened
// struct still decodes without error and reports zero for everything.
type ModelMetric struct {
	Requests ModelRequests `json:"requests"`
	Usage    ModelUsage    `json:"usage"`
	// CacheExpiresAt is the latest known prompt-cache expiry for this model. A
	// time in the past means the observed cache has expired.
	CacheExpiresAt string                 `json:"cacheExpiresAt,omitempty"`
	TotalNanoAIU   float64                `json:"totalNanoAiu,omitempty"`
	TokenDetails   map[string]TokenDetail `json:"tokenDetails,omitempty"`
}

// ModelRequests counts and costs one model's API requests.
type ModelRequests struct {
	Count int `json:"count"`
	// Cost is the user-initiated premium request cost with its multiplier
	// already applied.
	Cost float64 `json:"cost"`
}

// ModelUsage is one model's token totals.
type ModelUsage struct {
	InputTokens      int `json:"inputTokens"`
	OutputTokens     int `json:"outputTokens"`
	CacheReadTokens  int `json:"cacheReadTokens"`
	CacheWriteTokens int `json:"cacheWriteTokens"`
	ReasoningTokens  int `json:"reasoningTokens,omitempty"`
}

// TokenDetail is an accumulated count for one token type.
type TokenDetail struct {
	TokenCount int `json:"tokenCount"`
}

// SetNameParams are the arguments to `session.name.set`. Name must be 1–100
// characters after trimming; the server rejects anything else.
type SetNameParams struct {
	SessionID string `json:"sessionId"`
	Name      string `json:"name"`
}

// Notification is a server push, delivered to every subscriber.
type Notification struct {
	// Method is [MethodSessionEvent] or [MethodSessionLifecycle]. Other
	// methods are delivered too rather than dropped, so a subscriber can
	// notice a contract that has grown.
	Method string
	// Params is the raw payload. Use [Notification.SessionEvent] or
	// [Notification.Lifecycle] to decode the two known shapes.
	Params json.RawMessage
}

// SessionEventNotification is the payload of a [MethodSessionEvent] push.
type SessionEventNotification struct {
	SessionID string       `json:"sessionId"`
	Event     SessionEvent `json:"event"`
}

// SessionEvent is one entry in a session's event stream. Copilot defines a
// large open set of event types (assistant.turn_start, session.idle,
// assistant.usage, …); Data is left raw so consumers decode only what they
// handle and unknown types stay forward-compatible.
//
// The envelope fields around Data are modelled, because they are common to
// every event type and a consumer cannot interpret an event without them.
type SessionEvent struct {
	Type string `json:"type"`
	ID   string `json:"id,omitempty"`
	// AgentID identifies the sub-agent instance that produced the event, and
	// is absent for the root agent. Without it a consumer cannot tell whether
	// a `session.idle` means the agent is done or merely one of its
	// sub-agents.
	AgentID string `json:"agentId,omitempty"`
	// ParentID links an event to the one it was produced under.
	ParentID string `json:"parentId,omitempty"`
	// Ephemeral marks an event that is not persisted to the event log; the
	// server itself filters these out when replaying history.
	Ephemeral bool            `json:"ephemeral,omitempty"`
	Timestamp string          `json:"timestamp,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"`
}

// LifecycleNotification is the payload of a [MethodSessionLifecycle] push.
type LifecycleNotification struct {
	Type      string             `json:"type"`
	SessionID string             `json:"sessionId"`
	Metadata  *LifecycleMetadata `json:"metadata,omitempty"`
}

// LifecycleMetadata accompanies every lifecycle type except session.deleted.
type LifecycleMetadata struct {
	StartTime    time.Time `json:"startTime"`
	ModifiedTime time.Time `json:"modifiedTime"`
	Summary      string    `json:"summary,omitempty"`
}

// SessionEvent decodes the notification as a [MethodSessionEvent] payload.
func (n Notification) SessionEvent() (SessionEventNotification, error) {
	var decoded SessionEventNotification
	err := json.Unmarshal(n.Params, &decoded)
	return decoded, err
}

// Lifecycle decodes the notification as a [MethodSessionLifecycle] payload.
func (n Notification) Lifecycle() (LifecycleNotification, error) {
	var decoded LifecycleNotification
	err := json.Unmarshal(n.Params, &decoded)
	return decoded, err
}
