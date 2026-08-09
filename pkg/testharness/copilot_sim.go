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
// # Two kinds of gap, kept apart on purpose
//
// "Nobody measured this" and "this is measured and we chose not to implement
// it" are different sentences, and collapsing them leaves a stale to-do
// pointing at a fixture that already exists. Every refusal below says which one
// it is: copilotUnmodelledFlag for the first, copilotUnimplementedFlag for the
// second.
//
// MEASURED BUT NOT IMPLEMENTED HERE — the evidence is committed; this
// simulator's model is narrower than the binary:
//
//   - Host-scoped URL denies (`--deny-tool 'url(host)'`, `--deny-url host`).
//     Entry `web-fetch-url-access` measured them enforced at the permission
//     layer, before name resolution. This models only the bare-kind blanket
//     deny, so a host-scoped rule is refused at parse — ignoring one would
//     model a real deny as an allow.
//   - Tool removal (`--excluded-tools web_fetch`). Measured to drop the tool
//     from the catalog, with a call to it answering "Tool 'web_fetch' does not
//     exist" and the turn continuing — the fail-closed option a detached
//     posture wants. This simulator has a fixed catalog and cannot express a
//     removed tool.
//
// UNMEASURED — no committed scenario answers it, so a launch whose gate
// decision would depend on one fails the test rather than being guessed:
//
//   - Whether `--allow-all` / `--yolo`, in-pane `/allow-all`, or ambient
//     COPILOT_ALLOW_ALL open a gate they were never measured against. See
//     requireNoUnmeasuredWidening.
//   - Whether a launch-time URL deny survives post-launch widening from inside
//     the pane. The shell axis has that measurement; the URL axis explicitly
//     does not. See requireLaunchDenyIsMeasured.
//   - Whether `--allow-all-urls` closes the SHELL path's URL prompt. It was
//     measured against web_fetch only.
//   - The safe-command allowlist. A committed scenario measured `echo`
//     auto-approving and `rm -f ./victim` blocking; four further commands are
//     reported blocking by an independent rig whose scenarios are NOT
//     committed. The contract states plainly that the list is not enumerated
//     and that "ANY command may block unless a permission flag says otherwise".
//     CopilotToolCall.AutoApproved is therefore a fact the CALLER asserts about
//     its own scripted command, defaulting to "blocks", rather than a table
//     this simulator invents.
//   - The relative precedence of the trust / path / URL / tool dialogs beyond
//     trust coming first. Trust is measured as the first gate (zero provider
//     requests); the others were each measured in isolation. blockReason
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
	// production hook application is scoped to. Set through SetSessionID: the
	// hook path reads it under the mutex, so writing it bare would be a race
	// the day anything sets it after registration.
	SessionID string

	// Model and CliVersion stamp the event log's session.start line, which is
	// where the ConvStore reads a conversation's model from.
	Model      string
	CliVersion string

	// t is the reporting surface, held as an interface rather than *testing.T
	// so the simulator's own loud-failure guards can be tested.
	//
	// That is not a convenience: requireNoUnmeasuredWidening and
	// requireLaunchDenyIsMeasured are the mechanism that stops this simulator
	// from answering questions the fixtures never asked, and a guard nobody can
	// exercise is a guard nobody knows still fires. A real *testing.T aborts
	// the goroutine on Fatalf, so a test asserting "the guard fired" needs a
	// recorder in its place.
	t copilotSimT

	mu sync.Mutex

	launch   CopilotLaunch
	launched bool
	alive    bool

	// blocked/blockedBy record the parked state. NOTHING clears it on its
	// own: a Copilot dialog is dismissed only by a human at the keyboard or
	// by the measured C-c abort (see cancel, which excepts the trust prompt),
	// and reproducing that faithfully is the whole point — a simulator that
	// timed out of a block would let a deadlocking posture pass its test.
	blocked   bool
	blockedBy string
	// cancels counts C-c keystrokes the pane received.
	cancels int
	// ccArmed models 1.0.78's "ctrl+c again to exit": a C-c that reached an
	// idle, unparked pane with nothing to cancel arms it, and the next C-c
	// while armed exits the CLI cleanly (measured: status 0, window ~1.2 s).
	// The simulator is untimed, so the armed state persists until the next
	// keystroke instead of expiring — consecutive C-c is what production's
	// signal exit sends, and any other input disarms exactly like the real
	// TUI's.
	ccArmed bool

	// inPaneAllowAll records that /allow-all was accepted in the pane. It
	// widens nothing that a launch-time deny covers; see RequestTool.
	inPaneAllowAll bool

	title     string
	userNamed bool
	turnOpen  bool
	// hookLaunchSeq counts launches so the replayed capture uses the FRESH
	// SessionStart payload for a first launch and the RESUMED one afterwards.
	hookLaunchSeq int
	buf           strings.Builder
	capture       copilotfixture.HookCapture
	createdAt     time.Time
}

var (
	_ PaneSim      = (*CopilotSim)(nil)
	_ paneRenderer = (*CopilotSim)(nil)
)

// copilotSimT is the subset of *testing.T the simulator uses.
type copilotSimT interface {
	Helper()
	Errorf(format string, args ...any)
	Fatalf(format string, args ...any)
	Cleanup(func())
}

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
	// CopilotToolWebFetch is Copilot's built-in web fetch — the THIRD
	// independent deadlock source, and the last one to be measured. Entry
	// `web-fetch-url-access`: with trust granted and no permission flags a
	// web_fetch call blocks on the URL dialog and the turn never ends. It is
	// its own kind rather than a shell call with a URL because the two are
	// different tools reaching the same gate, and the measurement that closed
	// this gap had to drop COPILOT_OFFLINE (which removes web_fetch from the
	// catalog entirely) to ask the question at all.
	CopilotToolWebFetch CopilotToolKind = "web_fetch"
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
	return newCopilotSim(t, copilotfixture.LoadHookCapture(t, copilotfixture.HookScenarioClaudeDialect),
		home, cwd, launchCmd)
}

// newCopilotSim is NewCopilotSim over the narrowed reporting interface, so the
// package's own tests can drive it with a recorder. The hook capture is passed
// in because loading it needs a real *testing.T.
func newCopilotSim(t copilotSimT, capture copilotfixture.HookCapture,
	home, cwd, launchCmd string,
) (*CopilotSim, error) {
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
		// Refused rather than defaulted. The obvious default — somewhere under
		// `home` — would put the workspace inside COPILOT_HOME, which is
		// Copilot's state directory and never a working directory, and would
		// then quietly make the cwd path grant overlap the trust store.
		// Errorf + a usable fallback rather than Fatalf: NewCopilotSim is
		// reached from the daemon's spawn handler goroutine in a flow test,
		// where FailNow hangs the request instead of failing the test.
		t.Errorf("copilot sim: a pane needs an explicit cwd; COPILOT_HOME is " +
			"Copilot's state directory, not a workspace")
		cwd = filepath.Join(home, "unspecified-cwd")
	}
	c := &CopilotSim{
		ConvID:     convID,
		Cwd:        cwd,
		Home:       home,
		Model:      launch.Model,
		CliVersion: copilotfixture.PinnedCLIVersion,
		t:          t,
		launch:     launch,
		capture:    capture,
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

// SetSessionID records tclaude's session label for the pane.
func (c *CopilotSim) SetSessionID(label string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.SessionID = label
}

// Launch exposes the parsed launch so a test can assert on the argv the
// production spawner actually produced.
func (c *CopilotSim) Launch() CopilotLaunch {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.launch
}

// TrustCopilotFolder pre-grants folder trust for dir by calling PRODUCTION's
// Copilot trust editor, and must be called BEFORE Start.
//
// It delegates rather than writing the file itself, and that is the whole
// point: this simulator is what will validate tclaude's seeding, so a test-side
// imitation of the write would let a production editor with a different idea of
// the file's shape pass every test here and park a real pane. Delegating also
// inherits the editor's read-modify-write, so two agents seeding different
// directories compose instead of the second silently un-trusting the first.
//
// Why any of this is needed at all is the most consequential thing PR #1936
// measured. Contract entry `folder-trust`: with a fresh COPILOT_HOME the trust
// dialog is the FIRST gate, before the provider is contacted at all, and NO
// launch flag clears it — --allow-all-tools, --allow-all, --allow-all-paths and
// --add-dir <workdir> were each measured still blocking with zero provider
// requests. A detached Copilot agent cannot be produced by rendering argv
// alone; something has to write this file first.
func TrustCopilotFolder(t *testing.T, home, dir string) {
	t.Helper()
	h, err := harness.Resolve(copilotHarnessName)
	if err != nil {
		t.Fatalf("copilot sim: resolving the Copilot harness: %v", err)
	}
	// The launch's environment, spelled the way production spells it: an
	// explicit COPILOT_HOME plus the user's HOME as the fallback the two
	// fixed-path harnesses use. Passing COPILOT_HOME as the `home` argument
	// would work only because the getenv short-circuits it, and would stop
	// working the day this seeds a launch that does not relocate its home.
	if err := harness.EnsureDirTrustedForLaunch(h, dir,
		func(name string) string {
			if name == harness.CopilotHomeEnvVar {
				return home
			}
			return os.Getenv(name)
		}, os.Getenv("HOME")); err != nil {
		t.Fatalf("copilot sim: pre-trusting %s under %s: %v", dir, home, err)
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
	raw, err := os.ReadFile(filepath.Join(c.Home, harness.CopilotConfigFileName))
	if err != nil {
		return false
	}
	var cfg struct {
		TrustedFolders []string `json:"trustedFolders"`
	}
	// JSONC, not JSON. The contract's `folder-trust` entry closes with the
	// warning that config.json "is JSONC that self-describes as automatically
	// managed, so a naive encoding/json read of an existing file will fail on
	// its comment lines" — and the CLI writes exactly such a header into the
	// file it manages. A simulator that could not read a real operator's
	// config would report a correctly-seeded launch as parked.
	if err := json.Unmarshal(stripCopilotConfigComments(raw), &cfg); err != nil {
		return false
	}
	// Compare on the same spellings production seeds: the cleaned path and, on
	// a platform whose temp or home is a symlink (macOS resolves TMPDIR through
	// /var -> /private/var), its resolved form.
	wanted := []string{filepath.Clean(c.Cwd)}
	if resolved, err := filepath.EvalSymlinks(wanted[0]); err == nil {
		wanted = append(wanted, filepath.Clean(resolved))
	}
	for _, folder := range cfg.TrustedFolders {
		if slices.Contains(wanted, filepath.Clean(strings.TrimSpace(folder))) {
			return true
		}
	}
	return false
}

// stripCopilotConfigComments drops whole-line `//` comments, mirroring what
// production's own reader does to the same file.
func stripCopilotConfigComments(data []byte) []byte {
	lines := strings.Split(string(data), "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		kept = append(kept, line)
	}
	return []byte(strings.Join(kept, "\n"))
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
	// A RELAUNCH is a new process, so it re-evaluates every gate from scratch.
	// The parked state is per-process even though nothing but a human clears a
	// dialog within one: carrying `blocked` across a relaunch would report a
	// conversation as deadlocked forever once its trust had been seeded, which
	// is exactly the transition the DirTrust work exists to make.
	c.blocked = false
	c.blockedBy = ""
	c.turnOpen = false
	if !c.folderTrusted() {
		c.alive = true
		c.blocked = true
		c.blockedBy = copilotfixture.TrustPromptMarker
		// `launched` deliberately stays false: this process never reached the
		// provider, so it wrote no session.start, and the NEXT launch is still
		// this conversation's first real one. Marking it launched here would
		// make that later relaunch append a session.resume to a log with no
		// session.start in it.
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
// settles the question copilot_spawner.go still flags as unverified in its own
// comment, and it is why a relaunch briefing does not vanish silently. (This
// PR touches no production file, so that stale comment is left for the change
// that owns it.)
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
	outcome, seq := c.decideTool(call, name)
	if outcome == CopilotToolAllowed {
		// Outside the lock: see applyHook.
		c.applyHook("PostToolUse", seq)
	}
	return outcome
}

// decideTool is RequestTool's locked half. It is split out so the PostToolUse
// hook fires with the mutex released.
func (c *CopilotSim) decideTool(call CopilotToolCall, name string) (CopilotToolOutcome, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.alive {
		c.t.Fatalf("copilot sim: tool call on a dead pane")
	}
	if c.blocked {
		return CopilotToolBlocked, 0
	}

	// 1) Deny rules first, and they are absolute. Contract entry
	//    `in-pane-allow-all-override`: a launch-time --deny-tool survived an
	//    in-pane /allow-all that the pane confirmed with "All permissions are
	//    now enabled", and the tool was still refused. Denial precedence holds
	//    at RUNTIME, not merely at launch — which is why c.inPaneAllowAll is
	//    read after this arm and not before it.
	if rule, ok := c.denyRuleFor(name, call.Command, call.URL); ok {
		_ = c.appendEventLocked(map[string]any{
			"type": "tool.result",
			"data": map[string]any{
				"tool":    name,
				"content": "Permission to run this tool was denied (" + rule + ")",
			},
		})
		return CopilotToolDenied, 0
	}

	if reason := c.blockReasonLocked(call); reason != "" {
		c.blocked = true
		c.blockedBy = reason
		return CopilotToolBlocked, 0
	}

	_ = c.appendEventLocked(map[string]any{
		"type": "assistant.message",
		"data": map[string]any{"model": c.Model, "outputTokens": int64(20)},
	})
	return CopilotToolAllowed, c.hookLaunchSeq - 1
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
	// No requireNoUnmeasuredWidening here, and the asymmetry is deliberate:
	// --no-ask-user works by REMOVING the tool from the advertised catalog, and
	// no blanket approval flag can approve a call to a tool that is not offered.
	// So unlike the other three gates, there is no plausible reading under which
	// a blanket allow would open this one, and failing loudly would be noise.
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
		c.requireNoUnmeasuredWidening("the path gate", "out-of-cwd-paths")
		return copilotfixture.PathPromptMarker + ": " + call.Path
	}

	// The web-fetch tool. Contract entry `web-fetch-url-access`: it blocks on
	// the URL dialog with no permission flags, and BOTH --allow-all-tools and
	// --allow-all-urls close it independently — the latter is what proves the
	// prompt is a URL decision rather than ordinary tool approval.
	if call.Kind == CopilotToolWebFetch {
		if c.launch.AllowAllTools || c.launch.AllowAllURLs {
			return ""
		}
		c.requireNoUnmeasuredWidening("the web-fetch URL gate", "web-fetch-url-access")
		return "Copilot is attempting to access the following URL: " + call.URL
	}

	// URLs reached by the SHELL tool. Contract entry `url-access`, which
	// corrected the TCL-973 plan in the plan's own favour: the URL dialog is
	// real and distinct, AND --allow-all-tools closes it, so for the shell path
	// there is no second deadlock to close and no URL deny is needed.
	//
	// --allow-all-urls is deliberately NOT read as closing this one. It was
	// measured against web_fetch, and the shell path is a different consumer
	// whose scenarios never included it; the two paths agreeing about
	// --allow-all-tools does not license generalising the other flag across
	// them.
	if call.URL != "" && !c.launch.AllowAllTools {
		c.requireNoUnmeasuredWidening("the shell URL gate", "url-access")
		return "Copilot is attempting to access the following URL: " + call.URL
	}

	// Tool approval. Contract entry `default-interactive-blocking`: an unsafe
	// command blocks with no flags and completes with --allow-all-tools; entry
	// `ambient-allow-all-env` measured COPILOT_ALLOW_ALL executing an unsafe
	// tool call with no flags at all. Both are inputs to ToolsAutoApproved.
	if !call.AutoApproved && !c.launch.ToolsAutoApproved() {
		c.requireNoUnmeasuredWidening("the tool-approval gate", "default-interactive-blocking")
		return "Allow command? " + call.Command
	}
	return ""
}

// requireNoUnmeasuredWidening fails the test when a gate is about to block a
// launch that carries something which MIGHT have opened it, but which no
// committed scenario measured against this gate.
//
// The two cases, both caught by the cold review of this simulator's first
// revision, and both in the permissive direction that matters:
//
//   - `--allow-all` / `--yolo`. Measured only against the folder-trust gate,
//     where they do nothing. Their effect on tool approval, paths and URLs was
//     never measured, so reading them as a blanket approval — which the flag
//     names invite — would have the simulator assert an auto-approval nobody
//     observed.
//   - in-pane `/allow-all`. The `in-pane-allow-all-override` entry measured
//     only that a launch-time DENY survives it, and its own corroborating note
//     says plainly: "NOT measured: … whether the ALLOW posture rather than a
//     deny can be widened in-pane".
//
// Blocking anyway would be the safe direction for a production guard and the
// WRONG one for a simulator: a test asserting "this posture deadlocks" would
// pass on evidence that does not exist. Failing loudly is the only answer that
// cannot be mistaken for a measurement.
func (c *CopilotSim) requireNoUnmeasuredWidening(gate, entry string) {
	c.t.Helper()
	switch {
	case c.launch.BlanketAllow:
		c.t.Fatalf("copilot sim: this launch carries --allow-all/--yolo, whose effect "+
			"on %s is UNMEASURED — permission_contract.json entry %q measured only "+
			"--allow-all-tools, and the folder-trust entry measured --allow-all only "+
			"against the trust modal. Commit a scenario before a test depends on it.",
			gate, entry)
	case c.inPaneAllowAll:
		c.t.Fatalf("copilot sim: this pane ran in-pane /allow-all, whose widening "+
			"effect on %s is UNMEASURED — entry `in-pane-allow-all-override` "+
			"measured only that a launch-time DENY survives it, and says so "+
			"explicitly in its corroborating notes.", gate)
	case c.launch.AllowAllURLs && gate == "the shell URL gate":
		c.t.Fatalf("copilot sim: this launch carries --allow-all-urls, which entry " +
			"`web-fetch-url-access` measured against the WEB-FETCH gate only. Whether " +
			"it also closes the shell path's URL prompt is unmeasured.")
	case c.launch.AmbientAllowAll() && gate != "the tool-approval gate":
		c.t.Fatalf("copilot sim: this launch carries COPILOT_ALLOW_ALL=true, whose "+
			"effect on %s is UNMEASURED — entry `ambient-allow-all-env` measured it "+
			"against the folder-trust modal and an unsafe TOOL call, nothing else.",
			gate)
	}
}

// pathGranted models the default cwd-subtree + system-temp grant and the two
// flags that move it.
func (c *CopilotSim) pathGranted(path string) bool {
	// --allow-all-paths only. The ambient variable's reach beyond the trust
	// modal and a tool call is unmeasured, so it is handled by
	// requireNoUnmeasuredWidening rather than granted here.
	if c.launch.AllowAllPaths {
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
// Every rule reaching here is one the gate model can evaluate: the launch
// parser refuses `url(...)` and `write(...)` outright rather than letting them
// sit in this list doing nothing (see CopilotLaunch.addDenyTool). That refusal
// is what keeps this loop honest — an earlier revision skipped URL rules
// silently here, which modelled a domain-scoped deny as ALLOWED, the one
// direction the contract's own evidence says is wrong.
func (c *CopilotSim) denyRuleFor(name, command, url string) (string, bool) {
	for _, rule := range c.launch.DenyTools {
		kind, pattern, hasPattern := strings.Cut(strings.TrimSuffix(rule, ")"), "(")
		if kind == "url" {
			// Only two URL spellings reach here; the parser refuses the rest.
			// The bare kind is a working blanket deny at the permission layer,
			// measured to beat a launch-time --allow-all-tools; the wildcard
			// forms are measured INERT and must keep matching nothing.
			if hasPattern || url == "" {
				continue
			}
			c.requireLaunchDenyIsMeasured()
			return rule, true
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

// requireLaunchDenyIsMeasured guards the one thing entry
// `web-fetch-url-access` states at a deliberately narrow width: it establishes
// that a launch-time URL deny beats a launch-time blanket ALLOW, and says
// explicitly that whether the deny also survives POST-LAUNCH widening from
// inside the pane is not measured.
//
// The shell axis has that second half (entry `in-pane-allow-all-override`
// measured a launch deny surviving in-pane /allow-all); the URL axis does not,
// and a blanket deny a pane can widen away is a different product from one it
// cannot.
func (c *CopilotSim) requireLaunchDenyIsMeasured() {
	c.t.Helper()
	if c.inPaneAllowAll {
		c.t.Fatalf("copilot sim: this pane ran in-pane /allow-all after a launch-time " +
			"URL deny. Entry `web-fetch-url-access` establishes launch-time precedence " +
			"only and says explicitly that surviving post-launch widening is NOT " +
			"measured for the URL axis.")
	}
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
// submits. Any non-C-c key disarms a pending "ctrl+c again to exit", exactly
// as typing does on the real TUI.
func (c *CopilotSim) Receive(text string) {
	if text == copilotCancelKey {
		c.cancel()
		return
	}
	c.mu.Lock()
	c.ccArmed = false
	c.mu.Unlock()
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

// copilotCancelKey is the tmux key name for the cancel keystroke tclaude sends
// ahead of Copilot's soft exit (harness.copilotLifecycle.SoftExitPrefixKeys).
const copilotCancelKey = "C-c"

// cancel models what the pinned CLIs were measured to do with C-c: the
// pending input line is dropped, a running turn is cancelled ("Operation
// cancelled by user"), and a permission dialog is ABORTED — the request is
// refused, the pane returns to its input prompt, and the command it was
// asking about never runs. A C-c that reached an idle pane with nothing to
// cancel arms 1.0.78's "ctrl+c again to exit" instead, and the next C-c
// while armed exits the CLI — the pair production's signal exit
// (agentd.injectCopilotSignalExitSerializedBy) is built on.
//
// The trust prompt is deliberately NOT cleared, and a trust-parked pane
// never arms: the prompt gates the whole launch rather than one request, no
// measurement claims C-c dismisses it (or exits through it), and a simulator
// that unblocked or exited here would let a launch posture that really does
// deadlock the pane pass its test.
func (c *CopilotSim) cancel() {
	c.mu.Lock()
	c.cancels++
	if c.blocked {
		c.ccArmed = false
		if c.blockedBy != copilotfixture.TrustPromptMarker {
			c.blocked = false
			c.blockedBy = ""
		}
		c.buf.Reset()
		c.mu.Unlock()
		return
	}
	if c.buf.Len() > 0 || c.turnOpen {
		// The press was spent clearing the half-typed line or cancelling the
		// running turn ("Operation cancelled by user"); it does not arm.
		c.buf.Reset()
		c.turnOpen = false
		c.ccArmed = false
		c.mu.Unlock()
		return
	}
	if c.ccArmed {
		c.ccArmed = false
		c.mu.Unlock()
		c.exit()
		return
	}
	c.ccArmed = true
	c.mu.Unlock()
}

// Cancels reports how many cancel keystrokes the pane has received.
func (c *CopilotSim) Cancels() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cancels
}

// submit dispatches one submitted line: the in-pane commands tclaude's
// lifecycle types into the pane, then everything else as a prompt.
//
// A pane that is parked on a dialog — or already dead — SWALLOWS the line and
// does nothing. That guard is not a detail: a Copilot modal owns the keyboard,
// so `/rename`, `/compact` and `/exit` typed into a parked pane all land in the
// dialog's own input and are discarded. Without it the simulator would let a
// deadlocked pane rename its conversation (materialising the very
// session-state file the trust gate proves is never written), append to an
// event log that has no session.start, and — worst — answer tclaude's soft-stop
// with a clean SessionEnd, so a future "detect and recover a deadlocked pane"
// guard would test as working while doing nothing at all.
func (c *CopilotSim) submit(line string) {
	c.mu.Lock()
	swallowed := !c.alive || c.blocked
	c.mu.Unlock()
	if swallowed {
		return
	}
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
// The lock is NEVER held across the call. session.ApplyHook can reach
// paneinput.InjectTextAndSubmit (the clear-migration rename, the context
// nudge), which in a flow test routes straight back through the registered
// tmux simulator into this pane's Receive — and a Receive that had to take a
// lock the hook path was still holding would deadlock the test rather than
// fail it. CCSim releases before calling ApplyHook for the same reason; this
// follows that convention rather than relying on those paths staying disabled
// for Copilot.
func (c *CopilotSim) applyHook(event string, index int) {
	payloads := c.capture.FindAll(event)
	if len(payloads) == 0 {
		// Errorf, not Fatalf: this runs on whatever goroutine drove the turn,
		// which for a daemon-spawned pane is the HTTP handler's. FailNow there
		// would Goexit the handler and hang the request rather than fail the
		// test. Same reasoning as the Errorf in NewCopilotSim.
		c.t.Errorf("copilot sim: no recorded %s payload in scenario %s",
			event, c.capture.Scenario)
		return
	}
	if index < 0 || index >= len(payloads) {
		// Fall back to the last recorded one: the capture holds a fresh and a
		// resumed firing, and a third launch reuses the resumed shape.
		index = len(payloads) - 1
	}
	c.mu.Lock()
	convID, cwd, sessionID := c.ConvID, c.Cwd, c.SessionID
	c.mu.Unlock()

	var in session.HookCallbackInput
	raw := copilotfixture.HookPayloadFor(payloads[index], convID, cwd)
	if err := json.Unmarshal(raw, &in); err != nil {
		c.t.Errorf("copilot sim: decoding the recorded %s payload: %v", event, err)
		return
	}
	_ = session.ApplyHook(in, sessionID)
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
