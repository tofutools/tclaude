package testharness

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// CodexSim is a behavior-accurate simulator of one OpenAI Codex CLI
// instance — the Codex analog of CCSim. It owns a real rollout `.jsonl`
// under ~/.codex/sessions/YYYY/MM/DD/rollout-<ts>-<uuid>.jsonl and
// processes keystrokes routed by TmuxSim's send-keys dispatcher, with
// the same OnInput/SetCommandDelay handler design as CCSim.
//
// Why this exists: tclaude is going harness-agnostic (drive Codex CLI
// alongside Claude Code). CodexSim lets the Codex read path be tested
// against a faithful on-disk rollout the same way CCSim tests the CC
// path, so we can prove Codex parity at real surfaces.
//
// # Ground-truth format (Codex CLI v0.139.0)
//
// Each rollout line is an envelope `{timestamp, type, payload}` where
// `type` ∈ {session_meta, event_msg, response_item, turn_context, …}
// — NOT the `RolloutLine{timestamp,item}` model older research
// assumed. The shapes modelled here were sampled from a real v0.139
// rollout (see testdata/codex_rollout_v0139.jsonl):
//
//   - session_meta  — id, cwd, cli_version, model_provider,
//     originator, source, thread_source, base_instructions{text}.
//     Written once at session creation; carries the session id + cwd
//     the read path keys on. (Codex is date-indexed, cwd lives INSIDE
//     the file — unlike CC's cwd-encoded project dir.)
//   - event_msg     — payload.type ∈ {task_started, user_message,
//     agent_message, token_count, task_complete, …}: turn lifecycle +
//     token telemetry.
//   - response_item — payload.type=message, role ∈ {developer, user,
//     assistant}, content[{type:input_text|output_text, text}].
//   - turn_context  — per-turn snapshot of cwd, model, current_date,
//     timezone, approval_policy, sandbox_policy.
//
// # Faithful but minimal
//
// Like CCSim, this models the lines that carry information the read
// path consumes, not Codex's full system-prompt machinery:
//   - base_instructions text is a short placeholder, not the ~10KB
//     GPT-5 prompt. The field is present and shaped correctly.
//   - The developer/environment_context preamble Codex emits at the
//     first turn is omitted by default — it is noise for tclaude. Add
//     it via a handler if a regression makes us care.
//   - The default input handler writes only the USER side of a turn
//     (task_started + turn_context + user message + user_message
//     event), mirroring CCSim's "don't fabricate assistant output"
//     stance. Compose the agent side with WriteAgentMessage /
//     WriteTokenCount / CompleteTask, or use WriteExchange for a full
//     realistic turn.
//
// # Where Codex diverges from CC (encoded here as institutional memory)
//
//   - Titles do NOT live in the rollout. A renamed Codex session
//     persists its title in ~/.codex/state_5.sqlite `threads.title`
//     (also `tokens_used`, `cwd`, `git_branch`, `first_user_message`,
//     `preview`). For an un-renamed session the title is auto-derived
//     from the first user message. The sim tracks the title in memory
//     (Title()); it does not yet write the Codex state DB — when the
//     parser (JOH-152) decides whether it reads that DB, the writer
//     slots into SetTitle. See JOH-165.
//   - Resume is a subcommand (`codex resume <id>`), not a flag, and it
//     reopens the SAME rollout file by id. HydrateCodexSim models that
//     by locating the existing rollout from the session id.
type CodexSim struct {
	ConvID      string // session id (the rollout uuid)
	Cwd         string
	RolloutPath string

	// Model / Effort / CliVersion / ContextWindow stamp the lines the read
	// path reads for harness/model/effort/context% resolution. Defaults
	// match the sampled v0.139 session; override before Start to model
	// another.
	Model         string
	Effort        string
	CliVersion    string
	ContextWindow int

	// GitBranch, when non-empty, models the working branch. Codex keeps
	// it in the state DB (threads.git_branch), not in rollout turns, so
	// the sim only tracks it; it is reported here for the future state
	// DB writer.
	GitBranch string

	createdAt time.Time
	home      string // HOME the rollout + state DB live under

	mu         sync.Mutex
	title      string
	firstSeen  bool // whether session_meta has been written
	turnCount  int
	lastTurnID string // turn_id of the most recent turn, for CompleteTask
	alive      bool
	buf        strings.Builder
	handlers   []codexHandlerEntry
	delays     []codexDelayEntry
}

// CodexInputHandler processes one submitted Codex input. Return true to
// mark the line consumed; false to fall through to the next handler.
// Handlers run with the sim's lock NOT held — use the Write*/MarkDead
// helpers, which take the lock themselves.
type CodexInputHandler func(c *CodexSim, line string) bool

type codexHandlerEntry struct {
	prefix string
	fn     CodexInputHandler
}

type codexDelayEntry struct {
	prefix string
	d      time.Duration
}

// NewCodexSim picks a fresh session id, computes the date-indexed
// rollout path, and registers Shutdown via t.Cleanup. Inert until
// Start.
func NewCodexSim(t *testing.T, home, cwd string) *CodexSim {
	t.Helper()
	return NewCodexSimWithID(t, home, generateConvID(), cwd)
}

// NewCodexSimWithID is NewCodexSim with a caller-chosen session id.
// Used when a test reuses a fixed id across setup and assertions.
func NewCodexSimWithID(t *testing.T, home, convID, cwd string) *CodexSim {
	t.Helper()
	if cwd == "" {
		cwd = filepath.Join(home, "sim-cwd")
	}
	// See NewCCSimWithID: model the invariant that a live/resumable session's
	// working directory exists on disk, so resume flow tests take the normal
	// path rather than the new missing-cwd guard. Best-effort.
	_ = os.MkdirAll(cwd, 0o755)
	created := time.Now()
	cx := &CodexSim{
		ConvID:        convID,
		Cwd:           cwd,
		RolloutPath:   codexRolloutPath(home, convID, created),
		Model:         "gpt-5.5",
		Effort:        "high",
		CliVersion:    "0.139.0",
		ContextWindow: 258400,
		createdAt:     created,
		home:          home,
	}
	cx.installDefaultHandlers()
	t.Cleanup(cx.Shutdown)
	return cx
}

// Start materialises the rollout with a session_meta line and flips
// alive=true. Idempotent: a Start on an already-started sim just
// re-arms alive (mirrors `codex resume` reopening the file). The
// session_meta line is written only once.
func (c *CodexSim) Start() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.alive {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(c.RolloutPath), 0o755); err != nil {
		return err
	}
	if !c.firstSeen {
		if _, err := os.Stat(c.RolloutPath); os.IsNotExist(err) {
			if err := c.writeSessionMetaLocked(); err != nil {
				return err
			}
		}
		c.firstSeen = true
	}
	c.alive = true
	return nil
}

// Receive is the tmux send-keys entry point — identical contract to
// CCSim.Receive. Plain text accumulates; "Enter" flushes the buffer
// through the registered handlers (honouring any per-prefix delay,
// which runs the handler in a background goroutine so the call returns
// before the turn lands on disk).
func (c *CodexSim) Receive(text string) {
	c.mu.Lock()
	if !c.alive {
		c.mu.Unlock()
		return
	}
	if text != "Enter" {
		c.buf.WriteString(text)
		c.mu.Unlock()
		return
	}
	line := c.buf.String()
	c.buf.Reset()
	delay := c.delayForLocked(line)
	c.mu.Unlock()

	if line == "" {
		return
	}
	if delay > 0 {
		go func() {
			time.Sleep(delay)
			c.dispatch(line)
		}()
		return
	}
	c.dispatch(line)
}

// Shutdown drops alive and discards pending input. Auto-called via
// t.Cleanup; callers may invoke it to model a hard tmux kill-session.
func (c *CodexSim) Shutdown() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.alive = false
	c.buf.Reset()
}

// IsAlive reports whether the simulator is still processing input.
func (c *CodexSim) IsAlive() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.alive
}

// MarkDead flips alive=false. Handlers call it from exit-style commands.
func (c *CodexSim) MarkDead() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.alive = false
}

// Title returns the latest title. For Codex this is an in-memory mirror
// of what would land in state_5.sqlite threads.title (auto-derived from
// the first user message, or a user rename). Production reads the state
// DB; this is a sanity-check accessor.
func (c *CodexSim) Title() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.title
}

// SetTitle records a title the way a Codex rename (#22526) would. Codex
// persists it in state_5.sqlite, NOT in the rollout turns, so this only
// updates the in-memory mirror today. It is the extension point for the
// future Codex state-DB writer (JOH-165): when the parser decides to
// read that DB, persist here.
func (c *CodexSim) SetTitle(title string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.title = title
}

// CreatedUnix is the session's start time as unix seconds — the stamp a
// threads-row writer uses for created_at/updated_at so the seeded row
// agrees with the rollout's session_meta time.
func (c *CodexSim) CreatedUnix() int64 {
	return c.createdAt.Unix()
}

// OnInput registers a handler. Newer registrations win on prefix match.
// Empty prefix matches every input (a custom catch-all that shadows the
// default user-turn fallback).
func (c *CodexSim) OnInput(prefix string, h CodexInputHandler) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.handlers = append([]codexHandlerEntry{{prefix: prefix, fn: h}}, c.handlers...)
}

// SetCommandDelay configures the wait between a matching Enter arriving
// and the handler firing — models Codex being busy for ~Nms after a
// command settles. Newer calls win on prefix match; d=0 clears.
func (c *CodexSim) SetCommandDelay(prefix string, d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.delays = append([]codexDelayEntry{{prefix: prefix, d: d}}, c.delays...)
}

// WriteUserInput writes the USER side of one turn: a task_started
// event, a turn_context snapshot, the user message response_item, and
// the user_message event. It also derives the session title from the
// first user message (as Codex does) when no explicit title is set.
// This is what the default catch-all handler calls.
func (c *CodexSim) WriteUserInput(text string) error {
	c.mu.Lock()
	c.turnCount++
	turnID := generateConvID()
	if c.title == "" {
		c.title = codexPreview(text)
	}
	startedAt := time.Now().Unix()
	c.mu.Unlock()

	if err := c.appendLine("event_msg", map[string]any{
		"type":                    "task_started",
		"turn_id":                 turnID,
		"started_at":              startedAt,
		"model_context_window":    c.ContextWindow,
		"collaboration_mode_kind": "default",
	}); err != nil {
		return err
	}
	if err := c.appendLine("turn_context", c.turnContextPayload(turnID)); err != nil {
		return err
	}
	if err := c.appendLine("response_item", codexMessage("user", "input_text", text)); err != nil {
		return err
	}
	return c.appendLine("event_msg", map[string]any{
		"type":          "user_message",
		"message":       text,
		"images":        []any{},
		"local_images":  []any{},
		"text_elements": []any{},
	})
}

// WriteAgentMessage writes the ASSISTANT side of a turn: an
// agent_message event and the assistant message response_item.
func (c *CodexSim) WriteAgentMessage(text string) error {
	if err := c.appendLine("event_msg", map[string]any{
		"type":            "agent_message",
		"message":         text,
		"phase":           "final_answer",
		"memory_citation": nil,
	}); err != nil {
		return err
	}
	msg := codexMessage("assistant", "output_text", text)
	msg["phase"] = "final_answer"
	return c.appendLine("response_item", msg)
}

// CodexTokenUsage mirrors the token-usage block inside a token_count
// event (both total_token_usage and last_token_usage use this shape).
// This is the telemetry that later feeds context% into SQLite (JOH-170).
type CodexTokenUsage struct {
	InputTokens           int `json:"input_tokens"`
	CachedInputTokens     int `json:"cached_input_tokens"`
	OutputTokens          int `json:"output_tokens"`
	ReasoningOutputTokens int `json:"reasoning_output_tokens"`
	TotalTokens           int `json:"total_tokens"`
}

// WriteTokenCount writes a token_count event. total is the cumulative
// usage for the session; last is the most recent turn's usage. The
// model_context_window and a minimal valid rate_limits block are filled
// from the sim's config.
func (c *CodexSim) WriteTokenCount(total, last CodexTokenUsage) error {
	return c.appendLine("event_msg", map[string]any{
		"type": "token_count",
		"info": map[string]any{
			"total_token_usage":    total,
			"last_token_usage":     last,
			"model_context_window": c.ContextWindow,
		},
		"rate_limits": map[string]any{
			"limit_id":   "codex",
			"limit_name": nil,
			"primary": map[string]any{
				"used_percent":   0.0,
				"window_minutes": 43200,
				"resets_at":      time.Now().Add(30 * 24 * time.Hour).Unix(),
			},
			"secondary":               nil,
			"credits":                 nil,
			"individual_limit":        nil,
			"plan_type":               "free",
			"rate_limit_reached_type": nil,
		},
	})
}

// CodexRateLimitWindowSeed describes one rate-limit window for
// WriteTokenCountRateLimits — a subscription account's 5-hour (≈300 min) or
// weekly (≈10080 min) bucket. A zero ResetsAt writes resets_at:0 (the
// "no reset timestamp" shape).
type CodexRateLimitWindowSeed struct {
	UsedPercent   float64
	WindowMinutes int
	ResetsAt      time.Time
}

// WriteTokenCountRateLimits writes a token_count event carrying the given
// rate_limits windows (either may be nil). Where WriteTokenCount's default
// block models a free account's lone 30-day window, this lets a test model a
// subscription account's 5-hour + weekly limits — the shape the dashboard's
// Codex usage readout (agentd/codex_usage.go) consumes off the rollout.
func (c *CodexSim) WriteTokenCountRateLimits(total, last CodexTokenUsage, primary, secondary *CodexRateLimitWindowSeed) error {
	return c.appendLine("event_msg", map[string]any{
		"type": "token_count",
		"info": map[string]any{
			"total_token_usage":    total,
			"last_token_usage":     last,
			"model_context_window": c.ContextWindow,
		},
		"rate_limits": map[string]any{
			"limit_id":                "codex",
			"limit_name":              nil,
			"primary":                 codexRateLimitSeedToMap(primary),
			"secondary":               codexRateLimitSeedToMap(secondary),
			"credits":                 nil,
			"individual_limit":        nil,
			"plan_type":               "plus",
			"rate_limit_reached_type": nil,
		},
	})
}

// codexRateLimitSeedToMap renders a window seed as the on-disk rate-limit
// window object, or nil (a JSON null slot) when the seed is nil.
func codexRateLimitSeedToMap(s *CodexRateLimitWindowSeed) any {
	if s == nil {
		return nil
	}
	resets := int64(0)
	if !s.ResetsAt.IsZero() {
		resets = s.ResetsAt.Unix()
	}
	return map[string]any{
		"used_percent":   s.UsedPercent,
		"window_minutes": s.WindowMinutes,
		"resets_at":      resets,
	}
}

// CompleteTask writes a task_complete event closing the current turn.
func (c *CodexSim) CompleteTask(lastAgentMessage string) error {
	c.mu.Lock()
	turnID := c.lastTurnID
	c.mu.Unlock()
	return c.appendLine("event_msg", map[string]any{
		"type":                   "task_complete",
		"turn_id":                turnID,
		"last_agent_message":     lastAgentMessage,
		"completed_at":           time.Now().Unix(),
		"duration_ms":            0,
		"time_to_first_token_ms": 0,
	})
}

// WriteExchange writes a full faithful turn: the user side, the agent
// side, a token_count, and a task_complete — the shape a real completed
// "hello" → "Hi" turn produces on disk. Convenience for tests that want
// a realistic complete session rather than composing the pieces.
func (c *CodexSim) WriteExchange(userText, agentText string) error {
	if err := c.WriteUserInput(userText); err != nil {
		return err
	}
	if err := c.WriteAgentMessage(agentText); err != nil {
		return err
	}
	usage := CodexTokenUsage{InputTokens: 100, CachedInputTokens: 0, OutputTokens: len(agentText), TotalTokens: 100 + len(agentText)}
	if err := c.WriteTokenCount(usage, usage); err != nil {
		return err
	}
	return c.CompleteTask(agentText)
}

func (c *CodexSim) installDefaultHandlers() {
	// Mirror CCSim: dispatch walks top-to-bottom, first matching prefix
	// wins, OnInput prepends so test handlers shadow defaults. Codex's
	// exact slash-command surface (rename/compact) is still being
	// confirmed against the real CLI, so the only specific default is
	// `/quit`; everything else falls through to the catch-all user turn.
	// More commands are added here as we confirm them (the "sims accrete
	// knowledge" rule).
	c.handlers = []codexHandlerEntry{
		// `/quit` is Codex's soft-exit slash command (the CC `/exit`
		// analog). Real Codex routes Quit to
		// request_quit_without_confirmation — a one-shot graceful
		// shutdown, no confirm prompt, no turn written — so the sim just
		// flips alive=false. This is what lets the daemon's soft-stop
		// path (stopOneConv → harness SoftExitCommand "/quit") take a
		// Codex pane offline gracefully instead of falling back to a
		// hard kill-session.
		{prefix: "/quit", fn: func(c *CodexSim, _ string) bool {
			c.MarkDead()
			return true
		}},
		{prefix: "", fn: func(c *CodexSim, line string) bool {
			_ = c.WriteUserInput(line)
			return true
		}},
	}
}

func (c *CodexSim) dispatch(line string) {
	c.mu.Lock()
	if !c.alive {
		c.mu.Unlock()
		return
	}
	hs := make([]codexHandlerEntry, len(c.handlers))
	copy(hs, c.handlers)
	c.mu.Unlock()

	for _, h := range hs {
		if h.prefix == "" || strings.HasPrefix(line, h.prefix) {
			if h.fn(c, line) {
				return
			}
		}
	}
}

func (c *CodexSim) delayForLocked(line string) time.Duration {
	for _, d := range c.delays {
		if d.prefix == "" || strings.HasPrefix(line, d.prefix) {
			return d.d
		}
	}
	return 0
}

// lastTurnID is updated by turnContextPayload so CompleteTask can close
// the turn it opened. Guarded by mu.
func (c *CodexSim) turnContextPayload(turnID string) map[string]any {
	c.mu.Lock()
	c.lastTurnID = turnID
	c.mu.Unlock()
	return map[string]any{
		"turn_id":         turnID,
		"cwd":             c.Cwd,
		"workspace_roots": []string{c.Cwd},
		"current_date":    c.createdAt.Format("2006-01-02"),
		"timezone":        "Europe/Stockholm",
		"approval_policy": "on-request",
		"sandbox_policy": map[string]any{
			"type":           "workspace-write",
			"network_access": false,
		},
		"model":       c.Model,
		"personality": "pragmatic",
		"collaboration_mode": map[string]any{
			"mode": "default",
			"settings": map[string]any{
				"model":            c.Model,
				"reasoning_effort": c.Effort,
			},
		},
		"effort":  c.Effort,
		"summary": "auto",
	}
}

func (c *CodexSim) writeSessionMetaLocked() error {
	return c.appendLineLocked("session_meta", map[string]any{
		"id":             c.ConvID,
		"timestamp":      codexNow(),
		"cwd":            c.Cwd,
		"originator":     "codex-tui",
		"cli_version":    c.CliVersion,
		"source":         "cli",
		"thread_source":  "user",
		"model_provider": "openai",
		"base_instructions": map[string]any{
			"text": "<codex base instructions placeholder — see testharness.CodexSim>",
		},
	})
}

// appendLine wraps payload in the {timestamp,type,payload} envelope and
// appends it. Takes the lock; handler-facing.
func (c *CodexSim) appendLine(typ string, payload map[string]any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.appendLineLocked(typ, payload)
}

func (c *CodexSim) appendLineLocked(typ string, payload map[string]any) error {
	b, err := json.Marshal(map[string]any{
		"timestamp": codexNow(),
		"type":      typ,
		"payload":   payload,
	})
	if err != nil {
		return err
	}
	f, err := os.OpenFile(c.RolloutPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = f.Write(append(b, '\n'))
	return err
}

// codexMessage builds a response_item message payload.
func codexMessage(role, contentType, text string) map[string]any {
	return map[string]any{
		"type": "message",
		"role": role,
		"content": []any{
			map[string]any{"type": contentType, "text": text},
		},
	}
}

// HydrateCodexSim builds a CodexSim against an existing on-disk rollout,
// locating it by session id (modelling `codex resume <id>` finding the
// file in the date tree). Reads the latest title off disk so Title()
// answers post-resume.
func HydrateCodexSim(t *testing.T, home, convID, cwd string) *CodexSim {
	t.Helper()
	cx := NewCodexSimWithID(t, home, convID, cwd)
	if p := findRolloutPath(home, convID); p != "" {
		cx.RolloutPath = p
		cx.firstSeen = true
	}
	cx.title = readLatestCodexTitle(cx.RolloutPath)
	return cx
}

// codexRolloutPath builds the date-indexed rollout path. The date dir
// and filename timestamp use LOCAL time (matching real Codex:
// dir 2026/06/13, file rollout-2026-06-13T10-06-05-…); the timestamp
// INSIDE the file is UTC. Filename ts uses '-' separators because ':'
// is not path-safe.
func codexRolloutPath(home, convID string, created time.Time) string {
	dir := filepath.Join(home, ".codex", "sessions",
		created.Format("2006"), created.Format("01"), created.Format("02"))
	name := fmt.Sprintf("rollout-%s-%s.jsonl", created.Format("2006-01-02T15-04-05"), convID)
	return filepath.Join(dir, name)
}

// findRolloutPath locates an existing rollout by session id, scanning
// the whole sessions tree (resume finds by id, not by date).
func findRolloutPath(home, convID string) string {
	root := filepath.Join(home, ".codex", "sessions")
	var found string
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if strings.HasPrefix(info.Name(), "rollout-") && strings.HasSuffix(info.Name(), convID+".jsonl") {
			found = path
		}
		return nil
	})
	return found
}

// readLatestCodexTitle derives a display title from a rollout the way
// an un-renamed Codex session would: the first user_message. (A real
// rename lives in state_5.sqlite, outside the rollout — see SetTitle.)
func readLatestCodexTitle(rolloutPath string) string {
	data, err := os.ReadFile(rolloutPath)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" {
			continue
		}
		var env struct {
			Type    string `json:"type"`
			Payload struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"payload"`
		}
		if err := json.Unmarshal([]byte(line), &env); err != nil {
			continue
		}
		if env.Type == "event_msg" && env.Payload.Type == "user_message" && env.Payload.Message != "" {
			return codexPreview(env.Payload.Message)
		}
	}
	return ""
}

// codexPreview trims a user message to a one-line preview/title, the way
// Codex derives threads.title / threads.preview from the first message.
func codexPreview(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	const max = 80
	if len(s) > max {
		s = s[:max]
	}
	return s
}

// codexNow formats a timestamp the way Codex stamps rollout lines:
// UTC, millisecond precision, Z suffix (e.g. 2026-06-13T08:06:09.418Z).
func codexNow() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
}
