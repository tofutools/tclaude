package testharness

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/harness/copilotfixture"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// CopilotSim is a behaviour-accurate simulator of one GitHub Copilot CLI
// pane — the third PaneSim, alongside CCSim and CodexSim.
//
// # Why this one is shaped differently
//
// CCSim and CodexSim exist so a read path has a faithful file to read. This
// one exists for that too — it owns a real
// <COPILOT_HOME>/session-state/<id>/{workspace.yaml,events.jsonl} pair, which
// is what tclaude's Copilot ConvStore, telemetry follower and context refresh
// read — but its load-bearing job is different, and it is the reason TCL-973
// is hard: a detached Copilot pane can be launched into a state where it never
// does anything again, and looks from the outside exactly like an agent that
// is thinking. Modelling only the happy path would make every guard tclaude
// writes against that untestable, because a permissive stub answers "fine" to
// launches production must refuse.
//
// So this simulator BLOCKS. Every gate below reproduces a measurement from the
// permission contract committed with PR #1936
// (pkg/claude/harness/copilotfixture/testdata/1.0.77/permission_contract.json),
// and each is annotated with the contract entry it comes from. Nothing here is
// modelled from GitHub's documentation or from the TCL-973 plan — where those
// two disagreed with the binary, the binary won, and in one case (the plan's
// proposed `--deny-tool 'url()'` default) it won by killing the launch.
//
// # What is deliberately NOT modelled
//
//   - web_fetch. The contract records the shell path's URL behaviour and
//     records, at length, that COPILOT_OFFLINE=true removes web_fetch from the
//     catalog so the suite structurally could not ask about it. A tool call
//     naming it therefore fails the test loudly rather than being answered by
//     a guess (see RequestTool).
//   - Whether a rule pattern is ENFORCED as opposed to parsed. The contract
//     records that the two come apart for URL rules — `url(*)` parses and
//     matches nothing — so the gate model reads only tool-kind deny rules as
//     working denies and treats a URL deny as unmodelled.
//   - The safe-command allowlist. `echo` was measured auto-approving and four
//     other commands blocking, and the contract states plainly that the list is
//     not enumerated and that "ANY command may block unless a permission flag
//     says otherwise". CopilotToolCall.AutoApproved is therefore a fact the
//     CALLER asserts about its own scripted command, defaulting to "blocks",
//     rather than a table this simulator invents.
//   - The relative precedence of the trust / path / URL / tool dialogs beyond
//     trust coming first. Trust is measured as the first gate (zero provider
//     requests); the other three were each measured in isolation. blockReason
//     reports them in a fixed order and says so.
type CopilotSim struct {
	// ConvID is the session id — the session-state directory name, the value
	// `--session-id` presets and `--resume=` matches.
	ConvID string
	// Cwd is the pane's working directory, which is also the root of Copilot's
	// default path grant.
	Cwd string
	// Home is COPILOT_HOME: where config.json (folder trust) and session-state
	// live.
	Home string
	// SessionID is tclaude's own session label (TCLAUDE_SESSION_ID), the key
	// production hook application is scoped to.
	SessionID string

	// Model and CliVersion stamp the event log's session.start line, which is
	// where the ConvStore reads a conversation's model from.
	Model      string
	CliVersion string

	t *testing.T

	mu sync.Mutex

	launch   CopilotLaunch
	launched bool
	alive    bool

	// blocked/blockedBy record the parked state. Once set they are never
	// cleared: nothing but a human at the keyboard clears a Copilot dialog,
	// and reproducing that faithfully is the whole point — a simulator that
	// timed out of a block would let a deadlocking posture pass its test.
	blocked   bool
	blockedBy string

	// inPaneAllowAll records that /allow-all was accepted in the pane. It
	// widens nothing that a launch-time deny covers; see RequestTool.
	inPaneAllowAll bool

	title     string
	userNamed bool
	turnOpen  bool
	// hookLaunchSeq counts launches so the replayed capture uses the FRESH
	// SessionStart payload for a first launch and the RESUMED one afterwards.
	hookLaunchSeq int
	userMessages  int
	outputTokens  int64
	buf           strings.Builder
	capture       copilotfixture.HookCapture
	createdAt     time.Time
}

var (
	_ PaneSim      = (*CopilotSim)(nil)
	_ paneRenderer = (*CopilotSim)(nil)
)

// CopilotToolKind names the permission surface a tool call touches. It exists
// because Copilot's gates are independent axes rather than one policy: the
// contract measured four separate dialogs, and closing one says nothing about
// the others.
type CopilotToolKind string

const (
	// CopilotToolShell is a shell command — the tool-approval axis, plus the
	// path axis when it names a path and the URL axis when it reaches one.
	CopilotToolShell CopilotToolKind = "shell"
	// CopilotToolAskUser is the ask_user tool: the second deadlock source,
	// removable only by --no-ask-user.
	CopilotToolAskUser CopilotToolKind = "ask_user"
)

// CopilotToolCall is one tool call a scripted turn makes.
type CopilotToolCall struct {
	// Kind selects which gates apply.
	Kind CopilotToolKind
	// Name is the tool name as the deny rules see it ("shell", an MCP server
	// name). Empty defaults to the kind.
	Name string
	// Command is the shell command text, used only to report what blocked.
	Command string
	// AutoApproved asserts that 1.0.77 classifies this particular command as
	// trivially safe and runs it with no permission flag at all — the measured
	// `echo` case. The default (false) is the contract's own safe reading:
	// assume it blocks.
	AutoApproved bool
	// Path, when set, is an absolute path the call touches. It is checked
	// against the launch's path grants.
	Path string
	// URL, when set, is a URL the SHELL command reaches (curl/wget). The
	// web-fetch tool's URL behaviour is unmeasured and unmodelled.
	URL string
}

// CopilotToolOutcome is what happened to a tool call, in the vocabulary the
// Phase 0 classifier established.
type CopilotToolOutcome string

const (
	// CopilotToolAllowed: the tool ran.
	CopilotToolAllowed CopilotToolOutcome = "allowed"
	// CopilotToolDenied: refused WITHOUT a prompt, the refusal posted back as
	// the tool result, and the turn continues. A denial is not a deadlock.
	CopilotToolDenied CopilotToolOutcome = "denied"
	// CopilotToolBlocked: parked on a dialog. The pane stays alive and the
	// turn never ends — no Stop, ever.
	CopilotToolBlocked CopilotToolOutcome = "blocked"
)

// NewCopilotSim builds a simulator for a launch command string, which is
// normally the output of the REAL harness spawner (see the Copilot branch of
// simSpawner). A command the CLI would reject is not an error here — it is a
// pane that dies at launch, which is what the caller must be able to observe,
// so the parse failure is reported and the sim is left dead.
func NewCopilotSim(t *testing.T, home, cwd, launchCmd string) (*CopilotSim, error) {
	t.Helper()
	launch, err := ParseCopilotLaunch(launchCmd)
	if err != nil {
		return nil, err
	}
	convID := launch.SessionID
	if convID == "" {
		convID = launch.ResumeID
	}
	if convID == "" {
		// A launch with neither is a human `copilot` invocation, which mints
		// its own id. tclaude never does this on a daemon path.
		convID = generateConvID()
	}
	if cwd == "" {
		cwd = filepath.Join(home, "copilot-sim-cwd")
		_ = os.MkdirAll(cwd, 0o755)
	}
	c := &CopilotSim{
		ConvID:     convID,
		Cwd:        cwd,
		Home:       home,
		Model:      launch.Model,
		CliVersion: copilotfixture.PinnedCLIVersion,
		t:          t,
		launch:     launch,
		capture:    copilotfixture.LoadHookCapture(t, copilotfixture.HookScenarioClaudeDialect),
		createdAt:  time.Now().UTC(),
	}
	if c.Model == "" {
		c.Model = "claude-sonnet-4.5"
	}
	if launch.Name != "" {
		c.title = launch.Name
		c.userNamed = true
	}
	t.Cleanup(c.Shutdown)
	return c, nil
}

// Launch exposes the parsed launch so a test can assert on the argv the
// production spawner actually produced.
func (c *CopilotSim) Launch() CopilotLaunch {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.launch
}

// TrustCopilotFolder pre-grants folder trust for dir by writing Copilot's
// config.json under home, and must be called BEFORE Start.
//
// This helper is the single most consequential thing PR #1936 measured, so it
// is reproduced here rather than approximated. Contract entry `folder-trust`:
// with a fresh COPILOT_HOME the trust dialog is the FIRST gate, before the
// provider is contacted at all, and NO launch flag clears it —
// --allow-all-tools, --allow-all, --allow-all-paths and --add-dir <workdir>
// were each measured still blocking with zero provider requests. The only
// argv-free bypass is this config write.
//
// The consequence for tclaude, which this simulator makes testable: a Copilot
// agent cannot be spawned detached by rendering argv alone. Something has to
// write this file first, and that is a config-FILE contract with its own
// review surface (it pre-answers a human trust decision), not an approval
// token.
func TrustCopilotFolder(t *testing.T, home, dir string) {
	t.Helper()
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatalf("copilot sim: mkdir %s: %v", home, err)
	}
	path := filepath.Join(home, "config.json")
	enc, err := json.Marshal(map[string]any{"trustedFolders": []string{dir}})
	if err != nil {
		t.Fatalf("copilot sim: marshal trustedFolders: %v", err)
	}
	if err := os.WriteFile(path, enc, 0o600); err != nil {
		t.Fatalf("copilot sim: write %s: %v", path, err)
	}
}

// folderTrusted reports whether this launch clears the first gate.
//
// Two ways, both measured. A trustedFolders entry naming the cwd, written
// before launch; or COPILOT_ALLOW_ALL=true in the environment, which the
// contract found to be strictly stronger than the --allow-all-tools flag it
// documents — it clears trust as well, silently promoting the whole session.
// The second is modelled precisely so a test can prove tclaude UNSETS the
// variable rather than relying on it.
func (c *CopilotSim) folderTrusted() bool {
	if c.launch.AmbientAllowAll() {
		return true
	}
	raw, err := os.ReadFile(filepath.Join(c.Home, "config.json"))
	if err != nil {
		return false
	}
	var cfg struct {
		TrustedFolders []string `json:"trustedFolders"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return false
	}
	return slices.Contains(cfg.TrustedFolders, c.Cwd)
}

// Start boots the pane: it materialises the session-state files and evaluates
// the folder-trust gate.
//
// An untrusted launch still returns nil and still leaves an ALIVE pane. That
// is the deadlock shape and the reason this method cannot simply fail: a real
// untrusted pane is running, its process is healthy, tmux reports it fine, and
// it will never do anything. It also never fires a hook — the gate precedes
// the provider connection entirely — so the session-state files are not
// written either.
func (c *CopilotSim) Start() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.alive {
		return nil
	}
	if !c.folderTrusted() {
		c.alive = true
		c.launched = true
		c.blocked = true
		c.blockedBy = copilotfixture.TrustPromptMarker
		return nil
	}
	if err := c.writeWorkspaceLocked(); err != nil {
		return err
	}
	event := "session.start"
	if c.launched {
		event = "session.resume"
	}
	if err := c.appendEventLocked(map[string]any{
		"type": event,
		"data": map[string]any{
			"sessionId":      c.ConvID,
			"copilotVersion": c.CliVersion,
			"selectedModel":  c.Model,
		},
	}); err != nil {
		return err
	}
	c.alive = true
	c.launched = true
	c.hookLaunchSeq++
	return nil
}

// StartTurn submits a prompt and fires the hooks Copilot fires for one, in the
// order the recorded capture proves it uses: UserPromptSubmit BEFORE
// SessionStart. That order is not a detail — it is the opposite of every other
// harness, and tclaude's status machine had to grow SessionStartAfterPrompt
// for it.
//
// A blocked pane silently swallows the prompt, because that is what a pane
// sitting on a modal dialog does.
func (c *CopilotSim) StartTurn(prompt string) {
	c.mu.Lock()
	if !c.alive || c.blocked {
		c.mu.Unlock()
		return
	}
	c.userMessages++
	seq := c.hookLaunchSeq
	_ = c.appendEventLocked(map[string]any{
		"type": "user.message",
		"data": map[string]any{"content": prompt},
	})
	c.turnOpen = true
	c.mu.Unlock()

	c.applyHook("UserPromptSubmit", seq-1)
	c.applyHook("SessionStart", seq-1)
}

// SubmitLaunchPrompt runs the `-i <prompt>` first turn, if the launch carried
// one. `-i` starts an interactive session and AUTOMATICALLY executes the
// prompt, so a launch-enrolled pane is working before anyone types into it.
//
// Contract entry `resume-submits-prompt` is what lets this fire on a relaunch
// too: measured two-phase against the real binary, `-i` combined with
// `--resume=<full-id>` submits into the RESUMED conversation (the request
// carried [system, user, assistant, user], where a fork would have sent
// [system, user]) and the session-state directory kept its original UUID. That
// retired the standing "unverified" comment in copilot_spawner.go, and it is
// why a relaunch briefing does not vanish silently.
//
// The caller invokes it AFTER the session row exists and the pane is
// registered, mirroring production's ordering — the row is written before the
// tmux session is created, and hooks land on that row.
func (c *CopilotSim) SubmitLaunchPrompt() {
	c.mu.Lock()
	prompt := c.launch.InitialPrompt
	c.mu.Unlock()
	if prompt == "" {
		return
	}
	c.StartTurn(prompt)
}

// RequestTool runs one tool call through the permission gates and returns what
// 1.0.77 was measured to do with it.
//
// A blocked call parks the pane permanently: no further turn ever ends, and
// FinishTurn will refuse to emit Stop. That single property is what turns "the
// resolved posture prevents deadlock" from a claim into a regression test.
func (c *CopilotSim) RequestTool(call CopilotToolCall) CopilotToolOutcome {
	c.t.Helper()
	if call.Kind == "" {
		c.t.Fatalf("copilot sim: tool call has no Kind")
	}
	name := call.Name
	if name == "" {
		name = string(call.Kind)
	}
	if name == "web_fetch" {
		// Guardrail, and the contract is explicit about why: COPILOT_OFFLINE
		// removes web_fetch from the catalog, so no committed scenario has ever
		// observed whether it is gated with tool approval or independently of
		// it. Answering here would manufacture evidence.
		c.t.Fatalf("copilot sim: web_fetch's permission behaviour is UNMEASURED " +
			"(permission_contract.json entry url-access records that " +
			"COPILOT_OFFLINE removes it from the catalog). Model it only once a " +
			"fixture establishes it.")
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.alive {
		c.t.Fatalf("copilot sim: tool call on a dead pane")
	}
	if c.blocked {
		return CopilotToolBlocked
	}

	// 1) Deny rules first, and they are absolute. Contract entry
	//    `in-pane-allow-all-override`: a launch-time --deny-tool survived an
	//    in-pane /allow-all that the pane confirmed with "All permissions are
	//    now enabled", and the tool was still refused. Denial precedence holds
	//    at RUNTIME, not merely at launch — which is why c.inPaneAllowAll is
	//    read after this arm and not before it.
	if rule, ok := c.denyRuleFor(name, call.Command); ok {
		_ = c.appendEventLocked(map[string]any{
			"type": "tool.result",
			"data": map[string]any{
				"tool":    name,
				"content": "Permission to run this tool was denied (" + rule + ")",
			},
		})
		return CopilotToolDenied
	}

	if reason := c.blockReasonLocked(call); reason != "" {
		c.blocked = true
		c.blockedBy = reason
		return CopilotToolBlocked
	}

	c.outputTokens += 20
	_ = c.appendEventLocked(map[string]any{
		"type": "assistant.message",
		"data": map[string]any{"model": c.Model, "outputTokens": int64(20)},
	})
	c.applyHookLocked("PostToolUse", c.hookLaunchSeq-1)
	return CopilotToolAllowed
}

// blockReasonLocked returns the dialog this call parks on, or "".
//
// The ORDER of these arms is not itself measured — each gate was measured in
// isolation, with the others cleared — so it decides only which dialog gets
// named when more than one would fire. What is measured is each arm's own
// condition, and each cites the contract entry that established it.
func (c *CopilotSim) blockReasonLocked(call CopilotToolCall) string {
	// ask_user. Contract entry `no-ask-user`: the tool is advertised by
	// default and --no-ask-user removes it (measured on the advertised tool
	// catalog in the provider request, not on screen text). Present, it is a
	// question nobody is there to answer.
	if call.Kind == CopilotToolAskUser {
		if c.launch.NoAskUser {
			c.t.Fatalf("copilot sim: this launch passed --no-ask-user, so ask_user " +
				"is not in the tool catalog and the model cannot call it")
		}
		return "ask_user (no human is attached to answer it)"
	}

	// Paths. Contract entry `out-of-cwd-paths`: a read outside every granted
	// root blocks on its own "Allow directory access" dialog; --add-dir and
	// --allow-all-paths each clear it; the system temp dir is granted by
	// default and --disallow-temp-dir removes that grant. The internal control
	// there — the same `cat` allowed inside temp and blocked outside it — is
	// what proves the block is path-driven rather than command-risk-driven.
	if call.Path != "" && !c.pathGranted(call.Path) {
		return copilotfixture.PathPromptMarker + ": " + call.Path
	}

	// URLs reached by the SHELL tool. Contract entry `url-access`, which
	// corrected the TCL-973 plan in the plan's own favour: the URL dialog is
	// real and distinct, AND --allow-all-tools closes it, so for the shell path
	// there is no second deadlock to close and no URL deny is needed.
	if call.URL != "" && !c.launch.ToolsAutoApproved() && !c.inPaneAllowAll {
		return "Copilot is attempting to access the following URL: " + call.URL
	}

	// Tool approval. Contract entry `default-interactive-blocking`: an unsafe
	// command blocks with no flags and completes with --allow-all-tools.
	if !call.AutoApproved && !c.launch.ToolsAutoApproved() && !c.inPaneAllowAll {
		return "Allow command? " + call.Command
	}
	return ""
}

// pathGranted models the default cwd-subtree + system-temp grant and the two
// flags that move it.
func (c *CopilotSim) pathGranted(path string) bool {
	if c.launch.AllowAllPaths || c.launch.AmbientAllowAll() {
		return true
	}
	if underDir(path, c.Cwd) {
		return true
	}
	if !c.launch.DisallowTempDir && underDir(path, os.TempDir()) {
		return true
	}
	for _, dir := range c.launch.AddDirs {
		if underDir(path, dir) {
			return true
		}
	}
	return false
}

func underDir(path, dir string) bool {
	if dir == "" {
		return false
	}
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	return rel == "." || !strings.HasPrefix(rel, "..")
}

// denyRuleFor matches a launch-time --deny-tool rule against a call.
//
// Only TOOL-KIND rules are honoured. The contract records that URL rule
// spellings parse and then match nothing at runtime for the wildcard forms,
// i.e. parse acceptance and enforcement come apart, and that no committed
// scenario establishes a working blanket deny. Reading `url(*)` as a working
// deny here would let a tclaude default that does nothing in production look
// effective in every test.
func (c *CopilotSim) denyRuleFor(name, command string) (string, bool) {
	for _, rule := range c.launch.DenyTools {
		kind, pattern, hasPattern := strings.Cut(strings.TrimSuffix(rule, ")"), "(")
		if kind == "url" {
			continue
		}
		// The rule's kind names the tool: `shell(...)` matches the shell tool,
		// `<mcp>(...)` an MCP server's tool. A shell call's default Name is
		// already "shell" (CopilotToolShell), so one comparison covers both.
		if kind != name {
			continue
		}
		if !hasPattern || pattern == "*" {
			return rule, true
		}
		if command != "" && strings.Contains(command, pattern) {
			return rule, true
		}
	}
	return "", false
}

// FinishTurn ends the turn with Stop — unless the pane is blocked, in which
// case it emits NOTHING.
//
// This is the assertion the whole simulator is built around. tclaude's status
// machine returns an agent to idle on Stop, so a simulator that emitted Stop
// from a parked pane would report a permanently deadlocked agent as free, and
// every test of the nonblocking posture would pass whether or not the posture
// worked.
func (c *CopilotSim) FinishTurn() {
	c.mu.Lock()
	if !c.alive || c.blocked || !c.turnOpen {
		c.mu.Unlock()
		return
	}
	c.turnOpen = false
	seq := c.hookLaunchSeq
	_ = c.appendEventLocked(map[string]any{
		"type": "assistant.message",
		"data": map[string]any{"model": c.Model, "outputTokens": int64(30)},
	})
	c.outputTokens += 30
	c.mu.Unlock()
	c.applyHook("Stop", seq-1)
}

// Blocked reports whether the pane is parked on a dialog, and on which.
func (c *CopilotSim) Blocked() (bool, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.blocked, c.blockedBy
}

// Receive is the tmux send-keys entry point. Text accumulates; "Enter"
// submits.
func (c *CopilotSim) Receive(text string) {
	if text == "Enter" {
		c.mu.Lock()
		line := strings.TrimSpace(c.buf.String())
		c.buf.Reset()
		c.mu.Unlock()
		if line != "" {
			c.submit(line)
		}
		return
	}
	c.mu.Lock()
	c.buf.WriteString(text)
	c.mu.Unlock()
}

// submit dispatches one submitted line: the in-pane commands tclaude's
// lifecycle types into the pane, then everything else as a prompt.
func (c *CopilotSim) submit(line string) {
	switch {
	case strings.HasPrefix(line, "/rename"):
		name := strings.TrimSpace(strings.TrimPrefix(line, "/rename"))
		if name == "" {
			return
		}
		c.mu.Lock()
		c.title = name
		c.userNamed = true
		err := c.writeWorkspaceLocked()
		c.mu.Unlock()
		if err != nil {
			c.t.Errorf("copilot sim: /rename: %v", err)
		}
	case strings.HasPrefix(line, "/compact"):
		// A compaction is Copilot's one authoritative context disclosure — the
		// only event that puts a window size and a percentage in the durable
		// log. See copilot_context_refresh.go.
		c.mu.Lock()
		err := c.appendEventLocked(map[string]any{
			"type": "session.compaction_start",
			"data": map[string]any{
				"currentTokens": 64000, "tokenLimit": 128000, "trigger": "manual",
			},
		})
		c.mu.Unlock()
		if err != nil {
			c.t.Errorf("copilot sim: /compact: %v", err)
		}
	case line == "/exit":
		c.exit()
	case line == "/allow-all":
		// Measured: the pane accepts the widening and confirms "All permissions
		// are now enabled" — and a launch-time deny still refuses afterwards.
		c.mu.Lock()
		c.inPaneAllowAll = true
		c.mu.Unlock()
	case strings.HasPrefix(line, "/remote-control"):
		// Copilot has no such command; its remote access is the DIRECTIONAL
		// `/remote [on|off]`, which is exactly why copilotLifecycle leaves
		// RemoteControlCommand empty. A pane receiving this would show an
		// unknown-command error, so the simulator fails the test that sent it.
		c.t.Errorf("copilot sim: Copilot has no /remote-control command " +
			"(its remote access is the directional /remote [on|off]); tclaude " +
			"must not inject a toggle it cannot express")
	default:
		c.StartTurn(line)
	}
}

// exit models `/exit`: the CLI closes the session and fires SessionEnd. The
// descriptor marks Copilot's SessionEnd best-effort because it is observed
// only on clean runs and is at-least-once; a killed pane fires nothing, which
// Shutdown reproduces.
func (c *CopilotSim) exit() {
	c.mu.Lock()
	if !c.alive {
		c.mu.Unlock()
		return
	}
	seq := c.hookLaunchSeq
	c.alive = false
	c.mu.Unlock()
	c.applyHook("SessionEnd", seq-1)
}

// IsAlive reports pane liveness. A BLOCKED pane is alive — that is the entire
// difficulty this simulator exists to express.
func (c *CopilotSim) IsAlive() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.alive
}

// Shutdown models a hard tmux kill-session: the process dies with no
// SessionEnd at all.
func (c *CopilotSim) Shutdown() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.alive = false
}

// RenderPane answers `tmux capture-pane`. A blocked pane shows its dialog,
// which is how an operator (and a future blocked-state detector) would see the
// difference between parked and thinking.
func (c *CopilotSim) RenderPane() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.blocked {
		return c.blockedBy
	}
	return copilotfixture.VersionBanner
}

// applyHook drives the PRODUCTION hook path with a payload the real 1.0.77
// binary wrote, with only the session id and cwd substituted for this pane's.
//
// Replaying recorded bytes rather than building a struct is the same
// discipline copilot_hooks_flow_test.go already follows: if Copilot renames a
// field, a simulator built on struct literals keeps passing while production
// stops working.
func (c *CopilotSim) applyHook(event string, index int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.applyHookLocked(event, index)
}

func (c *CopilotSim) applyHookLocked(event string, index int) {
	payloads := c.capture.FindAll(event)
	if len(payloads) == 0 {
		c.t.Fatalf("copilot sim: no recorded %s payload in scenario %s",
			event, c.capture.Scenario)
	}
	if index < 0 || index >= len(payloads) {
		// Fall back to the last recorded one: the capture holds a fresh and a
		// resumed firing, and a third launch reuses the resumed shape.
		index = len(payloads) - 1
	}
	var in session.HookCallbackInput
	raw := copilotfixture.HookPayloadFor(payloads[index], c.ConvID, c.Cwd)
	if err := json.Unmarshal(raw, &in); err != nil {
		c.t.Fatalf("copilot sim: decoding the recorded %s payload: %v", event, err)
	}
	_ = session.ApplyHook(in, c.SessionID)
}

// writeWorkspaceLocked materialises workspace.yaml — the file tclaude's
// Copilot ConvStore reads identity, cwd and title from.
func (c *CopilotSim) writeWorkspaceLocked() error {
	dir := c.sessionStateDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	stamp := c.createdAt.Format(time.RFC3339)
	body := fmt.Sprintf(
		"id: %s\ncwd: %s\nhost_type: local\nclient_name: copilot-cli\n"+
			"name: %s\nuser_named: %t\nsummary_count: 0\ncreated_at: %s\nupdated_at: %s\n",
		c.ConvID, c.Cwd, c.title, c.userNamed, stamp, time.Now().UTC().Format(time.RFC3339))
	return os.WriteFile(filepath.Join(dir, "workspace.yaml"), []byte(body), 0o600)
}

// appendEventLocked appends one line to events.jsonl. A `--resume` APPENDS to
// the same file rather than starting a new one, which is exactly what Start
// does on a relaunch.
func (c *CopilotSim) appendEventLocked(event map[string]any) error {
	dir := c.sessionStateDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	enc, err := json.Marshal(event)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(filepath.Join(dir, "events.jsonl"),
		os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	_, err = file.Write(append(enc, '\n'))
	return err
}

func (c *CopilotSim) sessionStateDir() string {
	return filepath.Join(c.Home, "session-state", c.ConvID)
}

// CopilotHomeFor returns the COPILOT_HOME a simulated pane uses under a test
// HOME: the documented default, so production's own resolver
// (harness.copilotStateDir) finds it with no environment override.
func CopilotHomeFor(home string) string {
	return filepath.Join(home, ".copilot")
}

// copilotHarnessName is the harness tag the production spawn path threads for
// a Copilot agent. Declared here rather than imported so the constant's
// spelling is asserted once, in copilot_sim_test.go, against
// harness.CopilotName.
const copilotHarnessName = "copilot"

// copilotBuildLaunchCommand renders the REAL production launch string for a
// simulated spawn.
//
// This is the seam that makes the simulator worth having, and also its one
// honest limitation. The spawner (harness.copilotSpawner.BuildCommand) is
// production code and is called here unchanged, so its flag spellings, its
// ordering and — when TCL-973's approval work lands — its rendered permission
// flags all reach the simulator exactly as they reach a real pane.
//
// What is NOT production is the SpawnArgs → SpawnSpec mapping below.
// Production builds that spec deep inside session/new.go, interleaved with
// sandbox resolution and tmux setup that a flow test does not run, so this
// mirrors the fields the Copilot spawner reads rather than reusing the
// original. A field production threads and this does not would be invisible
// here; that is a known gap, not a claim of coverage.
func copilotBuildLaunchCommand(args copilotLaunchArgs) (string, error) {
	h, err := harness.Resolve(copilotHarnessName)
	if err != nil {
		return "", err
	}
	spec := harness.SpawnSpec{
		Cwd:            args.Cwd,
		EnvExports:     args.EnvExports,
		SessionID:      args.SessionID,
		ResumeID:       args.ResumeID,
		Name:           args.Name,
		Model:          args.Model,
		Effort:         args.Effort,
		InitialPrompt:  args.InitialPrompt,
		ApprovalPolicy: args.ApprovalPolicy,
	}
	return h.Spawn.BuildCommand(spec), nil
}

// copilotLaunchArgs is the subset of a spawn the Copilot spawner consumes.
type copilotLaunchArgs struct {
	Cwd            string
	EnvExports     string
	SessionID      string
	ResumeID       string
	Name           string
	Model          string
	Effort         string
	InitialPrompt  string
	ApprovalPolicy string
}
