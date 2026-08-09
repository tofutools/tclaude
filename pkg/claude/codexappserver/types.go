package codexappserver

import "encoding/json"

const (
	MinimumCodexVersion = "0.147.0"
	MaximumCodexVersion = "0.148.0"

	MethodInitialize         = "initialize"
	MethodInitialized        = "initialized"
	MethodThreadLoadedList   = "thread/loaded/list"
	MethodThreadRead         = "thread/read"
	MethodThreadNameSet      = "thread/name/set"
	MethodThreadCompactStart = "thread/compact/start"
	MethodTurnStart          = "turn/start"
	MethodTurnSteer          = "turn/steer"
	MethodTurnInterrupt      = "turn/interrupt"

	MethodCommandApproval     = "item/commandExecution/requestApproval"
	MethodFileChangeApproval  = "item/fileChange/requestApproval"
	MethodPermissionsApproval = "item/permissions/requestApproval"
	MethodRequestUserInput    = "item/tool/requestUserInput"
)

// StableMethods returns the capability surface validated against Codex 0.147.0.
func StableMethods() []string {
	return []string{
		MethodThreadLoadedList, MethodThreadRead, MethodThreadNameSet,
		MethodThreadCompactStart, MethodTurnStart, MethodTurnSteer,
		MethodTurnInterrupt,
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
