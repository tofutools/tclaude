package harness

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
)

// Copilot's live runtime telemetry, derived from the DURABLE half of
// `<COPILOT_HOME>/session-state/<id>/events.jsonl`.
//
// What "durable" means here is not a judgement call. Copilot ships a formal
// `session-events.schema.json`, and every event definition in it carries an
// `ephemeral` flag documented as "when true, the event is transient and not
// persisted to the session event log on disk". Running the pinned 1.0.77
// binary credential-free (pkg/claude/harness/copilotfixture) confirms the flag
// is honoured: the log contains only non-ephemeral events.
//
// That single fact decides this whole file, because the three events a usage
// follower would most want are all ephemeral:
//
//   - `assistant.usage`   — per-call input/output/cache/reasoning tokens, cost
//   - `session.usage_info` — live context window (currentTokens + tokenLimit)
//   - `model.call_start`  — per-call model/provider
//
// None of them reach the disk, and TCL-976 established that hooks do not carry
// them either. Scraping the TUI and editing Copilot's SQLite are both out of
// scope by ticket. So the honest answer is that a file follower CANNOT produce
// a live per-turn token meter for Copilot, and this projection deliberately
// does not pretend otherwise: it reports exactly the fields the durable log
// actually carries, and reports nothing where the log is silent.
//
// The durable surface, verified against a real 1.0.77 run:
//
//	session.start            selectedModel, reasoningEffort, contextTier,
//	                         copilotVersion
//	session.resume           the same, restated for the appended lifetime
//	session.model_change     newModel, reasoningEffort, contextTier
//	user.message             one per user turn
//	assistant.turn_start     turnId (a per-lifetime counter, NOT monotonic
//	                         across a resume)
//	assistant.message        model, outputTokens — the ONE per-turn token
//	                         figure that is durable
//	session.compaction_start currentTokens, tokenLimit, and the
//	                         system/conversation/toolDefinitions split
//	session.compaction_complete  postCompactionTokens + the same split
//	session.truncation       tokenLimit and post-truncation message tokens
//	session.usage_checkpoint totalNanoAiu — "durable session usage checkpoint
//	                         for reconstructing aggregate accounting on resume"
//	session.shutdown         session-lifetime modelMetrics, totalNanoAiu, and
//	                         the context window occupancy at exit
//	session.error            errorType/statusCode/message (and a `stack` that
//	                         this code never retains — see below)
//
// Two consequences worth stating plainly:
//
//   - Context-window occupancy is only observable at a compaction, a
//     truncation, or a shutdown. Between those points the log says nothing,
//     so HasContext stays false and no context row is written rather than a
//     stale or invented one.
//   - Cost is reported in nano-AI units exactly as Copilot writes it. Copilot
//     documents an AI credit as $0.01 in a DIFFERENT structure (a quota
//     snapshot), and nowhere states that one AIU is one credit. Multiplying on
//     that guess would put a fabricated dollar figure on a billing surface, so
//     this stops at the unit Copilot actually emits and the USD conversion is
//     ticketed separately.

// copilotAbsPathRE matches a POSIX absolute path with at least two segments.
// It is used to scrub host paths out of anything derived from Copilot's error
// reporting before tclaude keeps it.
//
// Deliberately greedy about what counts as a path character and deliberately
// applied to whole messages: over-redacting an error string costs a reader a
// little context, while under-redacting writes an operator's directory layout
// into tclaude's database and from there into a dashboard.
var copilotAbsPathRE = regexp.MustCompile(`/[\w.@+-]+(?:/[\w.@+-]+)+`)

const copilotRedactedPath = "<path>"

// copilotErrorMessageLimit bounds a retained error message. Copilot's error
// messages are short; a provider that returns a whole HTML page should not be
// able to push a megabyte into a session row.
const copilotErrorMessageLimit = 512

// sanitizeCopilotText strips absolute host paths and bounds the length. Every
// string this package retains from an error path goes through it.
func sanitizeCopilotText(in string) string {
	out := copilotAbsPathRE.ReplaceAllString(strings.TrimSpace(in), copilotRedactedPath)
	if len(out) > copilotErrorMessageLimit {
		out = out[:copilotErrorMessageLimit] + "…"
	}
	return out
}

// CopilotContextTelemetry is one durable observation of the context window.
//
// Copilot reports the window as three addends plus their total, and the total
// is what it renders itself, so CurrentTokens is used as the occupancy rather
// than re-summing the parts (a future fourth addend would silently undercount
// a hand-rolled sum).
type CopilotContextTelemetry struct {
	CurrentTokens         int64 `json:"current_tokens,omitempty"`
	SystemTokens          int64 `json:"system_tokens,omitempty"`
	ConversationTokens    int64 `json:"conversation_tokens,omitempty"`
	ToolDefinitionsTokens int64 `json:"tool_definitions_tokens,omitempty"`
	// TokenLimit is the model's context window. It is NOT carried by
	// session.shutdown, so a session that ended without ever compacting or
	// truncating reports occupancy with no denominator.
	TokenLimit int64 `json:"token_limit,omitempty"`
	// Source names the durable event this observation came from, so a reader
	// can tell "at exit" from "at the last compaction".
	Source string `json:"source,omitempty"`
}

// Pct is the window occupancy percentage, or 0 when Copilot has not disclosed
// a limit. 0 therefore means "unknown", exactly as it does for the other
// harnesses' context snapshots.
func (c CopilotContextTelemetry) Pct() float64 {
	if c.TokenLimit <= 0 || c.CurrentTokens <= 0 {
		return 0
	}
	return float64(c.CurrentTokens) / float64(c.TokenLimit) * 100
}

// CopilotUsage is the session-lifetime token accounting Copilot writes into
// session.shutdown, summed across every model the session used.
//
// It is cumulative across resumes: a resumed session restores its counters, so
// the newest shutdown record is the whole session's total, not one lifetime's.
type CopilotUsage struct {
	InputTokens      int64 `json:"input_tokens,omitempty"`
	OutputTokens     int64 `json:"output_tokens,omitempty"`
	CacheReadTokens  int64 `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int64 `json:"cache_write_tokens,omitempty"`
	ReasoningTokens  int64 `json:"reasoning_tokens,omitempty"`
	Requests         int64 `json:"requests,omitempty"`
}

// CopilotErrorObservation is the retainable part of a session.error event.
//
// `data.stack` is deliberately absent from this struct rather than merely
// unused: TCL-976 recorded that Copilot's stacks embed absolute host paths,
// and a field that does not exist cannot be logged, checkpointed or rendered
// by a later change that forgets why.
type CopilotErrorObservation struct {
	ErrorType  string `json:"error_type,omitempty"`
	ErrorCode  string `json:"error_code,omitempty"`
	StatusCode int    `json:"status_code,omitempty"`
	// Message is path-scrubbed and length-bounded by sanitizeCopilotText.
	Message string `json:"message,omitempty"`
}

// CopilotRuntimeSnapshot is one projection of a session's durable event log.
type CopilotRuntimeSnapshot struct {
	// Model is the model in force at the end of the scanned prefix.
	Model string
	// Effort is Copilot's reasoningEffort, when it discloses one.
	Effort string
	// ContextTier is "default" or "long_context" when a tiered model is
	// selected, and "" when Copilot reports no tier (it writes an explicit
	// null for non-tiered models).
	ContextTier string
	// CopilotVersion is the CLI version that opened the session.
	CopilotVersion string

	// UserMessages and AssistantMessages count durable turn records.
	UserMessages      int
	AssistantMessages int
	// AssistantOutputTokens sums assistant.message.outputTokens. This is the
	// only token figure that advances DURING a session rather than at exit.
	AssistantOutputTokens int64

	// Lifetimes counts session.start plus session.resume records — how many
	// times this one log has been appended to by a fresh CLI process.
	Lifetimes int

	Context    CopilotContextTelemetry
	HasContext bool

	// Usage is nil until a session.shutdown has been observed.
	Usage *CopilotUsage

	// NanoAIU is Copilot's own cumulative session cost unit. HasNanoAIU
	// distinguishes "Copilot reported zero" from "Copilot said nothing":
	// a BYOK/mock provider legitimately reports 0.
	NanoAIU         float64
	HasNanoAIU      bool
	PremiumRequests float64

	// LastError is the most recent sanitized session.error.
	LastError *CopilotErrorObservation
}

// maxCopilotEventLineBytes bounds one buffered record. It matches the
// conversation store's limit for the same reason: Copilot writes its ~26 kB
// system prompt and arbitrarily large tool results as single lines, so a small
// bound would drop real records, and an unbounded one would let a single line
// decide tclaude's memory usage.
const maxCopilotEventLineBytes = 8 << 20

// copilotTelemetryPrefilters are the raw-line needles that make a record worth
// decoding. Everything else — above all the per-turn `system.message` carrying
// the full system prompt, and `tool.execution_complete` carrying tool output —
// is skipped without allocating a decoder.
//
// A false positive (the needle appearing inside prompt or tool text) costs one
// wasted decode that the type switch discards. A false NEGATIVE would need
// Copilot to escape plain ASCII in its type discriminator, which its
// machine-written log does not do.
var copilotTelemetryPrefilters = [][]byte{
	[]byte("session.start"),
	[]byte("session.resume"),
	[]byte("session.model_change"),
	[]byte("session.usage_checkpoint"),
	[]byte("session.compaction_start"),
	[]byte("session.compaction_complete"),
	[]byte("session.truncation"),
	[]byte("session.shutdown"),
	[]byte("session.error"),
	[]byte("user.message"),
	[]byte("assistant.message"),
}

func copilotTelemetryLineOfInterest(line []byte) bool {
	for _, needle := range copilotTelemetryPrefilters {
		if bytes.Contains(line, needle) {
			return true
		}
	}
	return false
}

// copilotTelemetryEvent is the minimal decode of one durable event.
//
// Every numeric field is a pointer or a plain zero-valued scalar chosen so
// that an ABSENT key and a written zero stay distinguishable where that
// difference matters (cost), and collapse where it does not (token counts).
// Unknown keys are ignored: a future CLI adding fields must not stop the scan.
type copilotTelemetryEvent struct {
	Type string `json:"type"`
	Data struct {
		// session.start / session.resume
		SelectedModel  string          `json:"selectedModel"`
		CopilotVersion string          `json:"copilotVersion"`
		ContextTier    *string         `json:"contextTier"`
		ReasoningEfrt  json.RawMessage `json:"reasoningEffort"`

		// session.model_change
		NewModel string `json:"newModel"`

		// session.usage_checkpoint / session.shutdown
		TotalNanoAiu         *float64 `json:"totalNanoAiu"`
		TotalPremiumRequests *float64 `json:"totalPremiumRequests"`

		// assistant.message
		Model        string `json:"model"`
		OutputTokens int64  `json:"outputTokens"`

		// session.compaction_start / session.compaction_complete /
		// session.shutdown share this context-window vocabulary.
		CurrentTokens         int64 `json:"currentTokens"`
		SystemTokens          int64 `json:"systemTokens"`
		ConversationTokens    int64 `json:"conversationTokens"`
		ToolDefinitionsTokens int64 `json:"toolDefinitionsTokens"`
		TokenLimit            int64 `json:"tokenLimit"`
		PostCompactionTokens  int64 `json:"postCompactionTokens"`
		Success               *bool `json:"success"`

		// session.truncation
		PostTruncationTokens int64 `json:"postTruncationTokensInMessages"`

		// session.shutdown
		CurrentModel string                          `json:"currentModel"`
		ModelMetrics map[string]copilotShutdownModel `json:"modelMetrics"`

		// session.error — `stack` is intentionally NOT decoded.
		ErrorType  string `json:"errorType"`
		ErrorCode  string `json:"errorCode"`
		StatusCode int    `json:"statusCode"`
		Message    string `json:"message"`
	} `json:"data"`
}

type copilotShutdownModel struct {
	Requests struct {
		Count int64 `json:"count"`
	} `json:"requests"`
	Usage struct {
		InputTokens      int64 `json:"inputTokens"`
		OutputTokens     int64 `json:"outputTokens"`
		CacheReadTokens  int64 `json:"cacheReadTokens"`
		CacheWriteTokens int64 `json:"cacheWriteTokens"`
		ReasoningTokens  int64 `json:"reasoningTokens"`
	} `json:"usage"`
}

// copilotRuntimeScanState is the fold this follower accumulates. It must be
// checkpointable in full: resuming at byte N with an empty state would produce
// a different answer from scanning [0,N) first.
type copilotRuntimeScanState struct {
	model                 string
	effort                string
	contextTier           string
	copilotVersion        string
	userMessages          int
	assistantMessages     int
	assistantOutputTokens int64
	lifetimes             int

	context    CopilotContextTelemetry
	hasContext bool

	usage *CopilotUsage

	nanoAIU         float64
	hasNanoAIU      bool
	premiumRequests float64

	lastError *CopilotErrorObservation
}

func newCopilotRuntimeScanState() copilotRuntimeScanState { return copilotRuntimeScanState{} }

// clone is a deep copy of everything the scan mutates. The pointer fields are
// replaced rather than shared so an appended-then-discarded scan cannot leak
// its writes back into the committed state.
func (s copilotRuntimeScanState) clone() copilotRuntimeScanState {
	out := s
	if s.usage != nil {
		usage := *s.usage
		out.usage = &usage
	}
	if s.lastError != nil {
		lastError := *s.lastError
		out.lastError = &lastError
	}
	return out
}

func (s copilotRuntimeScanState) snapshot() CopilotRuntimeSnapshot {
	snap := CopilotRuntimeSnapshot{
		Model:                 s.model,
		Effort:                s.effort,
		ContextTier:           s.contextTier,
		CopilotVersion:        s.copilotVersion,
		UserMessages:          s.userMessages,
		AssistantMessages:     s.assistantMessages,
		AssistantOutputTokens: s.assistantOutputTokens,
		Lifetimes:             s.lifetimes,
		Context:               s.context,
		HasContext:            s.hasContext,
		NanoAIU:               s.nanoAIU,
		HasNanoAIU:            s.hasNanoAIU,
		PremiumRequests:       s.premiumRequests,
	}
	if s.usage != nil {
		usage := *s.usage
		snap.Usage = &usage
	}
	if s.lastError != nil {
		lastError := *s.lastError
		snap.LastError = &lastError
	}
	return snap
}

// consumeLine folds one complete record. It returns false only for a line that
// looked relevant but could not be decoded, which the follower treats as doubt
// on an APPEND scan (the writer may have been mid-flush) and ignores on a full
// scan (a corrupt historical record must not block the rest of the log).
func (s *copilotRuntimeScanState) consumeLine(line []byte) bool {
	if len(bytes.TrimSpace(line)) == 0 {
		return true
	}
	if !copilotTelemetryLineOfInterest(line) {
		return true
	}
	var event copilotTelemetryEvent
	if json.Unmarshal(line, &event) != nil {
		return false
	}
	switch event.Type {
	case "session.start", "session.resume":
		s.lifetimes++
		if event.Data.SelectedModel != "" {
			s.model = event.Data.SelectedModel
		}
		if event.Data.CopilotVersion != "" {
			s.copilotVersion = event.Data.CopilotVersion
		}
		s.applyEffort(event.Data.ReasoningEfrt)
		s.applyContextTier(event.Data.ContextTier)
	case "session.model_change":
		if event.Data.NewModel != "" {
			s.model = event.Data.NewModel
		}
		s.applyEffort(event.Data.ReasoningEfrt)
		s.applyContextTier(event.Data.ContextTier)
	case "user.message":
		s.userMessages++
	case "assistant.message":
		s.assistantMessages++
		s.assistantOutputTokens += max(event.Data.OutputTokens, 0)
		if event.Data.Model != "" {
			s.model = event.Data.Model
		}
	case "session.usage_checkpoint":
		s.applyCost(event.Data.TotalNanoAiu, event.Data.TotalPremiumRequests)
	case "session.compaction_start":
		// The pre-compaction reading is the last honest "how full was the
		// window" figure before Copilot rewrites the conversation.
		s.setContext(CopilotContextTelemetry{
			CurrentTokens:         event.Data.CurrentTokens,
			SystemTokens:          event.Data.SystemTokens,
			ConversationTokens:    event.Data.ConversationTokens,
			ToolDefinitionsTokens: event.Data.ToolDefinitionsTokens,
			TokenLimit:            event.Data.TokenLimit,
			Source:                "compaction_start",
		})
	case "session.compaction_complete":
		// A FAILED compaction leaves the window where it was, so its
		// post-compaction figures describe nothing and are ignored.
		if event.Data.Success != nil && !*event.Data.Success {
			return true
		}
		s.setContext(CopilotContextTelemetry{
			CurrentTokens:         event.Data.PostCompactionTokens,
			SystemTokens:          event.Data.SystemTokens,
			ConversationTokens:    event.Data.ConversationTokens,
			ToolDefinitionsTokens: event.Data.ToolDefinitionsTokens,
			TokenLimit:            event.Data.TokenLimit,
			Source:                "compaction_complete",
		})
	case "session.truncation":
		// Truncation reports only the conversation half of the window, so it
		// contributes a limit and a conversation figure and deliberately does
		// not claim a total.
		s.setContext(CopilotContextTelemetry{
			ConversationTokens: event.Data.PostTruncationTokens,
			TokenLimit:         event.Data.TokenLimit,
			Source:             "truncation",
		})
	case "session.shutdown":
		if event.Data.CurrentModel != "" {
			s.model = event.Data.CurrentModel
		}
		s.applyCost(event.Data.TotalNanoAiu, event.Data.TotalPremiumRequests)
		s.applyShutdownUsage(event.Data.ModelMetrics)
		// Shutdown carries occupancy but never a tokenLimit, so an earlier
		// compaction's limit is carried forward rather than blanked: the model
		// has not changed under us if the session simply exited.
		next := CopilotContextTelemetry{
			CurrentTokens:         event.Data.CurrentTokens,
			SystemTokens:          event.Data.SystemTokens,
			ConversationTokens:    event.Data.ConversationTokens,
			ToolDefinitionsTokens: event.Data.ToolDefinitionsTokens,
			TokenLimit:            s.context.TokenLimit,
			Source:                "shutdown",
		}
		s.setContext(next)
	case "session.error":
		if event.Data.ErrorType == "" && event.Data.Message == "" {
			return true
		}
		s.lastError = &CopilotErrorObservation{
			ErrorType:  event.Data.ErrorType,
			ErrorCode:  event.Data.ErrorCode,
			StatusCode: event.Data.StatusCode,
			Message:    sanitizeCopilotText(event.Data.Message),
		}
	}
	return true
}

// setContext records an observation only when it says something. An event that
// carries neither an occupancy nor a limit would otherwise blank a good
// earlier reading.
func (s *copilotRuntimeScanState) setContext(next CopilotContextTelemetry) {
	if next.CurrentTokens <= 0 && next.ConversationTokens <= 0 && next.TokenLimit <= 0 {
		return
	}
	s.context = next
	s.hasContext = true
}

// applyEffort accepts Copilot's reasoningEffort, which the schema types as
// `string | null` on model_change and as a plain string elsewhere. An explicit
// null CLEARS the effort — that is the CLI saying the new model has none —
// while an absent key leaves the previous value in force.
func (s *copilotRuntimeScanState) applyEffort(raw json.RawMessage) {
	if len(raw) == 0 {
		return
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		s.effort = ""
		return
	}
	var effort string
	if json.Unmarshal(raw, &effort) != nil {
		return
	}
	s.effort = strings.TrimSpace(effort)
}

// applyContextTier mirrors applyEffort: Copilot writes an explicit null for a
// non-tiered model, and that is information, not absence.
func (s *copilotRuntimeScanState) applyContextTier(tier *string) {
	if tier == nil {
		return
	}
	s.contextTier = strings.TrimSpace(*tier)
}

func (s *copilotRuntimeScanState) applyCost(nanoAIU, premium *float64) {
	if nanoAIU != nil && *nanoAIU >= 0 {
		s.nanoAIU = *nanoAIU
		s.hasNanoAIU = true
	}
	if premium != nil && *premium >= 0 {
		s.premiumRequests = *premium
	}
}

// applyShutdownUsage replaces (rather than accumulates) the usage totals.
// Copilot's modelMetrics are session-lifetime cumulative and are restored
// across a resume, so the newest shutdown record already includes every
// earlier lifetime; adding them would double-count a resumed session.
func (s *copilotRuntimeScanState) applyShutdownUsage(metrics map[string]copilotShutdownModel) {
	if len(metrics) == 0 {
		return
	}
	usage := &CopilotUsage{}
	for _, metric := range metrics {
		usage.Requests += metric.Requests.Count
		usage.InputTokens += metric.Usage.InputTokens
		usage.OutputTokens += metric.Usage.OutputTokens
		usage.CacheReadTokens += metric.Usage.CacheReadTokens
		usage.CacheWriteTokens += metric.Usage.CacheWriteTokens
		usage.ReasoningTokens += metric.Usage.ReasoningTokens
	}
	s.usage = usage
}
