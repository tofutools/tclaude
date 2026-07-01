package testharness

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/convops"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// CCSim is a behavior-accurate simulator of one Claude Code instance.
// It owns a real .jsonl under ~/.claude/projects/<encoded-cwd>/<convID>.jsonl
// and processes keystrokes routed by TmuxSim's send-keys dispatcher.
//
// Lifecycle mirrors real CC closely enough that production code reading
// the .jsonl (FreshConvRow / ScanAndUpsertFile) sees the same shape:
//   - Start writes an initial summary turn.
//   - Plain text accumulates in a buffer until "Enter" arrives, then
//     becomes a user turn (or, if it's a slash command, gets parsed).
//   - The default handler set covers /rename, /exit, /compact, and a
//     catch-all user-turn writer. Tests register additional behaviors
//     via OnInput(prefix, handler); custom handlers run before defaults
//     so a test can override a builtin without touching this file.
//   - Per-prefix delays via SetCommandDelay model "CC takes a moment
//     to process this command" — the asynchrony catches bugs where
//     prod assumes send-keys success ⇒ turn already on disk.
//
// What it deliberately does NOT model:
//   - Assistant responses / tool use. Scenarios that need these can
//     register a handler that calls AppendTurn for the assistant turn
//     they want.
//   - Bracketed-paste coalescing. Add a quirk via OnInput when a
//     regression makes us care.
type CCSim struct {
	ConvID    string
	Cwd       string
	JsonlPath string

	// SessionID is the owning tclaude session's TCLAUDE_SESSION_ID — the
	// stable per-process key the hook callback uses to track a conv-id
	// rotation (/clear, /resume) across the rotation. Set by the spawner
	// / HaveAliveSession to the session row's ID; "" for a CCSim not
	// tied to a tclaude session, which makes the /clear handler a no-op
	// (it has no env-keyed session to migrate).
	SessionID string

	// GitBranch, when non-empty, is stamped into the gitBranch field of
	// every user turn — mirroring how real Claude Code records the
	// working branch on each .jsonl entry. Set it before Start; a
	// conv_index scan then resolves the agent's branch the same way it
	// does in production. Empty (the default) writes turns with no
	// gitBranch, exactly as a non-git-repo session does.
	GitBranch string

	mu       sync.Mutex
	title    string
	alive    bool
	buf      strings.Builder
	handlers []handlerEntry
	delays   []delayEntry

	// Remote-control (CC's /remote-control) state machine. remoteOn is the
	// modeled live state: a /remote-control toggle while OFF turns it on with
	// no confirmation; a toggle while ON opens a disconnect-confirm MENU
	// instead of flipping immediately. rcMenuOpen tracks that menu being up
	// and rcMenuPos the highlight position — Up moves toward "disconnect"
	// (rcDisconnectMenuOffset moves up from the default "keep connected"
	// entry), Enter selects the highlight. This mirrors the real CC quirk the
	// daemon's disable path must drive (Up, Up, Enter), so a flow test sees
	// whether the injected keystrokes ACTUALLY disconnected — not merely that
	// some keys reached the pane. Read via RemoteControlOn().
	remoteOn   bool
	rcMenuOpen bool
	rcMenuPos  int
	// rcFailed models Claude Code's FAILED remote-control footer pill (a red
	// "/rc" shown when the connection couldn't be established). Independent of
	// remoteOn: a pane can be armed-and-reachable (remoteOn && !rcFailed) or
	// armed-but-failed (remoteOn && rcFailed). Set via SetRemoteControlFailed;
	// RenderPane reflects it so the daemon's footer readback can be tested
	// against the failed branch. Off by default — most tests want the healthy
	// pill.
	rcFailed bool
}

// rcDisconnectMenuOffset is how many Up presses move the disconnect-confirm
// menu's highlight from its default entry ("keep connected") to "disconnect".
// CC's menu sits the default away from disconnect on purpose, so a bare Enter
// keeps the connection; the daemon walks Up, Up, Enter to actually disconnect
// (operator-verified). The sim clamps at this offset (disconnect is the top
// entry), so two-or-more Ups all land on it.
const rcDisconnectMenuOffset = 2

// InputHandler processes one submitted CC input. Return true to mark
// the line consumed; false to fall through to the next handler.
//
// Handlers run while the CCSim's lock is NOT held — callers must use
// AppendTurn / WriteCustomTitle / MarkDead, which take the lock
// themselves. This avoids accidental deadlocks when a handler does
// I/O.
type InputHandler func(c *CCSim, line string) bool

type handlerEntry struct {
	prefix string
	fn     InputHandler
}

type delayEntry struct {
	prefix string
	d      time.Duration
}

// NewCCSim picks a fresh conv-id, computes the .jsonl path, and
// registers a Shutdown via t.Cleanup. The simulator is inert until
// Start is called. Default handlers (rename / exit / compact / user
// fallback) are pre-installed; tests override via OnInput.
func NewCCSim(t *testing.T, home, cwd string) *CCSim {
	t.Helper()
	return NewCCSimWithID(t, home, generateConvID(), cwd)
}

// NewCCSimWithID is NewCCSim with a caller-chosen conv-id. Used when
// a test needs to reuse a fixed ID across setup and assertions.
func NewCCSimWithID(t *testing.T, home, convID, cwd string) *CCSim {
	t.Helper()
	if cwd == "" {
		// Default to a real, home-rooted working dir (home is a t.TempDir, so
		// it's writable and auto-cleaned) and create it. A real spawned/resumed
		// session always has an existing cwd, and the resume path now refuses to
		// relaunch into a vanished one (error:missing_cwd) — so the sim's default
		// must exist. Only the default is created: an explicit cwd is left
		// untouched, so the sim never litters arbitrary absolute paths (e.g.
		// /tmp/...) on disk. A test that needs its explicit cwd to exist passes a
		// real one (t.TempDir() / f.World.HomeDir).
		cwd = filepath.Join(home, "sim-cwd")
		_ = os.MkdirAll(cwd, 0o755)
	}
	projectDir := filepath.Join(home, ".claude", "projects", convops.PathToProjectDir(cwd))
	cc := &CCSim{
		ConvID:    convID,
		Cwd:       cwd,
		JsonlPath: filepath.Join(projectDir, convID+".jsonl"),
	}
	cc.installDefaultHandlers()
	t.Cleanup(cc.Shutdown)
	return cc
}

// Start materialises the .jsonl with an initial summary turn and
// flips alive=true. Idempotent: a Start on an already-started sim
// just re-arms alive (mirrors a resume after /exit).
func (c *CCSim) Start() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.alive {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(c.JsonlPath), 0o755); err != nil {
		return err
	}
	// Only write the initial summary turn the first time. Subsequent
	// Starts (post-/exit resume) just re-arm alive without rewriting
	// the header.
	if _, err := os.Stat(c.JsonlPath); os.IsNotExist(err) {
		if err := c.appendLineLocked(map[string]any{
			"type":      "summary",
			"summary":   "session " + c.ConvID,
			"sessionId": c.ConvID,
			"timestamp": now(),
		}); err != nil {
			return err
		}
	}
	c.alive = true
	return nil
}

// Receive is the tmux send-keys entry point. Plain text accumulates;
// "Enter" flushes the buffer through the registered handlers (with
// any configured per-prefix delay).
//
// When a delay is configured for the matched prefix, processing runs
// in a background goroutine so the call returns immediately — that
// mirrors prod's send-keys/Enter returning before CC has actually
// committed the turn to disk.
func (c *CCSim) Receive(text string) {
	c.mu.Lock()
	if !c.alive {
		c.mu.Unlock()
		return
	}
	// While CC's disconnect-confirm menu is up, arrow keys move the highlight
	// and Enter selects the highlighted entry — none of it touches the input
	// buffer. Model that so a test observes the REAL effect of the disable
	// keystrokes (did "disconnect" actually get chosen?) rather than just that
	// keys reached the pane. A stray Enter here — e.g. injectTextAndSubmit's
	// belt-and-suspenders second Enter — lands on the default entry and leaves
	// remote access ON, which is the exact bug the disable path had.
	if c.rcMenuOpen {
		switch text {
		case "Up":
			if c.rcMenuPos < rcDisconnectMenuOffset {
				c.rcMenuPos++
			}
		case "Down":
			if c.rcMenuPos > 0 {
				c.rcMenuPos--
			}
		case "Enter":
			if c.rcMenuPos == rcDisconnectMenuOffset {
				c.remoteOn = false
			}
			c.rcMenuOpen = false
			c.rcMenuPos = 0
		default:
			// Typing does nothing while the confirm menu is up.
		}
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

// Shutdown drops alive and discards any pending input. Auto-called via
// t.Cleanup; callers may invoke directly to model a hard tmux
// kill-session.
func (c *CCSim) Shutdown() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.alive = false
	c.buf.Reset()
}

// IsAlive reports whether the simulator is still processing input.
// Flips false on /exit, MarkDead, or Shutdown.
func (c *CCSim) IsAlive() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.alive
}

// Title returns the latest customTitle written via /rename or
// WriteCustomTitle. Reading via this is faster than parsing the .jsonl
// and useful as a sanity check; production code reads the file.
func (c *CCSim) Title() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.title
}

// RemoteControlOn reports the simulator's modeled CC remote-access state — set
// on by a /remote-control toggle while off, and back off only when the
// disconnect-confirm menu's "disconnect" entry is actually selected (Up, Up,
// Enter). Tests assert on THIS — the real outcome of the injected keystrokes —
// not on the daemon's optimistically-recorded best-known state, which always
// reflects the intended toggle whether or not the menu was driven correctly.
func (c *CCSim) RemoteControlOn() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.remoteOn
}

// SetRemoteControlFailed models the connection behind an armed pane
// failing/recovering: it flips the rcFailed flag so RenderPane draws the red
// "/rc failed" pill (true) or the healthy "/rc" pill (false). Only meaningful
// while remoteOn — a disarmed pane shows no pill regardless. Lets a flow test
// exercise the daemon's failed-readback branch.
func (c *CCSim) SetRemoteControlFailed(failed bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rcFailed = failed
}

// RenderPane returns a stand-in for what `tmux capture-pane -e` would print for
// this CC pane: a couple of transcript lines and a bottom status row that
// carries the "/rc" remote-control pill when Remote Access is armed — exactly
// the footer signal the daemon's on-demand readback (observeRemoteControl)
// scans for. The pill is drawn as a claude.ai/code OSC 8 hyperlink (so URL
// extraction is exercised) and, when rcFailed, in red (so the failed branch is
// exercised). A disarmed pane shows no pill. This is the institutional
// knowledge of "what CC's footer looks like" encoded in the sim, mirroring how
// the disconnect-confirm menu quirk lives in the Receive state machine.
func (c *CCSim) RenderPane() string {
	c.mu.Lock()
	on, failed, conv := c.remoteOn, c.rcFailed, c.ConvID
	c.mu.Unlock()
	return renderCCFooter(on, failed, conv)
}

// renderCCFooter builds the pane content RenderPane returns. Kept as a free
// function so the exact bytes (ANSI + OSC 8) are easy to read and adjust in one
// place if the real CC footer changes.
func renderCCFooter(on, failed bool, convID string) string {
	var b strings.Builder
	// A couple of transcript-ish rows above the footer. Deliberately free of a
	// bounded "/rc" token so the band scan has nothing but the real pill to
	// find. (Decoy-text robustness — conversation that DOES say "/rc" far above
	// the footer — is pinned by the parser unit tests, not here.)
	b.WriteString("> waiting for input\n")
	b.WriteString("\n")
	// The model/status row, padded to a realistic terminal width (real CC
	// panes are wide; a too-narrow capture is what makes the daemon report
	// "unknown" rather than "off"). The "/rc" pill sits right-aligned, and ONLY
	// when armed — mirroring real Claude Code, which draws it solely while
	// Remote Access is on.
	const statusWidth = 72
	status := "claude-opus  12%  7d"
	if pad := statusWidth - len(status); pad > 0 {
		status += strings.Repeat(" ", pad)
	}
	if on {
		pill := osc8(("https://claude.ai/code/" + convID), "/rc")
		if failed {
			pill = ansiRed(pill)
		}
		status = status + "   " + pill
	}
	b.WriteString(status)
	b.WriteString("\nauto mode on (shift+tab to cycle)\n")
	return b.String()
}

// osc8 wraps label in an OSC 8 hyperlink to url (ESC ] 8 ; ; url ST … ESC ] 8 ;
// ; ST), the terminal escape Claude Code uses to make the footer pill
// clickable. ST here is ESC \.
func osc8(url, label string) string {
	return "\x1b]8;;" + url + "\x1b\\" + label + "\x1b]8;;\x1b\\"
}

// ansiRed wraps s in an SGR red-foreground sequence (31) reset by SGR 0 — how
// Claude Code colours the failed footer pill.
func ansiRed(s string) string {
	return "\x1b[31m" + s + "\x1b[0m"
}

// MarkDead flips alive=false. Handlers call this from /exit-style
// commands so subsequent has-session checks report the session down.
func (c *CCSim) MarkDead() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.alive = false
}

// AppendTurn writes one turn to the .jsonl as a single JSON line.
// Custom handlers use this to record arbitrary turn shapes; the
// sessionId field is auto-injected when missing so handlers don't
// have to remember to set it.
func (c *CCSim) AppendTurn(obj map[string]any) error {
	if obj == nil {
		obj = map[string]any{}
	}
	if _, ok := obj["sessionId"]; !ok {
		obj["sessionId"] = c.ConvID
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.appendLineLocked(obj)
}

// WriteCustomTitle is the convenience wrapper handlers use to record
// a /rename. Updates the cached title and writes a custom-title turn.
func (c *CCSim) WriteCustomTitle(title string) error {
	c.mu.Lock()
	c.title = title
	err := c.appendLineLocked(map[string]any{
		"type":        "custom-title",
		"customTitle": title,
		"sessionId":   c.ConvID,
	})
	c.mu.Unlock()
	return err
}

// WriteUserTurn appends a plain user message turn to the .jsonl.
// Used by the catch-all default handler and by tests that script
// scenarios where the agent receives a prompt.
func (c *CCSim) WriteUserTurn(content string) error {
	turn := map[string]any{
		"type":      "user",
		"cwd":       c.Cwd,
		"message":   map[string]any{"role": "user", "content": content},
		"timestamp": now(),
	}
	// Real CC stamps gitBranch onto every turn; mirror that when the
	// sim was configured with a branch so conv_index scans pick it up.
	if c.GitBranch != "" {
		turn["gitBranch"] = c.GitBranch
	}
	return c.AppendTurn(turn)
}

// WriteSummary appends a summary turn. Used by /compact-style
// behaviors and by tests that want to seed a summary into the .jsonl.
func (c *CCSim) WriteSummary(summary string) error {
	return c.AppendTurn(map[string]any{
		"type":      "summary",
		"summary":   summary,
		"timestamp": now(),
	})
}

// clear models Claude Code's /clear: the conversation ends and a fresh
// one starts inside the SAME process. CC rotates the conv-id (a new
// .jsonl in the same project dir), keeps the process — and so
// TCLAUDE_SESSION_ID — alive, and fires SessionEnd(reason=clear) on the
// old conv-id followed by SessionStart on the new one. Confirmed
// against a real captured /clear hook recording (issue #192).
//
// Driving the production hook callback (session.ApplyHook) for both
// events is the point: it is the daemon-side identity-migration path
// the /clear fix lives in, exercised here exactly as CC would trigger
// it. With no SessionID the hook has no env-keyed session to migrate,
// so the rotation still happens on disk but no migration fires.
func (c *CCSim) clear() {
	c.mu.Lock()
	oldConv := c.ConvID
	cwd := c.Cwd
	sessionID := c.SessionID
	newConv := generateConvID()
	c.ConvID = newConv
	c.JsonlPath = filepath.Join(filepath.Dir(c.JsonlPath), newConv+".jsonl")
	// /clear wipes the conversation; the fresh one has no custom title
	// until the agent renames it again.
	c.title = ""
	c.mu.Unlock()

	// SessionEnd(clear) on the OLD conv-id — not an exit, the process
	// lives on.
	_ = session.ApplyHook(session.HookCallbackInput{
		ConvID:        oldConv,
		HookEventName: "SessionEnd",
		Reason:        "clear",
		Cwd:           cwd,
	}, sessionID)

	// Materialise the new conv's .jsonl with an initial summary turn,
	// exactly as a fresh CC session would, so production read paths
	// (ScanAndUpsertFile / FreshConvRow) have a file to scan.
	_ = c.AppendTurn(map[string]any{
		"type":      "summary",
		"summary":   "session " + newConv,
		"sessionId": newConv,
		"timestamp": now(),
	})

	// SessionStart on the NEW conv-id — the first hook carrying the
	// rotated conv-id, where the daemon's identity migration triggers.
	// source=clear is what real CC sends; the foreign-process guard
	// keys on it to tell this announced transition apart from a
	// different claude process's hooks.
	_ = session.ApplyHook(session.HookCallbackInput{
		ConvID:        newConv,
		HookEventName: "SessionStart",
		Source:        "clear",
		Cwd:           cwd,
	}, sessionID)
}

// RunForeignOneShot models a one-shot headless claude invocation
// (`claude -p`, `claude mcp get`, …) launched from inside this
// session's pane — e.g. by the agent's own Bash tool. The child
// process inherits TCLAUDE_SESSION_ID, so its hooks land on the
// PARENT's session row, but they carry the child's own throwaway
// conv-id. The child fires SessionEnd(reason=other) on exit (observed
// against real CC 2.1.170: `claude mcp get` fires only SessionEnd, no
// SessionStart). It is NOT the parent exiting, and it must not be read
// as a /clear-style conv rotation either — production bite: agentd's
// per-minute plugin probes retired live agents as "superseded by
// <probe-conv> (clear)" and fired an "Exited" notification per run.
//
// Returns the child's conv-id so tests can assert nothing latched
// onto it.
func (c *CCSim) RunForeignOneShot() string {
	c.mu.Lock()
	cwd := c.Cwd
	sessionID := c.SessionID
	c.mu.Unlock()

	foreignConv := generateConvID()
	_ = session.ApplyHook(session.HookCallbackInput{
		ConvID:        foreignConv,
		HookEventName: "SessionEnd",
		Reason:        "other",
		Cwd:           cwd,
	}, sessionID)
	return foreignConv
}

// OnInput registers a handler. Newer registrations win over older
// ones when their prefix matches a submitted line. Empty prefix
// matches every input — use it as a custom catch-all (which then
// shadows the default user-turn fallback).
func (c *CCSim) OnInput(prefix string, h InputHandler) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.handlers = append([]handlerEntry{{prefix: prefix, fn: h}}, c.handlers...)
}

// SetCommandDelay configures the wait between an Enter that matches
// `prefix` arriving and the handler firing. Models "CC's input box
// is busy for ~Nms after this command settles" — the asynchrony
// catches prod bugs that assume send-keys success ⇒ turn-on-disk.
//
// Newer SetCommandDelay calls win on prefix match (same first-wins
// rule as OnInput). Pass d=0 to clear a previously-set delay.
func (c *CCSim) SetCommandDelay(prefix string, d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.delays = append([]delayEntry{{prefix: prefix, d: d}}, c.delays...)
}

func (c *CCSim) installDefaultHandlers() {
	// Order matters: dispatch walks the slice top-to-bottom and the
	// first matching prefix wins. Specific handlers up front; the
	// empty-prefix catch-all stays at the end so it only fires when
	// nothing else matches. OnInput prepends, so test-registered
	// custom handlers shadow the defaults naturally.
	c.handlers = []handlerEntry{
		{prefix: "/rename ", fn: func(c *CCSim, line string) bool {
			_ = c.WriteCustomTitle(strings.TrimSpace(strings.TrimPrefix(line, "/rename ")))
			return true
		}},
		{prefix: "/exit", fn: func(c *CCSim, line string) bool {
			_ = c.WriteUserTurn("[/exit]")
			c.MarkDead()
			return true
		}},
		{prefix: "/compact", fn: func(c *CCSim, line string) bool {
			_ = c.WriteSummary("post-compact " + c.ConvID)
			return true
		}},
		{prefix: "/clear", fn: func(c *CCSim, _ string) bool {
			c.clear()
			return true
		}},
		{prefix: "/remote-control", fn: func(c *CCSim, _ string) bool {
			c.mu.Lock()
			if c.remoteOn {
				// Toggling while ON opens the disconnect-confirm menu
				// (default highlight = "keep connected") rather than
				// flipping immediately; the caller must drive it.
				c.rcMenuOpen = true
				c.rcMenuPos = 0
			} else {
				// Toggling while OFF turns remote access on, no confirm.
				c.remoteOn = true
			}
			c.mu.Unlock()
			return true
		}},
		{prefix: "", fn: func(c *CCSim, line string) bool {
			_ = c.WriteUserTurn(line)
			return true
		}},
	}
}

// dispatch walks the handler list (newest first), invokes the first
// matching prefix's handler, and stops on a true return. Lock-free
// after a snapshot of the handler list.
func (c *CCSim) dispatch(line string) {
	c.mu.Lock()
	if !c.alive {
		c.mu.Unlock()
		return
	}
	hs := make([]handlerEntry, len(c.handlers))
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

// delayForLocked returns the configured delay for line. Caller holds c.mu.
func (c *CCSim) delayForLocked(line string) time.Duration {
	for _, d := range c.delays {
		if d.prefix == "" || strings.HasPrefix(line, d.prefix) {
			return d.d
		}
	}
	return 0
}

func (c *CCSim) appendLineLocked(obj map[string]any) error {
	b, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(c.JsonlPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = f.Write(append(b, '\n'))
	return err
}

// HydrateCCSim builds a CCSim against an existing on-disk .jsonl.
// Used by SpawnResume when the in-memory CCSim was already shut down
// (or never existed in this test run). Reads the latest customTitle
// off disk so Title() still answers correctly post-resume.
func HydrateCCSim(t *testing.T, home, convID, cwd string) *CCSim {
	t.Helper()
	cc := NewCCSimWithID(t, home, convID, cwd)
	cc.title = readLatestTitle(cc.JsonlPath)
	return cc
}

// readLatestTitle scans a .jsonl for the most recent custom-title
// turn. Returns "" if the file doesn't exist or has no title turn.
func readLatestTitle(jsonlPath string) string {
	data, err := os.ReadFile(jsonlPath)
	if err != nil {
		return ""
	}
	title := ""
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" {
			continue
		}
		var msg struct {
			Type        string `json:"type"`
			CustomTitle string `json:"customTitle"`
		}
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			continue
		}
		if msg.Type == "custom-title" && msg.CustomTitle != "" {
			title = msg.CustomTitle
		}
	}
	return title
}

// generateConvID produces a UUID-shaped 36-char string. Production
// uses real UUIDs; we just need uniqueness + a length that
// ScanAndUpsertFile (which gates on len==36) accepts.
func generateConvID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hex.EncodeToString(b[0:4]),
		hex.EncodeToString(b[4:6]),
		hex.EncodeToString(b[6:8]),
		hex.EncodeToString(b[8:10]),
		hex.EncodeToString(b[10:16]),
	)
}

// generateResumeLabel produces an "rsme-XXXXXX" identifier for a
// resume's new SessionRow / tmux name. Mirrors generateSpawnLabel's
// shape on the spawn side.
func generateResumeLabel() string {
	var b [3]byte
	_, _ = rand.Read(b[:])
	return "rsme-" + hex.EncodeToString(b[:])
}

func now() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}
