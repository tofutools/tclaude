package codexappserver

import "encoding/json"

const (
	MinimumCodexVersion = "0.147.0"
	MaximumCodexVersion = "0.148.0"

	MethodInitialize            = "initialize"
	MethodInitialized           = "initialized"
	MethodThreadLoadedList      = "thread/loaded/list"
	MethodThreadRead            = "thread/read"
	MethodThreadFork            = "thread/fork"
	MethodThreadNameSet         = "thread/name/set"
	MethodThreadCompactStart    = "thread/compact/start"
	MethodTurnStart             = "turn/start"
	MethodTurnSteer             = "turn/steer"
	MethodTurnInterrupt         = "turn/interrupt"
	MethodAccountRateLimitsRead = "account/rateLimits/read"

	NotificationThreadStatusChanged      = "thread/status/changed"
	NotificationThreadTokenUsageUpdated  = "thread/tokenUsage/updated"
	NotificationAccountRateLimitsUpdated = "account/rateLimits/updated"
	NotificationTurnStarted              = "turn/started"
	NotificationTurnCompleted            = "turn/completed"
	NotificationItemStarted              = "item/started"
	NotificationItemCompleted            = "item/completed"

	MethodCommandApproval     = "item/commandExecution/requestApproval"
	MethodFileChangeApproval  = "item/fileChange/requestApproval"
	MethodPermissionsApproval = "item/permissions/requestApproval"
	MethodRequestUserInput    = "item/tool/requestUserInput"
)

// StableMethods returns the capability surface validated against Codex 0.147.0.
func StableMethods() []string {
	return []string{
		MethodThreadLoadedList, MethodThreadRead, MethodThreadFork, MethodThreadNameSet,
		MethodThreadCompactStart, MethodTurnStart, MethodTurnSteer,
		MethodTurnInterrupt, MethodAccountRateLimitsRead,
	}
}

type ClientInfo struct {
	Name    string  `json:"name"`
	Title   *string `json:"title,omitempty"`
	Version string  `json:"version"`
}

type InitializeParams struct {
	ClientInfo ClientInfo `json:"clientInfo"`
	// Capabilities is intentionally omitted. In particular M1 never sends
	// experimentalApi, which would change the methods Codex may route here.
}

type InitializeResult struct {
	UserAgent      string `json:"userAgent"`
	CodexHome      string `json:"codexHome"`
	PlatformFamily string `json:"platformFamily"`
	PlatformOS     string `json:"platformOs"`
}

type Notification struct {
	Method string
	Params json.RawMessage
}

// ServerRequest is a decoded request initiated by app-server. M1 exposes it
// for diagnostics only; receipt is terminal and the client never replies.
type ServerRequest struct {
	ID     json.RawMessage
	Method string
	Params json.RawMessage
}

func (r ServerRequest) IsInteractionRequest() bool {
	switch r.Method {
	case MethodCommandApproval, MethodFileChangeApproval,
		MethodPermissionsApproval, MethodRequestUserInput:
		return true
	default:
		return false
	}
}

type ThreadLoadedListParams struct {
	Cursor *string `json:"cursor,omitempty"`
	Limit  *uint32 `json:"limit,omitempty"`
}

type ThreadLoadedListResult struct {
	Data       []string `json:"data"`
	NextCursor *string  `json:"nextCursor,omitempty"`
}

type ThreadReadParams struct {
	ThreadID     string `json:"threadId"`
	IncludeTurns bool   `json:"includeTurns,omitempty"`
}

type ThreadReadResult struct {
	Thread Thread `json:"thread"`
}

type ThreadForkParams struct {
	ThreadID   string  `json:"threadId"`
	Cwd        *string `json:"cwd,omitempty"`
	LastTurnID *string `json:"lastTurnId,omitempty"`
}

type ThreadForkResult struct {
	Thread Thread `json:"thread"`
}

// Thread is the small control projection used by M1. Status, items, and
// additive schema fields stay raw because policy belongs above this codec.
type Thread struct {
	ID         string          `json:"id"`
	Status     json.RawMessage `json:"status"`
	Turns      []Turn          `json:"turns"`
	Name       *string         `json:"name,omitempty"`
	Preview    string          `json:"preview,omitempty"`
	CLIVersion string          `json:"cliVersion,omitempty"`
}

type Turn struct {
	ID     string            `json:"id"`
	Status string            `json:"status"`
	Items  []json.RawMessage `json:"items"`
}

type ThreadNameSetParams struct {
	ThreadID string `json:"threadId"`
	Name     string `json:"name"`
}

type ThreadCompactStartParams struct {
	ThreadID string `json:"threadId"`
}

// UserInput is the stable M1 text-input projection. Other input variants can
// be added when a consuming ticket needs them.
type UserInput struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func TextInput(text string) UserInput { return UserInput{Type: "text", Text: text} }

type TurnStartParams struct {
	ThreadID            string      `json:"threadId"`
	Input               []UserInput `json:"input"`
	ClientUserMessageID *string     `json:"clientUserMessageId,omitempty"`
}

type TurnStartResult struct {
	Turn Turn `json:"turn"`
}

type TurnSteerParams struct {
	ThreadID            string      `json:"threadId"`
	ExpectedTurnID      string      `json:"expectedTurnId"`
	Input               []UserInput `json:"input"`
	ClientUserMessageID *string     `json:"clientUserMessageId,omitempty"`
}

type TurnSteerResult struct {
	TurnID string `json:"turnId"`
}

type TurnInterruptParams struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
}

type ThreadStatus struct {
	Type        string   `json:"type"`
	ActiveFlags []string `json:"activeFlags,omitempty"`
}

type ThreadStatusChangedNotification struct {
	ThreadID string       `json:"threadId"`
	Status   ThreadStatus `json:"status"`
}

type TokenUsageBreakdown struct {
	InputTokens           int64 `json:"inputTokens"`
	CachedInputTokens     int64 `json:"cachedInputTokens"`
	OutputTokens          int64 `json:"outputTokens"`
	ReasoningOutputTokens int64 `json:"reasoningOutputTokens"`
	TotalTokens           int64 `json:"totalTokens"`
}

type ThreadTokenUsage struct {
	Total              TokenUsageBreakdown `json:"total"`
	Last               TokenUsageBreakdown `json:"last"`
	ModelContextWindow *int64              `json:"modelContextWindow,omitempty"`
}

type ThreadTokenUsageUpdatedNotification struct {
	ThreadID   string           `json:"threadId"`
	TurnID     string           `json:"turnId"`
	TokenUsage ThreadTokenUsage `json:"tokenUsage"`
}

type RateLimitWindow struct {
	UsedPercent        int64  `json:"usedPercent"`
	WindowDurationMins *int64 `json:"windowDurationMins,omitempty"`
	ResetsAt           *int64 `json:"resetsAt,omitempty"`
}

type RateLimitSnapshot struct {
	LimitID   *string          `json:"limitId,omitempty"`
	LimitName *string          `json:"limitName,omitempty"`
	PlanType  *string          `json:"planType,omitempty"`
	Primary   *RateLimitWindow `json:"primary,omitempty"`
	Secondary *RateLimitWindow `json:"secondary,omitempty"`
}

type AccountRateLimitsReadResult struct {
	RateLimits          RateLimitSnapshot            `json:"rateLimits"`
	RateLimitsByLimitID map[string]RateLimitSnapshot `json:"rateLimitsByLimitId,omitempty"`
}

type AccountRateLimitsUpdatedNotification struct {
	RateLimits RateLimitSnapshot `json:"rateLimits"`
}

type ThreadScopedNotification struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId,omitempty"`
}
