package agentd

// Terminal UI for `tclaude agentd serve --tui` — a deliberately small
// in-terminal stand-in for the web dashboard's most-used moves: see which
// agents exist, start a new one, go to one's tmux session, take one offline,
// retire one that is done, and drop into a plain shell session. On its own it
// is the whole operator surface (runServe starts no dashboard listener); it
// also runs happily beside the web dashboard when the operator asks for both.
// Either way it is plain text with no color scheme, no theming and no
// per-terminal palette — the dashboard's cosmetic re-skins (--slop, --wizard)
// are the browser's business and never reach here. The only visual state is
// an inverse-video cursor row.
//
// Everything it shows or does about AGENTS goes through the daemon's own /v1
// HTTP API (tuiAPI), not through the DB or the spawn internals, so the TUI
// cannot drift from — or quietly skip — the validation, permission and audit
// paths every other spawn surface runs. The exceptions are the host-local
// moves that have no HTTP shape: attachSelected, which hands this very
// terminal to a tmux session, and everything to do with plain sessions — the
// shell form that starts one, and the listing, switching and killing of the
// non-agent sessions in tui_sessions.go. A session is not an agent (no
// conversation, no group, no permissions), so there is nothing for the agent
// API to describe; those paths read or drive local session state directly and
// are gated on the console being the operator instead.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/table"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// tuiRefreshInterval is how often the console re-polls the daemon. It
// matches the web dashboard's own snapshot cadence.
const tuiRefreshInterval = 2 * time.Second

// startServeTUI runs the terminal console on its own goroutine and returns an
// idempotent stop function that ends it, waits for the terminal to be
// restored, and reports how the console ended — a console that could not run
// at all (no TTY, for instance) is a failed `serve --tui`, so the error is
// carried back to the caller's exit status rather than only logged.
//
// The two directions of shutdown both terminate here: closing quit.ch (a
// signal, the tray's Quit, a dead socket server) cancels the program's
// context, and quitting the console signals quit itself — `agentd serve
// --tui` is a foreground process whose entire face is this screen, so
// leaving it means stopping the daemon.
// tuiStartup is what runServe hands the console at launch: the surfaces that
// came up beside it, and — when the console is where the operator has to read
// it — the operator token.
type tuiStartup struct {
	// dashboardURL is the web dashboard running alongside, empty when --tui
	// asked for no listener.
	dashboardURL string
	// operatorToken is set only when the console has taken over the startup
	// banner (see tokenBannerInTUI); empty means stdout printed it, or that
	// --no-print-human-token suppressed it entirely.
	operatorToken string
	tokenSource   tokenSource
	// suppressSecrets is --no-print-human-token: the operator has said this
	// terminal's output is scraped or logged, so nothing the console can put on
	// the screen may be a credential. It already covers the token banner by
	// leaving operatorToken empty; it covers the dashboard sign-in link too.
	suppressSecrets bool
	// ownsTmuxServer is set when this daemon started the tclaude tmux server
	// itself and will kill it on the way out if it is empty by then (see
	// startTUITmuxServer). It changes what quitting MEANS — that server ends
	// with the console instead of outliving it — so the console has to say so
	// before it acts on q.
	ownsTmuxServer bool
}

func startServeTUI(quit *quitter, startup tuiStartup) func() error {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-quit.ch
		cancel()
	}()

	m := newTUIModel(newInProcessTUIAPI())
	m.dashboardURL = startup.dashboardURL
	m.suppressSecrets = startup.suppressSecrets
	m.ownsTmuxServer = startup.ownsTmuxServer
	// The first screen is drawn before the first tick.
	m = m.refreshDashboardLink(time.Now())
	m.tokenLines = tuiOperatorTokenLines(startup.operatorToken, startup.tokenSource)
	m.showTokenBanner = len(m.tokenLines) > 0

	prog := tea.NewProgram(m, tea.WithContext(ctx))
	done := make(chan struct{})
	var runErr error
	go func() {
		defer close(done)
		if _, err := prog.Run(); err != nil && !tuiEndedNormally(err) {
			slog.Error("tui: terminal UI exited with an error", "error", err)
			fmt.Fprintf(os.Stderr, "terminal UI: %v\n", err)
			runErr = fmt.Errorf("terminal UI: %w", err)
		}
		quit.signal()
	}()

	stopped := false
	return func() error {
		if !stopped {
			stopped = true
			cancel()
			<-done
		}
		return runErr
	}
}

// tuiEndedNormally reports whether err is one of the two ways a console
// legitimately stops: killed from the outside (how the daemon-side shutdown
// path always ends it, via the cancelled context) or interrupted by a signal.
// Neither is a failure of the console, so neither reaches the exit status.
func tuiEndedNormally(err error) bool {
	return errors.Is(err, tea.ErrProgramKilled) || errors.Is(err, tea.ErrInterrupted)
}

// ---- API seam + in-process client -----------------------------------------

// tuiCapabilities names the operations that depend on the console sharing the
// daemon host and process lifetime. API-backed operations (list, spawn, retire,
// stop, resume) need no capability bit: both implementations support them.
type tuiCapabilities struct {
	attachAgent     bool
	attachLocalPane bool
	// startLocalShell is the console's ability to start a plain interactive
	// shell session on the daemon host. Like attachLocalPane it needs the
	// console and the daemon to share a machine and a terminal: the launch
	// creates a tmux session on the daemon's host and then hands this terminal
	// to it, neither of which a remote console can do.
	startLocalShell bool
	// localSessions is the console's ability to see and act on the daemon
	// host's plain (non-agent) tmux sessions. Like the two above it needs the
	// console and the daemon to share a machine: the listing is read straight
	// off the host's session store and tmux server, and switching to a row
	// hands this terminal to a pane on that host.
	localSessions    bool
	completeLocalDir bool
	shutdownOnQuit   bool
}

// tuiAPI is the terminal dashboard's only data/control dependency. The
// in-process implementation below dispatches directly into the daemon mux;
// remoteTUIAPI uses HTTP. Keeping the model on this deliberately small
// interface makes the two transports share all rendering and interaction
// behavior without pretending host-local actions work remotely.
type tuiAPI interface {
	get(path string, out any) error
	post(path string, in, out any) error
	attach(agentName, convID, tmuxSession string) tea.Cmd
	isOperator() bool
	identityWarning() string
	connectionLabel() string
	capabilities() tuiCapabilities
}

// inProcessTUIAPI issues requests against the daemon's own /v1 mux from inside the
// daemon process. The console is a client like any other: it reaches the
// real handlers, with their validation, permission checks and audit trail,
// rather than calling spawn/DB internals directly.
//
// Identity: withIdentity normally resolves the peer from the connecting Unix
// socket, which does not exist for an in-process call, so the console builds
// the peer itself — but it asserts nothing. It presents the live operator
// token and lets the real verifier decide (verifyHumanToken, constant-time),
// and it resolves its own harness ancestry through the same convIDForPID walk
// the socket path uses. classify() then applies its ordinary precedence.
//
// That precedence matters here. A daemon launched from inside a harness pane
// has a harness ancestor, and classify deliberately lets that beat any
// operator token — otherwise an agent whose shell inherited TCLAUDE_HUMAN_TOKEN
// could promote itself. Claiming "the console is always the human" would have
// re-opened exactly that hole through a console the owning agent can drive
// with tmux send-keys. So `agentd serve --tui` started from an agent's pane
// gets an agent-class console, scoped and permission-gated like that agent;
// started from the operator's own shell it gets the human.
type inProcessTUIAPI struct {
	handler http.Handler
	// pid and the resolved ancestry are fixed for the daemon's lifetime, so
	// the process-tree walk runs once here rather than on every 2s poll.
	pid                int
	convID             string
	hasHarnessAncestor bool
}

func newInProcessTUIAPI() *inProcessTUIAPI {
	// A second mux instance: buildMux only registers package-level handlers
	// and holds no per-mux state, so this shares every code path with the
	// socket server without sharing its identity middleware (which would
	// overwrite the peer stamped below).
	pid := os.Getpid()
	convID, hasAncestor := convIDForPID(pid)
	return &inProcessTUIAPI{
		handler:            buildTUIConsoleMux(),
		pid:                pid,
		convID:             convID,
		hasHarnessAncestor: hasAncestor,
	}
}

// callerClass is how the daemon classifies this console — the same verdict
// its requests get, computed once so the UI can say up front what the operator
// is working as.
func (a *inProcessTUIAPI) callerClass() callerClass {
	return classify(&peer{
		PID:               a.pid,
		ConvID:            a.convID,
		HasClaudeAncestor: a.hasHarnessAncestor,
		// The console always presents the live token, so it is valid
		// whenever the daemon managed to mint one.
		HumanTokenValid: currentOperatorToken() != "",
	})
}

// identityWarning explains, in one line, a console the daemon will not treat
// as the operator — empty when it will. Without it, a console started from
// the wrong place just answers every keystroke with a bare 403 and no hint
// about why or what to do instead.
func (a *inProcessTUIAPI) identityWarning() string {
	switch a.callerClass() {
	case classHuman:
		return ""
	case classAgent:
		return "Note: agentd was started from inside a harness session, so this console acts as agent " +
			a.convID + " — listings are scoped to its groups and spawns need its permissions."
	case classAgentUnknown:
		return "Note: agentd was started from inside a harness session whose conv-id cannot be resolved, " +
			"so the daemon refuses this console. Restart agentd from a plain shell to use --tui."
	default:
		return "Note: no operator token is available, so the daemon refuses this console."
	}
}

func (a *inProcessTUIAPI) isOperator() bool {
	return a.callerClass() == classHuman
}

func (a *inProcessTUIAPI) connectionLabel() string {
	return "in-process"
}

func (a *inProcessTUIAPI) capabilities() tuiCapabilities {
	return tuiCapabilities{
		attachAgent:      true,
		attachLocalPane:  true,
		startLocalShell:  true,
		localSessions:    true,
		completeLocalDir: true,
		shutdownOnQuit:   true,
	}
}

func (a *inProcessTUIAPI) attach(agentName, convID, tmuxSession string) tea.Cmd {
	if tmuxSession == "" {
		if sess := pickAliveSession(convID); sess != nil {
			tmuxSession = sess.TmuxSession
		}
	}
	if tmuxSession == "" {
		return func() tea.Msg {
			return tuiAttachedMsg{
				agent: agentName,
				err:   fmt.Errorf("%s has no live tmux session to attach to", agentName),
			}
		}
	}
	return tuiAttachToPane(agentName, tmuxSession, insideTmux())
}

func (a *inProcessTUIAPI) get(path string, out any) error {
	return a.do(http.MethodGet, path, nil, out)
}

func (a *inProcessTUIAPI) post(path string, in, out any) error {
	return a.do(http.MethodPost, path, in, out)
}

func (a *inProcessTUIAPI) do(method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, "http://agentd"+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set(humanTokenHeader, currentOperatorToken())
	req = req.WithContext(context.WithValue(req.Context(), peerKey{}, &peer{
		PID:               a.pid,
		ConvID:            a.convID,
		HasClaudeAncestor: a.hasHarnessAncestor,
		HumanTokenValid:   verifyHumanToken(req),
	}))

	rec := &tuiResponseRecorder{code: http.StatusOK}
	if err := serveTUIRequest(a.handler, rec, req); err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	if rec.code == http.StatusNotFound {
		// Same disposition the remote client gives a 404: the daemon has no
		// such operation, which an optional readout should go quiet about
		// rather than report as a failure.
		return fmt.Errorf("%s %s: %w", method, path, &tuiUnsupportedEndpointError{msg: tuiErrorMessage(rec)})
	}
	if rec.code >= http.StatusBadRequest {
		return fmt.Errorf("%s %s: %s", method, path, tuiErrorMessage(rec))
	}
	if out == nil || rec.body.Len() == 0 {
		return nil
	}
	if err := json.Unmarshal(rec.body.Bytes(), out); err != nil {
		return fmt.Errorf("decode %s response: %w", path, err)
	}
	return nil
}

// serveTUIRequest dispatches one request into the mux and turns a handler
// panic into an error instead of letting it escape.
//
// This restores what the socket path gets for free: net/http recovers a
// panicking handler and loses only that connection. An in-process call has no
// such net, and the console calls this from a bubbletea command goroutine, so
// an unrecovered panic in a handler would take down the whole daemon — and
// leave the terminal in alt-screen/raw mode on the way out. The console polls
// /v1/peers every two seconds, so this is not a corner of the code that is
// rarely reached.
func serveTUIRequest(h http.Handler, w http.ResponseWriter, r *http.Request) (err error) {
	defer func() {
		if rec := recover(); rec != nil {
			// Same disposition net/http gives a panicking handler: log it
			// with the stack, fail this one request, keep serving.
			slog.Error("tui: handler panicked", "path", r.URL.Path, "panic", rec,
				"stack", string(debug.Stack()))
			err = fmt.Errorf("the daemon panicked handling this request: %v", rec)
		}
	}()
	h.ServeHTTP(w, r)
	return nil
}

// tuiErrorMessage renders a failed response for a one-line status bar: the
// daemon's own {"error":…} text when there is one, else the status code.
func tuiErrorMessage(rec *tuiResponseRecorder) string {
	var payload struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.body.Bytes(), &payload); err == nil && strings.TrimSpace(payload.Error) != "" {
		return payload.Error
	}
	return http.StatusText(rec.code)
}

// tuiResponseRecorder is the minimal http.ResponseWriter the in-process
// calls above write into.
type tuiResponseRecorder struct {
	header  http.Header
	code    int
	written bool
	body    bytes.Buffer
}

func (r *tuiResponseRecorder) Header() http.Header {
	if r.header == nil {
		r.header = http.Header{}
	}
	return r.header
}

func (r *tuiResponseRecorder) Write(b []byte) (int, error) {
	r.written = true
	return r.body.Write(b)
}

func (r *tuiResponseRecorder) WriteHeader(code int) {
	if r.written {
		return
	}
	r.written = true
	r.code = code
}

// ---- wire shapes -----------------------------------------------------------

// tuiAgentRow is the subset of /v1/peers the console renders. Kept as its
// own decode shape (rather than reusing peerEntry) so the console reads only
// the fields it actually shows.
type tuiAgentRow struct {
	AgentID    string        `json:"agent_id"`
	ConvID     string        `json:"conv_id"`
	Title      string        `json:"title"`
	Online     bool          `json:"online"`
	Groups     []string      `json:"groups"`
	Branch     string        `json:"branch"`
	StartupDir string        `json:"startup_dir"`
	CurrentDir string        `json:"current_dir"`
	State      tuiAgentState `json:"state"`
}

// tuiAgentState is the activity subset of /v1/peers' state block.
type tuiAgentState struct {
	Harness string `json:"harness"`
	Status  string `json:"status"`
}

// dir is the directory column: where the agent is working now, falling back
// to where it was launched.
func (a tuiAgentRow) dir() string {
	if a.CurrentDir != "" {
		return a.CurrentDir
	}
	return a.StartupDir
}

// name is the display label: the conversation title, falling back to the
// stable agent id for an agent that has not been titled yet.
func (a tuiAgentRow) name() string {
	if t := strings.TrimSpace(a.Title); t != "" {
		return t
	}
	if a.AgentID != "" {
		return a.AgentID
	}
	return a.ConvID
}

// status is the one-word activity summary, with offline taking precedence:
// a stale status from the agent's last live turn would otherwise read as if
// the pane were still up.
func (a tuiAgentRow) status() string {
	if !a.Online {
		return "offline"
	}
	if s := strings.TrimSpace(a.State.Status); s != "" {
		return s
	}
	return "online"
}

// tuiGroupRow is the subset of /v1/groups the console needs: the name is
// what the spawn form picks from and posts to, and the default directory is
// what it starts that group's spawn in.
type tuiGroupRow struct {
	Name string `json:"name"`
	// DefaultCwd is the group's configured spawn directory, "" when it has
	// none — and always "" for a console the daemon does not treat as the
	// human, which is not served the path at all.
	DefaultCwd string `json:"default_cwd,omitempty"`
}

// tuiProfileRow is the subset of /v1/spawn-profiles the spawn form picks
// from. Disabled comes along because a disabled profile is refused at spawn,
// so offering one would only produce a rejection the operator cannot act on
// from here.
//
// AgentName and SyncWorktree are the two profile fields this form has to apply
// ITSELF. Every other one the daemon resolves down its own tier stack, so
// naming the profile is enough — but a worktree is cut before the spawn request
// goes out, from a branch typed into this form, so "name the agent's worktree
// after the agent" has to happen here or not at all.
type tuiProfileRow struct {
	Name     string `json:"name"`
	Disabled *bool  `json:"disabled,omitempty"`
	// AgentName is the profile's display name for the agents it spawns. The
	// daemon applies it to a request that omits `name`; the form prefills it so
	// the operator can see (and extend) the name their worktree branch follows.
	AgentName string `json:"agent_name,omitempty"`
	// SyncWorktree is the profile's "give each agent its own worktree, named
	// after it" toggle — tri-state, nil when the profile says nothing.
	SyncWorktree *bool `json:"sync_worktree,omitempty"`
	// InitialMessage is the profile's task-brief default. The form does not
	// prefill it — a profile's brief may be many lines and the Brief field is a
	// one-line input — so this is read only to SAY that an empty box will be
	// filled by it, rather than leaving the operator to find out afterwards.
	InitialMessage string `json:"initial_message,omitempty"`
}

func (p tuiProfileRow) disabled() bool { return p.Disabled != nil && *p.Disabled }

// syncsWorktree reports whether this profile asks its agents to be spawned into
// a worktree of their own. Unset is not "no": it leaves the picker alone.
func (p tuiProfileRow) syncsWorktree() (bool, bool) {
	if p.SyncWorktree == nil {
		return false, false
	}
	return *p.SyncWorktree, true
}

// tuiRetireResult is the subset of the retire response the console reports:
// what the demotion actually revoked, and what became of the agent's session.
type tuiRetireResult struct {
	Outcome  retireConvOutcome `json:"outcome"`
	Shutdown memberOpResult    `json:"shutdown"`
}

// tuiStopResult is the subset of the stop response the console reports.
type tuiStopResult struct {
	Action string `json:"action"`
	Detail string `json:"detail,omitempty"`
}

// tuiResumeResult is the subset of the resume response the console reports:
// what the daemon did, and — for everything that is not a plain "resumed" —
// why.
type tuiResumeResult struct {
	Action string `json:"action"`
	Detail string `json:"detail,omitempty"`
	// Warnings are the notes a resume that DID land still wants read: an
	// agent that came back with reduced sandbox access, say. They are the one
	// thing a bare "Started X." would otherwise swallow, and this console has
	// no browser to go and find them in.
	Warnings []string `json:"warnings,omitempty"`
}

// ---- model -----------------------------------------------------------------

type tuiMode int

const (
	tuiModeList tuiMode = iota
	tuiModeSpawn
	tuiModeShell
	tuiModeHelp
	tuiModeConfirmQuit
	tuiModeConfirmStop
	tuiModeConfirmRetire
	tuiModeConfirmKillSession
)

// Spawn-form fields, in tab order. The profile sits directly under the group
// because it is the field the others are read against: it decides what a
// blank harness (or model, or approval posture) ends up being. The two
// worktree fields sit under the directory for the same reason: the worktree is
// cut from the repo that directory is in, and the branch field only exists
// once the picker asks for a new one (see spawnFieldEnabled).
const (
	tuiFieldGroup = iota
	tuiFieldProfile
	tuiFieldName
	tuiFieldDir
	tuiFieldWorktree
	tuiFieldWorktreeBranch
	tuiFieldHarness
	tuiFieldBrief
	tuiSpawnFieldCount
)

// Shell-form fields, in tab order.
const (
	tuiShellFieldDir = iota
	tuiShellFieldLabel
	tuiShellFieldCount
)

// tuiHarnessDefault is the spawn form's "don't pin a harness" choice: the
// request leaves Harness empty and the daemon's own profile chain decides.
const tuiHarnessDefault = "(default)"

// tuiProfileDefault is the same idea for the profile picker: naming no
// profile is NOT "no profile at all", it hands the choice back to the
// daemon's chain — the group's default profile, then the global one.
const tuiProfileDefault = "(default)"

// The worktree picker's two settings. "(none)" spawns in the directory the
// form names, which is what every spawn did before this field existed;
// tuiWorktreeNew resolves the branch below it into a worktree first and
// launches the agent inside it.
const (
	tuiWorktreeNone = "(none)"
	tuiWorktreeNew  = "create new worktree"
)

func tuiWorktreeOptions() []string { return []string{tuiWorktreeNone, tuiWorktreeNew} }

type tuiModel struct {
	api tuiAPI

	agents   []tuiAgentRow
	groups   []tuiGroupRow
	profiles []tuiProfileRow
	// sessions are the daemon host's live non-agent sessions, listed under the
	// agents — empty on a console that does not read them (see
	// listsLocalSessions). sessionsErr is that read's last failure, kept apart
	// from refreshErr for the same reason profilesErr is: it costs one part of
	// the listing, not the listing.
	sessions    []tuiSessionRow
	sessionsErr string

	cursor         int
	viewportOffset int
	width          int
	height         int
	// helpOffset is the first help line on screen, in body lines rather than
	// terminal rows. The help text is longer than most terminals, so it gets a
	// viewport of its own — see renderHelpView — reset every time the view is
	// opened.
	helpOffset int

	mode tuiMode
	// notice is the outcome of the last thing the operator did (a spawn, a
	// refused spawn), cleared when they do the next one. refreshErr is the
	// health of the background poll, cleared by the next poll that works —
	// two different lifetimes, so they are two different lines rather than
	// one that can strand a stale "refresh failed" over a live listing.
	notice     string
	refreshErr string
	// profilesErr is the spawn-profile listing's last failure, shown in the
	// spawn form rather than over the agent list — it costs one picker.
	profilesErr string
	// identityWarning is fixed for the console's lifetime: it says the
	// daemon will not treat this console as the operator, and why.
	identityWarning string
	// operator is true when the daemon classifies this console as the human.
	// Attaching to a pane is gated on it — see attachSelected.
	operator bool
	// capabilities are the host/lifetime-dependent operations supported by
	// this API implementation. A remote operator is still the operator, but
	// its terminal and filesystem are not the daemon's.
	capabilities tuiCapabilities
	// connectionLabel distinguishes the embedded console from a standalone
	// client and names the latter's endpoint in the header/help.
	connectionLabel string
	// dashboardURL is the web dashboard running beside this console, empty
	// when --tui is the only surface. It changes what the console can honestly
	// say about approvals and deep links.
	dashboardURL string
	// dashboardLink is dashboardURL carrying a fresh init token, so opening it
	// lands in the dashboard already signed in instead of on its sign-in page.
	// dashboardLinkMinted is when its token was minted, which is what decides
	// when refreshDashboardLink replaces it. Both are empty on a console that
	// may not put a credential on the screen — see canMintDashboardLink.
	dashboardLink       string
	dashboardLinkMinted time.Time
	// suppressSecrets is --no-print-human-token: this terminal's output is
	// scraped or logged, so the console shows no credential of any kind.
	suppressSecrets bool
	// ownsTmuxServer means quitting kills the tmux server this daemon started,
	// provided nothing is left on it (see startTUITmuxServer). Read by
	// confirmPrompt.
	ownsTmuxServer bool
	// tokenLines is the operator-token block this console shows in place of
	// the stdout banner, empty when stdout printed it. showTokenBanner is the
	// startup presentation of that block; it goes away on the first keystroke,
	// and the help view keeps it reachable afterwards.
	tokenLines      []string
	showTokenBanner bool
	// startupDir is the working directory the console was started in, and the
	// shell form's starting point — read once here because a shell session is
	// launched by the daemon process and inherits nothing from a form field
	// left blank.
	startupDir string
	// refreshing / spawning / stopping / retiring / resuming keep the periodic tick from
	// stacking requests and the operator from firing two of the same action at
	// once.
	refreshing        bool
	refreshGeneration uint64
	spawning          bool
	stopping          bool
	retiring          bool
	resuming          bool
	startingShell     bool
	killingSession    bool
	// spawnFormGen numbers the spawn forms this console has opened; see
	// tuiSpawnForm.gen.
	spawnFormGen uint64
	// spawnWorktree is the worktree the in-flight spawn was given, empty when
	// it was not given one. It outlives the form because it is only worth
	// reporting once the spawn itself has answered: on success it says where
	// the agent actually landed, and on failure it names a directory that was
	// not there before — see tuiSpawnedMsg.
	spawnWorktree tuiWorktreeResponse
	// reconcilingMutation latches after a remote mutation's outcome becomes
	// ambiguous. It survives failed polls and blocks every mutating key until
	// a refresh started after that mutation establishes canonical daemon
	// state. The generation prevents an older in-flight poll from clearing it.
	reconcilingMutation      bool
	reconciliationRefreshGen uint64
	lastRefresh              time.Time
	// usage is the last subscription-usage readout the daemon served — the
	// account's rolling limits and API spend, shown as the console's status
	// line. usageLoaded distinguishes "the daemon has answered" from the empty
	// zero value; usageFailed says every attempt so far has failed, which is
	// the one thing worth saying out loud in place of the figures.
	// lastUsageAttempt paces the poll (see tuiUsageInterval) — it is far slower
	// than the listing's, so it rides the same tick rather than its own timer.
	// usageUnsupported says the daemon has no usage endpoint at all — a
	// standalone console pointed at an older tclaude. That is not a failure to
	// report, so the line goes away entirely and the poll drops to a slow
	// re-check that costs nothing but lets an upgraded daemon bring it back.
	usage            tuiUsage
	usageLoaded      bool
	usageFailed      bool
	usageUnsupported bool
	usageFetching    bool
	lastUsageAttempt time.Time
	// lifecycleTarget is the row the stop/retire/kill confirmation is about,
	// captured when the prompt opens rather than re-read when it is answered:
	// the listing re-sorts under the cursor every two seconds, so resolving the
	// target on "y" could act on something the operator never saw named.
	lifecycleTarget tuiListRow

	filterActive bool

	form  tuiSpawnForm
	shell tuiShellForm
}

// tuiSpawnForm is the "new agent" prompt. Group, profile and harness are
// cycled choices; the rest are text inputs.
type tuiSpawnForm struct {
	field int

	groupNames []string
	groupIdx   int

	profileNames []string
	profileIdx   int

	harnessNames []string
	harnessIdx   int

	worktreeNames []string
	worktreeIdx   int

	name   textinput.Model
	dir    textinput.Model
	branch textinput.Model
	brief  textinput.Model
	// branchSynced is the worktree branch's "still following the name" state:
	// while it holds, every keystroke in Name is copied into the branch field,
	// so the common case (one agent, its own worktree, named after it) needs
	// nothing typed twice. Typing in the branch field itself ends the sync —
	// after that the branch is the operator's and the name picker leaves it
	// alone, the same contract the directory prefill has with the group picker.
	branchSynced bool
	// gen identifies this form among the ones this console has opened, so a
	// worktree resolution that lands after the operator has escaped and
	// reopened the prompt can tell "the form that asked" from "the form on
	// screen" — and leave the second one, with everything typed into it,
	// alone. Assigned by openSpawnForm from the model's counter.
	gen uint64
	// dirPrefill is the value prefillDir last wrote into dir — the selected
	// group's default directory. It is what tells an untouched field from one
	// the operator has typed in, so changing the group can follow the first
	// and must leave the second alone.
	dirPrefill string
	// namePrefill is the same idea one field up: the value applyProfile last
	// wrote into name, which is the selected profile's agent_name. Changing the
	// profile follows an untouched name field and leaves a typed one alone.
	namePrefill string
	// worktreeTouched records that the operator has cycled the worktree picker
	// themselves. Until they do, a profile's sync_worktree toggle sets it; after
	// they do, the picker is theirs — the same contract dirPrefill and
	// namePrefill give the two text fields.
	worktreeTouched bool
	// dirSuggestions holds the ambiguous Tab-completion candidates for dir,
	// listed under the field until the next keystroke.
	dirSuggestions []string
}

// tuiShellForm is the "new shell session" prompt: a plain interactive shell in
// its own tmux session, which is a session and not an agent — no conversation,
// no group, no permissions, and so a "(session)" row in the listing rather
// than one of the agents. It asks only what a shell session actually has:
// where to start, and what to call the tmux handle.
type tuiShellForm struct {
	field int

	dir   textinput.Model
	label textinput.Model
	// dirPrefill is the value openShellForm wrote into dir — the directory the
	// console itself was started in. As in the spawn form it separates an
	// untouched field from one the operator has typed into, which is what
	// decides whether Tab completes a path or moves to the next field.
	dirPrefill string
	// dirSuggestions holds the ambiguous Tab-completion candidates for dir,
	// listed under the field until the next keystroke.
	dirSuggestions []string
}

type (
	tuiTickMsg time.Time
	// tuiDataMsg carries one completed refresh — both lists, or the error
	// that stopped it.
	tuiDataMsg struct {
		refreshGeneration uint64
		agents            []tuiAgentRow
		groups            []tuiGroupRow
		profiles          []tuiProfileRow
		sessions          []tuiSessionRow
		err               error
		// profilesErr is the profile listing's own failure, kept apart from
		// err: it costs the spawn form one picker, not the whole console.
		profilesErr error
		// sessionsErr is the non-agent session listing's own failure, kept
		// apart for the same reason: an agent listing that is live and correct
		// must not go blank because the host's session store was unreadable
		// for one poll.
		sessionsErr error
	}
	// tuiUsageMsg carries one completed usage poll — the account's rolling
	// limits and API spend, or the error that stopped the read.
	tuiUsageMsg struct {
		usage tuiUsage
		err   error
	}
	// tuiWorktreeResolvedMsg carries the outcome of the worktree step that
	// runs before a spawn that asked for one. It carries the spawn it was
	// resolved for, so the request that goes out is the one the form built
	// rather than a re-read of fields the operator may have edited since —
	// and formGen names the form that asked, so a late answer closes that
	// form and never one the operator has opened since.
	tuiWorktreeResolvedMsg struct {
		group   string
		formGen uint64
		req     agent.SpawnRequest
		wt      tuiWorktreeResponse
		err     error
	}
	// tuiSpawnedMsg carries the outcome of one spawn request.
	tuiSpawnedMsg struct {
		group string
		resp  agent.SpawnResponse
		err   error
	}
	// tuiShellStartedMsg carries the outcome of one shell-session launch.
	tuiShellStartedMsg struct {
		created session.ShellSession
		err     error
	}
	// tuiSessionKilledMsg carries the outcome of ending one non-agent session.
	tuiSessionKilledMsg struct {
		session string
		err     error
	}
	// tuiAttachedMsg carries the outcome of putting the operator on an
	// agent's pane — after they detach, in the attach case.
	tuiAttachedMsg struct {
		agent   string
		session string
		remote  bool
		err     error
	}
	// tuiRetiredMsg carries the outcome of one retire request.
	tuiRetiredMsg struct {
		agent string
		res   tuiRetireResult
		err   error
	}
	// tuiStoppedMsg carries the outcome of one stop request — turning an
	// online agent offline.
	tuiStoppedMsg struct {
		agent string
		res   tuiStopResult
		err   error
	}
	// tuiResumedMsg carries the outcome of one resume request — turning an
	// offline agent back on.
	tuiResumedMsg struct {
		agent string
		res   tuiResumeResult
		err   error
	}
)

func newTUIModel(api tuiAPI) tuiModel {
	// The zero-transport model is used by focused rendering/interaction tests
	// and historically represents the embedded console. A real API always
	// replaces these defaults with its explicit capabilities below.
	m := tuiModel{
		api:               api,
		refreshing:        true, // Init immediately starts the first refresh.
		refreshGeneration: 1,
		capabilities: tuiCapabilities{
			attachAgent:      true,
			attachLocalPane:  true,
			startLocalShell:  true,
			localSessions:    true,
			completeLocalDir: true,
			shutdownOnQuit:   true,
		},
		connectionLabel: "in-process",
	}
	// Where the console was started is where a shell session started from it
	// defaults to. A daemon that cannot read its own working directory leaves
	// this blank, which the form treats as "the daemon's directory" rather than
	// refusing the launch.
	if wd, err := os.Getwd(); err == nil {
		m.startupDir = wd
	}
	if api != nil {
		m.identityWarning = api.identityWarning()
		m.operator = api.isOperator()
		m.capabilities = api.capabilities()
		m.connectionLabel = api.connectionLabel()
	}
	return m
}

// remoteConsole reports whether this console drives a daemon in another
// process. A remote operator is still the operator, but the daemon's host,
// terminal and in-memory state are not this process's.
func (m tuiModel) remoteConsole() bool {
	return m.connectionLabel != "" && m.connectionLabel != "in-process"
}

// tuiDashboardLinkRotate is how long a minted sign-in link stays on screen
// before the console replaces it. Well under initTokenTTL, so the link being
// looked at always has time left to be opened; well over tuiRefreshInterval,
// so the line is not rewritten under a mouse dragging a selection across it.
const tuiDashboardLinkRotate = 40 * time.Second

// canMintDashboardLink reports whether this console may put a dashboard
// sign-in link on its screen.
//
// The link is a capability: redeeming it at the dashboard root buys a session
// cookie, and with it the whole authenticated /api surface that
// handleDashboardOpen guards with peer-cred requireHuman precisely so agents
// cannot reach it. So it goes on screen only when the screen is the human's:
//
//   - m.operator — the daemon classifies this console as the human. An agentd
//     started from inside a harness pane is classified as that agent instead
//     (see identityWarning), and that agent can read the pane it is running in:
//     minting there would hand it, via tmux capture-pane, the very authority
//     the permission system exists to withhold. Such a console still gets the
//     dashboard's address, just no token on it.
//   - not --no-print-human-token, whose whole point is that this terminal's
//     output is scraped or logged, so no credential may appear on it.
//
// A remote console is excluded for a different reason: mintInitToken writes
// this process's in-memory store, which is the dashboard's store exactly when
// the daemon is this process. A token from here would be worthless there.
func (m tuiModel) canMintDashboardLink() bool {
	return m.dashboardURL != "" && !m.remoteConsole() && m.operator && !m.suppressSecrets
}

// refreshDashboardLink keeps the console's ready-to-open dashboard link
// openable, minting a replacement once the current one has been up for
// tuiDashboardLinkRotate.
//
// Init tokens are single-use and expire in a minute (see inittoken.go), so one
// minted at startup and left there would be dead long before the operator got
// round to it. Rotating on age rather than on every tick is what keeps the
// line stable enough to select and copy, and leaves at most a couple of
// unredeemed tokens in the store at a time; the cost is that the link visibly
// on screen may already have been spent by an earlier open, which the
// dashboard answers with its "expired or was already used" sign-in page rather
// than a dead end.
func (m tuiModel) refreshDashboardLink(now time.Time) tuiModel {
	if !m.canMintDashboardLink() {
		m.dashboardLink, m.dashboardLinkMinted = "", time.Time{}
		return m
	}
	if m.dashboardLink != "" && now.Sub(m.dashboardLinkMinted) < tuiDashboardLinkRotate {
		return m
	}
	m.dashboardLink = m.dashboardURL + "/?init_token=" + mintInitToken(initScopeDashboard)
	m.dashboardLinkMinted = now
	return m
}

// dashboardAddressLine is the address the console prints for the co-running
// web dashboard: the signed-in link when it has one, the bare URL otherwise.
// Empty means there is nothing to print — no dashboard, or a remote console,
// which names its connection instead.
func (m tuiModel) dashboardAddressLine() string {
	if m.dashboardURL == "" || m.remoteConsole() {
		return ""
	}
	if m.dashboardLink != "" {
		return m.dashboardLink
	}
	return m.dashboardURL
}

// dashboardAddressIndent is the address line's leading indent, counted into
// its width because the terminal wraps on the whole line.
const dashboardAddressIndent = 4

// dashboardAddressRows is how many terminal rows the address line occupies. A
// URL only works whole, so a narrow terminal wraps it rather than the console
// truncating it — and then the row budget has to pay for every row it took,
// or the list overflows the alt screen (see viewportHeight).
func (m tuiModel) dashboardAddressRows() int {
	addr := m.dashboardAddressLine()
	if addr == "" {
		return 0
	}
	w := lipgloss.Width(addr) + dashboardAddressIndent
	if m.width <= 0 || w <= m.width {
		return 1
	}
	return (w + m.width - 1) / m.width
}

func (m tuiModel) visibleAgents() []tuiAgentRow {
	if !m.filterActive {
		return m.agents
	}
	var out []tuiAgentRow
	for _, a := range m.agents {
		if a.Online {
			out = append(out, a)
		}
	}
	return out
}

// listsLocalSessions reports whether this console shows the daemon host's
// non-agent sessions. Both gates matter, and for the same reasons the shell
// form has them: the listing is read off the host's own session store and tmux
// server (so a remote console cannot), and it is the operator's own working
// directories (so an agent-class console may not — see completingDir).
func (m tuiModel) listsLocalSessions() bool {
	return m.operator && m.capabilities.localSessions
}

// visibleRows is the listing the cursor moves over: the agents the filter
// leaves, then the live non-agent sessions.
//
// Sessions come last as a block rather than interleaved by name. They are a
// different kind of thing with a different set of keys, and keeping them
// together means the operator's mental "the agents are the top N rows" holds
// between refreshes — where a name-interleaved order would slide a session
// into the middle of the roster and back out again as agents come and go.
//
// The active filter does not apply to them: only live sessions are listed at
// all, so every session row is already an active one.
func (m tuiModel) visibleRows() []tuiListRow {
	agents := m.visibleAgents()
	rows := make([]tuiListRow, 0, len(agents)+len(m.sessions))
	for _, a := range agents {
		rows = append(rows, agentListRow(a))
	}
	for _, s := range m.sessions {
		rows = append(rows, sessionListRow(s))
	}
	return rows
}

// tuiListChrome is what renderList spends on everything that is not a table
// row: 3 lines of header, the table's own header + separator, a blank line
// and the summary, and a blank line and the key line.
const tuiListChrome = 9

// viewportHeight is how many agent rows fit right now. It is derived rather
// than stored because two of renderList's lines are conditional: a refresh
// error and the two-line quit prompt. Ignoring them overflowed the terminal
// exactly when one was showing, which scrolls the screen out from under
// bubbletea's diff renderer and leaves later partial repaints on the wrong
// lines.
func (m tuiModel) viewportHeight() int {
	chrome := tuiListChrome
	// The dashboard address takes header lines of its own when there is one.
	chrome += m.dashboardAddressRows()
	if m.showTokenBanner {
		chrome += len(m.tokenLines) + 2
	}
	if m.identityWarning != "" {
		chrome += lipgloss.Height(m.renderWrapped(m.identityWarning)) + 1
	}
	if m.refreshErr != "" {
		chrome += lipgloss.Height(m.renderWrapped(m.refreshErr))
	}
	if m.notice != "" {
		chrome += lipgloss.Height(m.renderWrapped(m.notice))
	}
	// The usage line is one row by contract — fitUsageLine trims it to the
	// terminal rather than wrapping — and is absent entirely until the console
	// has a readout to show.
	if m.usageLine() != "" {
		chrome++
	}
	if m.confirmPrompt() != "" {
		chrome += 2
	}
	return max(m.height-chrome, 1)
}

func (m tuiModel) renderWrapped(s string) string {
	if s == "" {
		return ""
	}
	indent := 2
	w := m.width - indent
	if w <= 0 {
		indent = 0
		w = max(m.width, 1)
	}
	return lipgloss.NewStyle().Width(w).PaddingLeft(indent).Render(s)
}

func (m tuiModel) Init() tea.Cmd {
	return tea.Batch(m.refreshCmd(), tuiTickCmd())
}

func tuiTickCmd() tea.Cmd {
	return tea.Tick(tuiRefreshInterval, func(t time.Time) tea.Msg { return tuiTickMsg(t) })
}

// refreshCmd re-reads both listings off the daemon API. It runs as a
// bubbletea command (i.e. on its own goroutine) because the peers listing
// touches tmux and the conversation index, and the console must stay
// responsive while it does.
func (m tuiModel) refreshCmd() tea.Cmd {
	api := m.api
	generation := m.refreshGeneration
	withSessions := m.listsLocalSessions()
	return func() tea.Msg {
		var agents []tuiAgentRow
		if err := api.get("/v1/peers", &agents); err != nil {
			return tuiDataMsg{refreshGeneration: generation, err: err}
		}
		var groups []tuiGroupRow
		if err := api.get("/v1/groups", &groups); err != nil {
			return tuiDataMsg{refreshGeneration: generation, err: err}
		}
		// The profile list feeds one field of a form that is usually closed,
		// so its failure travels beside the listing rather than instead of it:
		// a console whose agents are all live and visible must not go blank
		// because a profile read hit the DB at a bad moment.
		var profiles []tuiProfileRow
		profilesErr := api.get("/v1/spawn-profiles", &profiles)
		// The host's own sessions, which have no /v1 shape — see
		// tui_sessions.go. Their failure travels beside the listing for the
		// same reason the profiles' does.
		var sessions []tuiSessionRow
		var sessionsErr error
		if withSessions {
			sessions, sessionsErr = tuiListLocalSessions()
		}
		sortTUIAgents(agents)
		sort.Slice(groups, func(i, j int) bool { return groups[i].Name < groups[j].Name })
		sort.Slice(profiles, func(i, j int) bool { return profiles[i].Name < profiles[j].Name })
		return tuiDataMsg{
			refreshGeneration: generation,
			agents:            agents,
			groups:            groups,
			profiles:          profiles,
			sessions:          sessions,
			profilesErr:       profilesErr,
			sessionsErr:       sessionsErr,
		}
	}
}

// beginRefresh assigns each poll a generation so that a slower, older poll
// cannot overwrite newer state or settle mutation reconciliation.
func (m tuiModel) beginRefresh() (tuiModel, tea.Cmd) {
	m.refreshGeneration++
	m.refreshing = true
	return m, m.refreshCmd()
}

// sortTUIAgents orders the listing online-first, then by name, so the
// agents that can act sit at the top and a row keeps its place between
// refreshes.
func sortTUIAgents(agents []tuiAgentRow) {
	sort.SliceStable(agents, func(i, j int) bool {
		if agents[i].Online != agents[j].Online {
			return agents[i].Online
		}
		return strings.ToLower(agents[i].name()) < strings.ToLower(agents[j].name())
	})
}

// tuiResolveWorktreeCmd asks the daemon for the worktree the spawn form
// picked, before the spawn itself goes out. It runs as its own command
// because it is a git operation on the daemon's host — a fetchless one, but
// still a subprocess — and because its failures are the operator's to fix in
// the form (a branch that cannot be cut, a directory that is not a repo)
// rather than something to report over a listing they have already been
// returned to.
func tuiResolveWorktreeCmd(
	api tuiAPI, wtReq tuiWorktreeRequest, group string, formGen uint64, req agent.SpawnRequest,
) tea.Cmd {
	return func() tea.Msg {
		var resp tuiWorktreeResponse
		err := api.post(tuiWorktreePath, wtReq, &resp)
		return tuiWorktreeResolvedMsg{group: group, formGen: formGen, req: req, wt: resp, err: err}
	}
}

func tuiSpawnCmd(api tuiAPI, group string, req agent.SpawnRequest) tea.Cmd {
	return func() tea.Msg {
		var resp agent.SpawnResponse
		err := api.post("/v1/groups/"+url.PathEscape(group)+"/spawn", req, &resp)
		return tuiSpawnedMsg{group: group, resp: resp, err: err}
	}
}

// tuiRetireCmd retires one agent through the daemon's own verb. The
// require_offline precondition keeps Delete's progressive contract true even
// when another client resumes the agent while the confirmation is open. The
// remaining documented defaults still apply: the pane is asked to exit and
// the worktree is left alone.
func tuiRetireCmd(api tuiAPI, convID, name string) tea.Cmd {
	return func() tea.Msg {
		var res tuiRetireResult
		err := api.post("/v1/agent/"+url.PathEscape(convID)+"/retire?require_offline=1", nil, &res)
		return tuiRetiredMsg{agent: name, res: res, err: err}
	}
}

// tuiStopCmd turns one online agent off through the daemon's own stop verb.
// It sends no force query: Delete is the graceful step toward removal, so it
// asks the harness to exit cleanly and never drops unsubmitted input.
func tuiStopCmd(api tuiAPI, convID, name string) tea.Cmd {
	return func() tea.Msg {
		var res tuiStopResult
		err := api.post("/v1/agent/"+url.PathEscape(convID)+"/stop", nil, &res)
		return tuiStoppedMsg{agent: name, res: res, err: err}
	}
}

// tuiResumeCmd turns one offline agent back on through the daemon's own
// resume verb. Like the retire above it sends no query parameters, which
// leaves ?recreate=1 off: re-creating a launch directory the operator has
// since deleted is a decision the console has no way to put to them, so a
// vanished directory comes back as an error they can act on instead.
func tuiResumeCmd(api tuiAPI, convID, name string) tea.Cmd {
	return func() tea.Msg {
		var res tuiResumeResult
		err := api.post("/v1/agent/"+url.PathEscape(convID)+"/resume", nil, &res)
		return tuiResumedMsg{agent: name, res: res, err: err}
	}
}

// tuiHarnessOptions lists the harnesses a spawn may name, led by the
// "(default)" sentinel that pins nothing.
func tuiHarnessOptions() []string {
	out := []string{tuiHarnessDefault}
	for _, name := range harness.Names() {
		if h, ok := harness.Get(name); ok && h.Spawn != nil {
			out = append(out, name)
		}
	}
	return out
}

// tuiProfileOptions lists the spawn profiles the form may name, led by the
// "(default)" sentinel that names none. A disabled profile is left out: the
// daemon refuses a spawn that selects one, and nothing in this console can
// re-enable it.
func tuiProfileOptions(profiles []tuiProfileRow) []string {
	out := []string{tuiProfileDefault}
	for _, p := range profiles {
		if p.Name != "" && !p.disabled() {
			out = append(out, p.Name)
		}
	}
	return out
}

func newTUITextInput(prompt string) textinput.Model {
	ti := textinput.New()
	ti.Prompt = prompt
	ti.SetWidth(60)
	return ti
}

// openSpawnForm builds a fresh "new agent" prompt over the current group and
// profile lists.
//
// Both cycled launch fields start on "(default)", which is the only setting
// under which the daemon's resolution chain can do its job. The harness picker
// used to open on the default harness by name instead, which reads the same
// but is an explicit pin on the wire — and an explicit harness outranks every
// profile tier, so a profile that selects Codex would have been overruled by a
// field the operator never touched.
func (m tuiModel) openSpawnForm() tuiModel {
	m.spawnFormGen++
	form := tuiSpawnForm{
		gen:           m.spawnFormGen,
		harnessNames:  tuiHarnessOptions(),
		profileNames:  tuiProfileOptions(m.profiles),
		worktreeNames: tuiWorktreeOptions(),
		name:          newTUITextInput("  Name:      "),
		dir:           newTUITextInput("  Directory: "),
		branch:        newTUITextInput("  Branch:    "),
		brief:         newTUITextInput("  Brief:     "),
		// The branch follows the name until the operator says otherwise, so
		// picking "create new worktree" on a named agent needs nothing else
		// typed. Nothing is created until they pick it: the form opens on
		// "(none)", which is the spawn every earlier version of this form made.
		branchSynced: true,
	}
	for _, g := range m.groups {
		form.groupNames = append(form.groupNames, g.Name)
	}
	m.form = form
	m.mode = tuiModeSpawn
	return m.prefillDir()
}

// groupDefaultDir is the group's configured default working directory, ending
// in a separator, or "" when it has none (or when the daemon does not serve
// this console the path — see tuiGroupRow).
//
// The trailing separator is the point of the prefill: it makes the group's
// directory a starting point rather than an answer, so the operator types the
// subdirectory they want straight onto it and Tab lists what is in there.
func (m tuiModel) groupDefaultDir(name string) string {
	for _, g := range m.groups {
		if g.Name != name {
			continue
		}
		if dir := strings.TrimSpace(g.DefaultCwd); dir != "" {
			return strings.TrimSuffix(dir, "/") + "/"
		}
		return ""
	}
	return ""
}

// prefillDir points the directory field at the selected group's default
// directory, which is where that group's agents are launched anyway — so the
// common "somewhere under the group's directory" spawn is a subdirectory name
// away instead of a full path retyped.
//
// It only ever writes a field the operator has not touched. Once they have
// typed their own path, cycling the group picker leaves it exactly as it is:
// the alternative silently discards a path they may have Tab-completed their
// way to.
func (m tuiModel) prefillDir() tuiModel {
	// A cleared field counts as untouched: blank already means "the group's
	// default directory", so filling in the new group's own is the same
	// launch either way — and it puts the path back where it can be extended.
	if v := m.form.dir.Value(); v != "" && v != m.form.dirPrefill {
		return m
	}
	m.form.dirPrefill = m.groupDefaultDir(m.selectedGroup())
	m.form.dir.SetValue(m.form.dirPrefill)
	m.form.dir.CursorEnd()
	return m
}

func (m tuiModel) selectedGroup() string {
	if m.form.groupIdx < 0 || m.form.groupIdx >= len(m.form.groupNames) {
		return ""
	}
	return m.form.groupNames[m.form.groupIdx]
}

// selectedProfile is the profile name the request should carry, empty for the
// "(default)" sentinel — which is what lets the group's and the global default
// profile apply as they do for every other spawn surface.
func (m tuiModel) selectedProfile() string {
	if m.form.profileIdx < 0 || m.form.profileIdx >= len(m.form.profileNames) {
		return ""
	}
	if name := m.form.profileNames[m.form.profileIdx]; name != tuiProfileDefault {
		return name
	}
	return ""
}

// selectedProfileRow is the listing row behind the profile picker's current
// choice, absent for the "(default)" sentinel and for a name the last refresh
// has since dropped.
func (m tuiModel) selectedProfileRow() (tuiProfileRow, bool) {
	name := m.selectedProfile()
	if name == "" {
		return tuiProfileRow{}, false
	}
	for _, p := range m.profiles {
		if p.Name == name {
			return p, true
		}
	}
	return tuiProfileRow{}, false
}

// applyProfile pulls into the form the two spawn-profile fields this form has
// to apply itself: the agent name, and whether the agent is spawned into a
// worktree of its own. Everything else a profile carries — harness, model,
// approval posture, role, descr, the brief, group context, the owner flag,
// permission overrides — the daemon resolves from the profile NAME the request
// posts, so the form neither reads nor shows it.
//
// Both follow the directory prefill's contract with the group picker: a field
// the operator has already made theirs is left alone, and an untouched one
// follows the profile.
func (m tuiModel) applyProfile() tuiModel {
	// "(default)" names no profile, so it takes back the name the last one
	// prefilled rather than stranding it on a form that no longer explains it.
	// The worktree picker is left where it is: naming no profile says nothing
	// about worktrees, and silently disarming one would lose a branch the
	// operator may already have typed.
	prof, ok := m.selectedProfileRow()
	if v := m.form.name.Value(); v == "" || v == m.form.namePrefill {
		m.form.namePrefill = strings.TrimSpace(prof.AgentName)
		m.form.name.SetValue(m.form.namePrefill)
		m.form.name.CursorEnd()
	}
	if !ok {
		return m.syncWorktreeBranch()
	}
	// The picker only exists for a console allowed to cut worktrees, so a
	// profile cannot arm it on one that is not (see canCreateWorktree).
	if sync, set := prof.syncsWorktree(); set && m.canCreateWorktree() && !m.form.worktreeTouched {
		want := tuiWorktreeNone
		if sync {
			want = tuiWorktreeNew
		}
		for i, name := range m.form.worktreeNames {
			if name == want {
				m.form.worktreeIdx = i
				break
			}
		}
	}
	return m.syncWorktreeBranch()
}

func (m tuiModel) selectedHarness() string {
	if m.form.harnessIdx < 0 || m.form.harnessIdx >= len(m.form.harnessNames) {
		return ""
	}
	if name := m.form.harnessNames[m.form.harnessIdx]; name != tuiHarnessDefault {
		return name
	}
	return ""
}

// creatingWorktree reports whether the form's worktree picker is asking for a
// new worktree rather than a plain launch directory.
func (m tuiModel) creatingWorktree() bool {
	return m.form.worktreeIdx >= 0 && m.form.worktreeIdx < len(m.form.worktreeNames) &&
		m.form.worktreeNames[m.form.worktreeIdx] == tuiWorktreeNew
}

// canCreateWorktree gates the worktree fields on the console being the
// operator: resolving a worktree runs git as the daemon process — the human's
// own filesystem, outside any agent sandbox — and it CREATES a directory of
// the caller's choosing. A console started from inside a harness pane is
// agent-class and drivable by that agent through tmux send-keys, so it is
// offered the field no more than the daemon would honour it (see
// handleTUIWorktree, which refuses the same caller).
//
// Unlike directory completion and the shell form, that is the WHOLE gate:
// those two also need the console to share the daemon's host and terminal
// (completeLocalDir / startLocalShell), because they read or drive this
// machine. A worktree is cut on the daemon's host in either case, so a remote
// operator console gets the field too.
func (m tuiModel) canCreateWorktree() bool { return m.operator }

// spawnFieldEnabled reports whether a field takes part in this form at all.
// The two disabled shapes are a console that may not create worktrees, and the
// branch field of a form that is not creating one — a field with nothing to
// say should not cost a tab stop.
func (m tuiModel) spawnFieldEnabled(field int) bool {
	switch field {
	case tuiFieldWorktree:
		return m.canCreateWorktree()
	case tuiFieldWorktreeBranch:
		return m.canCreateWorktree() && m.creatingWorktree()
	default:
		return true
	}
}

// moveSpawnField shifts focus through the form, wrapping, and moves the
// text-input focus with it — the group, profile, worktree and harness fields
// have no input model behind them, so all of the inputs blur for those.
// Disabled fields are stepped over rather than landed on.
func (m tuiModel) moveSpawnField(delta int) tuiModel {
	step := 1
	if delta < 0 {
		step = -1
	}
	// Bounded by the field count: every field being disabled is impossible
	// (the text inputs always are), but a loop over a form must not depend on
	// that to terminate.
	for range tuiSpawnFieldCount {
		m.form.field = ((m.form.field+step)%tuiSpawnFieldCount + tuiSpawnFieldCount) % tuiSpawnFieldCount
		if m.spawnFieldEnabled(m.form.field) {
			break
		}
	}
	return m.focusSpawnField(m.form.field)
}

// focusSpawnField puts the form on one field and moves the text-input focus
// with it, without stepping over anything — used by moveSpawnField and by a
// refused submit, which sends the operator back to the field to fix.
func (m tuiModel) focusSpawnField(field int) tuiModel {
	m.form.field = field
	m.form.name.Blur()
	m.form.dir.Blur()
	m.form.branch.Blur()
	m.form.brief.Blur()
	switch field {
	case tuiFieldName:
		m.form.name.Focus()
	case tuiFieldDir:
		m.form.dir.Focus()
	case tuiFieldWorktreeBranch:
		m.form.branch.Focus()
	case tuiFieldBrief:
		m.form.brief.Focus()
	}
	return m
}

// cycleChoice steps whichever of the choice fields is focused.
func (m tuiModel) cycleChoice(delta int) tuiModel {
	switch m.form.field {
	case tuiFieldGroup:
		if n := len(m.form.groupNames); n > 0 {
			m.form.groupIdx = ((m.form.groupIdx+delta)%n + n) % n
			// The directory follows the group it belongs to, until the
			// operator types one of their own.
			m = m.prefillDir()
		}
	case tuiFieldProfile:
		if n := len(m.form.profileNames); n > 0 {
			m.form.profileIdx = ((m.form.profileIdx+delta)%n + n) % n
			// The two profile fields this form has to apply itself follow the
			// profile they came from, until the operator overrides them.
			m = m.applyProfile()
		}
	case tuiFieldHarness:
		if n := len(m.form.harnessNames); n > 0 {
			m.form.harnessIdx = ((m.form.harnessIdx+delta)%n + n) % n
		}
	case tuiFieldWorktree:
		if n := len(m.form.worktreeNames); n > 0 {
			m.form.worktreeIdx = ((m.form.worktreeIdx+delta)%n + n) % n
			m.form.worktreeTouched = true
			// Turning the picker to "create new worktree" is what makes the
			// branch field appear, so it arrives already carrying the name —
			// the sync's whole point is that the operator names the agent once.
			m = m.syncWorktreeBranch()
		}
	}
	return m
}

// syncWorktreeBranch copies the agent's name into the branch field while the
// branch is still following it. A blank name leaves a blank branch, which the
// submit refuses by name rather than guessing one: an unnamed agent gets an
// auto-generated label the console cannot know in advance, and cutting a
// branch called something the operator never saw is worse than asking.
//
// The leading characters a branch may not start with are dropped on the way
// across (see validateTUIWorktreeBranch): the spawn-name charset allows a
// leading "-", so a perfectly good agent name would otherwise sync into a
// branch the form then refuses — on a field the operator never typed in. The
// trimmed value is on screen, so nothing is renamed behind their back.
func (m tuiModel) syncWorktreeBranch() tuiModel {
	if !m.form.branchSynced {
		return m
	}
	m.form.branch.SetValue(strings.TrimLeft(strings.TrimSpace(m.form.name.Value()), "-./"))
	m.form.branch.CursorEnd()
	return m
}

// tuiIsChoiceField reports whether field is one of the cycled pickers, which
// is what makes ←/→ (and space) change a value rather than reach a text input.
func tuiIsChoiceField(field int) bool {
	switch field {
	case tuiFieldGroup, tuiFieldProfile, tuiFieldHarness, tuiFieldWorktree:
		return true
	default:
		return false
	}
}

// completingDir reports whether a Tab should complete a path rather than
// move to the next field: this transport shares the daemon filesystem, the
// caller is an operator, the directory field is focused, and there is
// something to complete.
//
// The operator check is the same gate attachSelected uses, for the same
// reason. Completion reads the filesystem as the process agentd runs in —
// the human's own, outside any agent sandbox — and a console started from
// inside a harness pane is agent-class and drivable by that agent through
// tmux send-keys. Ungated, Tab would hand it bulk directory listings from
// outside its sandbox. A failed spawn already leaks whether one guessed
// path exists, but that is a per-path oracle; enumerating names the agent
// could not have guessed is a different thing, so the console does not
// offer it to a caller the daemon would not treat as the human.
func (m tuiModel) completingDir() bool {
	if !m.operator || !m.capabilities.completeLocalDir || m.form.field != tuiFieldDir {
		return false
	}
	// Only a path the operator has typed into is completed. An untouched
	// field — blank, or the group's own directory as the form filled it in —
	// keeps Tab's ordinary next-field job, which is the only way to tab past
	// Directory at all. It would not be a harmless completion either:
	// CompleteDirPath resolves a directory with exactly one child straight
	// into that child, so a Tab meant as "next field" would silently spawn
	// the agent somewhere the operator never chose.
	value := m.form.dir.Value()
	return value != "" && value != m.form.dirPrefill
}

// completeDir runs one round of bash-like directory completion on the
// directory field — the same helper the `session watch` new-session prompt
// uses, so both forms complete paths identically. An unambiguous match is
// completed through its trailing "/" (Tab again walks further down); an
// ambiguous one extends as far as it can and leaves the candidates for the
// form to list.
func (m tuiModel) completeDir() tuiModel {
	completed, candidates := clcommon.CompleteDirPath(m.form.dir.Value())
	m.form.dir.SetValue(completed)
	m.form.dir.CursorEnd()
	m.form.dirSuggestions = candidates
	return m
}

func (m tuiModel) updateFocusedInput(msg tea.Msg) (tuiModel, tea.Cmd) {
	var cmd tea.Cmd
	switch m.form.field {
	case tuiFieldName:
		m.form.name, cmd = m.form.name.Update(msg)
		// The branch follows the name as it is typed, so the operator sees the
		// branch they are about to get rather than discovering it at submit.
		m = m.syncWorktreeBranch()
	case tuiFieldDir:
		m.form.dir, cmd = m.form.dir.Update(msg)
	case tuiFieldWorktreeBranch:
		before := m.form.branch.Value()
		m.form.branch, cmd = m.form.branch.Update(msg)
		// Any edit here ends the sync — including clearing the field, which is
		// how an operator says "not this name" rather than "no opinion". Keys
		// that change nothing (cursor moves) leave the sync alone.
		if m.form.branch.Value() != before {
			m.form.branchSynced = false
		}
	case tuiFieldBrief:
		m.form.brief, cmd = m.form.brief.Update(msg)
	}
	return m, cmd
}

// submitSpawn turns the form into a spawn request. A blank directory is left
// blank on purpose: the daemon then falls back to the group's default_cwd,
// which is what the operator configured it for.
func (m tuiModel) submitSpawn() (tuiModel, tea.Cmd) {
	group := m.selectedGroup()
	if group == "" {
		// An empty picker means one of two very different things, and saying
		// "no groups" to someone whose groups simply have not arrived yet
		// sends them off to create a duplicate.
		if m.lastRefresh.IsZero() {
			m.notice = "Group list has not loaded yet — press r to refresh, then try again."
		} else {
			m.notice = "No groups yet — create one first: tclaude agent groups create <name>"
		}
		return m, nil
	}
	if m.spawning {
		m.notice = "A spawn is already in flight."
		return m, nil
	}
	req := agent.SpawnRequest{
		Cwd:            strings.TrimSpace(m.form.dir.Value()),
		Profile:        m.selectedProfile(),
		Harness:        m.selectedHarness(),
		InitialMessage: strings.TrimSpace(m.form.brief.Value()),
	}
	// The name is STATED, not merely set: this form has a box for it and fills
	// that box from the selected profile, so an emptied box is the operator
	// saying "not that name" — not a silence for the profile to answer again.
	//
	// The brief is deliberately NOT stated. This form never prefills it (a
	// profile's brief may be many lines, and this is a one-line input that would
	// mangle them), so there is nothing here for the operator to have cleared:
	// an empty box is a real silence, and the profile's brief fills it as the
	// task default it is. briefHint says so on screen.
	req.StateName(strings.TrimSpace(m.form.name.Value()))
	m.spawnWorktree = tuiWorktreeResponse{}
	if m.canCreateWorktree() && m.creatingWorktree() {
		branch := strings.TrimSpace(m.form.branch.Value())
		if err := validateTUIWorktreeBranch(branch); err != nil {
			// The daemon refuses the same names for the same reasons; checking
			// here keeps the form open on the field the operator has to fix,
			// instead of closing it and reporting the refusal over the listing.
			m.notice = "Cannot use that branch name: " + err.Error()
			return m.focusSpawnField(tuiFieldWorktreeBranch), nil
		}
		// The form stays open until the worktree exists. Everything that can go
		// wrong from here — not a git repo, a name git will not take, a
		// directory already in the way — is answered by editing a field, and
		// closing the form would throw away the brief along with it.
		m.spawning = true
		m.notice = "Creating worktree " + branch + "…"
		return m, tuiResolveWorktreeCmd(m.api,
			tuiWorktreeRequest{Repo: m.worktreeRepoDir(), Branch: branch}, group, m.form.gen, req)
	}
	m.mode = tuiModeList
	m.spawning = true
	m.notice = "Spawning in group " + group + "…"
	return m, tuiSpawnCmd(m.api, group, req)
}

// worktreeRepoDir is the directory the new worktree's repo is looked up from:
// the directory field, falling back to the group's default — which is where a
// blank field spawns anyway — and then to nothing, which the daemon resolves
// as its own working directory, the last step of that same fallback chain.
//
// The agent then launches inside the resolved worktree rather than in this
// directory, which is what `tclaude agent spawn --worktree` does too.
func (m tuiModel) worktreeRepoDir() string {
	if dir := strings.TrimSpace(m.form.dir.Value()); dir != "" {
		return dir
	}
	return strings.TrimSpace(m.groupDefaultDir(m.selectedGroup()))
}

// openShellForm builds a fresh "new shell session" prompt, with the directory
// field on the one the console was started in — the directory the operator was
// standing in when they ran `agentd serve --tui`, and so the one they most
// likely mean. Tab extends it from there.
func (m tuiModel) openShellForm() tuiModel {
	form := tuiShellForm{
		dir:        newTUITextInput("  Directory: "),
		label:      newTUITextInput("  Label:     "),
		dirPrefill: m.startupDir,
	}
	form.dir.SetValue(m.startupDir)
	form.dir.CursorEnd()
	form.dir.Focus()
	m.shell = form
	m.mode = tuiModeShell
	return m
}

// moveShellField shifts focus through the shell form, wrapping, and moves the
// text-input focus with it. Both fields are text inputs, so unlike the spawn
// form there is no choice field to blur into.
func (m tuiModel) moveShellField(delta int) tuiModel {
	m.shell.field = ((m.shell.field+delta)%tuiShellFieldCount + tuiShellFieldCount) % tuiShellFieldCount
	m.shell.dir.Blur()
	m.shell.label.Blur()
	switch m.shell.field {
	case tuiShellFieldDir:
		m.shell.dir.Focus()
	case tuiShellFieldLabel:
		m.shell.label.Focus()
	}
	return m
}

func (m tuiModel) updateFocusedShellInput(msg tea.Msg) (tuiModel, tea.Cmd) {
	var cmd tea.Cmd
	switch m.shell.field {
	case tuiShellFieldDir:
		m.shell.dir, cmd = m.shell.dir.Update(msg)
	case tuiShellFieldLabel:
		m.shell.label, cmd = m.shell.label.Update(msg)
	}
	return m, cmd
}

// completingShellDir is the shell form's half of completingDir, with the same
// gates for the same reasons: completion reads the daemon host's filesystem
// outside any agent sandbox, so it is offered to an operator console on the
// daemon host only, and only on a path the operator has typed into — on the
// field as the form left it, Tab keeps its ordinary next-field job, which is
// the only way to reach Label at all.
func (m tuiModel) completingShellDir() bool {
	if !m.operator || !m.capabilities.completeLocalDir || m.shell.field != tuiShellFieldDir {
		return false
	}
	value := m.shell.dir.Value()
	return value != "" && value != m.shell.dirPrefill
}

func (m tuiModel) completeShellDir() tuiModel {
	completed, candidates := clcommon.CompleteDirPath(m.shell.dir.Value())
	m.shell.dir.SetValue(completed)
	m.shell.dir.CursorEnd()
	m.shell.dirSuggestions = candidates
	return m
}

// startShellSelected opens the shell form, refusing consoles that cannot run
// the launch. Starting a shell is host-local like attaching: it creates a tmux
// session on the daemon's own host, in the daemon's own filesystem and outside
// any agent sandbox, and then hands this terminal to it. It also goes nowhere
// near the daemon's /v1 API — there is no HTTP shape for it and no permission
// to check, because a shell session is not an agent — so the operator gate here
// is the whole gate, exactly as it is for attachSelected.
func (m tuiModel) startShellSelected() tuiModel {
	if !m.operator {
		m.notice = "Only an operator console can start a shell session."
		return m
	}
	if !m.capabilities.startLocalShell {
		m.notice = "This console cannot start a shell session — it does not share the daemon's host."
		return m
	}
	if m.startingShell {
		m.notice = "A shell session is already starting."
		return m
	}
	m.notice = ""
	return m.openShellForm()
}

// submitShell turns the shell form into a launch. A blank directory is left
// blank on purpose: session.StartShellSession then falls back to the daemon
// process's own working directory, which is what the field was prefilled with
// anyway.
func (m tuiModel) submitShell() (tuiModel, tea.Cmd) {
	if m.startingShell {
		m.notice = "A shell session is already starting."
		return m, nil
	}
	dir := strings.TrimSpace(m.shell.dir.Value())
	label := strings.TrimSpace(m.shell.label.Value())
	// The label is the tmux handle verbatim, so it is charset-gated before the
	// launch. The launch itself refuses an unsafe one anyway; checking here
	// keeps the form open on the field the operator has to fix, instead of
	// closing it and reporting the refusal over the listing.
	if err := session.ValidateSessionLabel(label); err != nil {
		m.notice = "Cannot use that label: " + err.Error()
		return m, nil
	}
	m.mode = tuiModeList
	m.startingShell = true
	m.notice = "Starting a shell session…"
	return m, tuiStartShellCmd(dir, label)
}

// tuiStartShell is the shell-session launch, indirected through a package var
// so tests can drive the form without a tmux server.
var tuiStartShell = session.StartShellSession

// tuiStartShellCmd creates the shell session on its own goroutine: the launch
// talks to tmux and the session DB, and the console must stay responsive while
// it does. It creates the session detached and returns its handle; attaching is
// the caller's own step, so bubbletea still owns the terminal when this lands.
func tuiStartShellCmd(dir, label string) tea.Cmd {
	return func() tea.Msg {
		created, err := tuiStartShell(dir, label)
		return tuiShellStartedMsg{created: created, err: err}
	}
}

// tuiAttachToPane hands this terminal to the tmux session named by target,
// indirected through a package var so tests can observe the target without a
// tmux server. inTmux reports whether agentd itself is running inside tmux.
var tuiAttachToPane = realTUIAttachToPane

func (m tuiModel) attachCmd(agentName, convID, tmuxSession string) tea.Cmd {
	if m.api != nil {
		return m.api.attach(agentName, convID, tmuxSession)
	}
	return tuiAttachToPane(agentName, tmuxSession, insideTmux())
}

// realTUIAttachToPane is the production handover, and it has two shapes
// because agentd's own terminal may already be a tmux client:
//
//   - Inside tmux, `switch-client` moves that client to the agent's session.
//     It returns immediately and the console keeps running in its own window,
//     so the operator can switch back with tmux's own keys. Attaching instead
//     would nest one session inside another, which tmux refuses.
//   - Outside tmux, `attach-session` under tea.ExecProcess: bubbletea releases
//     the terminal, tmux owns it until the operator detaches, and the console
//     repaints itself afterwards.
func realTUIAttachToPane(agentName, tmuxSession string, inTmux bool) tea.Cmd {
	target := clcommon.ExactTarget(tmuxSession)
	if inTmux {
		return func() tea.Msg {
			out, err := clcommon.TmuxCommand("switch-client", "-t", target).CombinedOutput()
			if err != nil {
				err = fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
			}
			return tuiAttachedMsg{agent: agentName, session: tmuxSession, err: err}
		}
	}
	cmd := clcommon.TmuxCommand("attach-session", "-t", target)
	return tea.ExecProcess(cmd, func(err error) tea.Msg {
		// tmux exits 1 on a plain detach, which is not a failure — the same
		// reading session.attachToSessionWithFlags applies.
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			err = nil
		}
		if err == nil {
			session.NormalizeTmuxPaneAfterDetach(tmuxSession)
		}
		return tuiAttachedMsg{agent: agentName, session: tmuxSession, err: err}
	})
}

// insideTmux reports whether this process is itself a tmux client.
func insideTmux() bool { return os.Getenv("TMUX") != "" }

// selectedRow is the row the cursor is on, and whether there is one — the
// listing can be empty, and it re-sorts under the cursor every two seconds.
func (m tuiModel) selectedRow() (tuiListRow, bool) {
	rows := m.visibleRows()
	if m.cursor < 0 || m.cursor >= len(rows) {
		return tuiListRow{}, false
	}
	return rows[m.cursor], true
}

// selectedAgent is selectedRow narrowed to an agent: false on an empty
// listing and false on a non-agent session row, so every agent-only move
// (resume, stop, retire, attach-to-an-agent) is inert on a session rather
// than acting on one under an agent's verbs.
func (m tuiModel) selectedAgent() (tuiAgentRow, bool) {
	row, ok := m.selectedRow()
	if !ok || row.isSession() {
		return tuiAgentRow{}, false
	}
	return row.agent, true
}

// enterSelected is what enter does on a row. On an agent it depends on
// whether it is up: an offline one is turned back on, a live one gets this
// terminal handed to its pane. Both are the move the operator wants next
// after picking that row, and an offline agent has no pane to attach to
// anyway — enter on it used to be a dead key that only said so. On a
// non-agent session there is only ever a live pane, so enter goes to it.
func (m tuiModel) enterSelected() (tuiModel, tea.Cmd) {
	row, ok := m.selectedRow()
	if !ok {
		return m, nil
	}
	if row.isSession() {
		return m.attachSessionSelected(row.session)
	}
	if !row.agent.Online {
		return m.resumeSelected(row.agent)
	}
	return m.attachSelected()
}

// attachSessionSelected hands this terminal to a non-agent session's pane —
// the console's `tclaude session attach <handle>`, and the same handover enter
// makes on a live agent's row.
//
// The gates are the shell form's, for the shell form's reasons: the pane is on
// the daemon's host, outside any agent sandbox, and it is the operator's own
// shell. A console the daemon does not classify as the human is refused, and
// so is one that does not share the host. In practice neither refusal is
// reachable from the listing — a console that fails either never sees a
// session row at all (listsLocalSessions) — but the check lives with the
// action rather than depending on the listing to have withheld it.
func (m tuiModel) attachSessionSelected(row tuiSessionRow) (tuiModel, tea.Cmd) {
	if !m.operator {
		m.notice = "Only an operator console can go to a session's terminal."
		return m, nil
	}
	if !m.capabilities.localSessions || !m.capabilities.attachLocalPane {
		m.notice = "This console cannot go to a session — it does not share the daemon's host."
		return m, nil
	}
	if row.TmuxSession == "" {
		m.notice = row.name() + " has no tmux session to go to."
		return m, nil
	}
	if insideTmux() {
		m.notice = "Switching to " + row.TmuxSession + "…"
	} else {
		m.notice = "Attaching to " + row.name() + " — detach (ctrl-b d) to come back."
	}
	return m, tuiAttachToPane(row.name(), row.TmuxSession, insideTmux())
}

// resumeSelected starts the selected agent's session again through the
// daemon's resume verb. It asks nothing first: resume is not destructive (it
// relaunches the agent in its recorded directory and conversation) and it is
// what the grey status dot does on the web dashboard.
//
// Permission is the daemon's call, not the console's — same as retire. An
// agent-class console holding agent.resume over its own group's members may
// use this; one that does not gets the endpoint's own refusal.
func (m tuiModel) resumeSelected(row tuiAgentRow) (tuiModel, tea.Cmd) {
	if m.resuming {
		m.notice = "A resume is already in flight."
		return m, nil
	}
	if row.ConvID == "" {
		// A member that has never had a conversation (a placeholder row) has
		// nothing to resume, and the daemon's own answer would be a 404 on a
		// path built from an empty selector.
		m.notice = row.name() + " has no conversation to start."
		return m, nil
	}
	m.resuming = true
	m.notice = "Starting " + row.name() + "…"
	return m, tuiResumeCmd(m.api, row.ConvID, row.name())
}

// tuiResumeSummary describes a landed resume in one line. Anything that is
// not a plain "resumed" carries its reason in Detail — a launch directory
// that no longer exists, provenance the daemon will not trust unattended —
// and this console has no browser to go and read that in, so the reason
// travels with the verdict or it is lost.
func tuiResumeSummary(msg tuiResumedMsg) string {
	var out string
	switch action := msg.res.Action; action {
	case "resumed":
		out = "Started " + msg.agent + "."
	case "":
		// No action at all: an older daemon, or a response shape that did not
		// carry one. Claiming a start that may not have happened is the one
		// reading to avoid — the listing settles it either way.
		out = "Asked the daemon to start " + msg.agent + "."
	case "skipped:already_online":
		out = msg.agent + " was already running."
	case "skipped:not_active_agent":
		// The agent was retired out from under the listing. That is not a
		// failure to report as one, and the raw wire token means nothing to
		// an operator.
		out = msg.agent + " has been retired, so it cannot be started from here."
	default:
		out = "Could not start " + msg.agent + ": " + action
		if detail := strings.TrimSpace(msg.res.Detail); detail != "" {
			out += " (" + detail + ")"
		}
		out += "."
	}
	if warnings := tuiTrimmedLines(msg.res.Warnings); len(warnings) > 0 {
		out += " Note: " + strings.Join(warnings, "; ") + "."
	}
	return out
}

// tuiTrimmedLines drops the blank entries from a wire string list and trims
// the rest, so a stray empty warning cannot render as an empty clause.
func tuiTrimmedLines(in []string) []string {
	var out []string
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// attachSelected puts the operator on the selected agent's tmux session — the
// console's answer to "let me actually go look at what this agent is doing".
//
// Both transports hand THIS terminal to the agent: the embedded API resolves
// the daemon-host tmux session directly, while the remote API streams the web
// dashboard's authenticated terminal WebSocket. The operator gate applies to
// both, so an agent-class console cannot use either path to reach a peer's pane
// and drive it.
func (m tuiModel) attachSelected() (tuiModel, tea.Cmd) {
	row, ok := m.selectedAgent()
	if !ok {
		return m, nil
	}
	if !m.operator {
		m.notice = "Only an operator console can attach to an agent's terminal."
		return m, nil
	}
	if !m.capabilities.attachAgent {
		m.notice = "This console cannot attach to an agent's terminal."
		return m, nil
	}
	if m.capabilities.attachLocalPane {
		sess := pickAliveSession(row.ConvID)
		if sess == nil || sess.TmuxSession == "" {
			m.notice = row.name() + " has no live tmux session to attach to."
			return m, nil
		}
		if insideTmux() {
			m.notice = "Switching to " + sess.TmuxSession + "…"
			return m, m.attachCmd(row.name(), row.ConvID, sess.TmuxSession)
		}
		m.notice = "Attaching to " + row.name() + " — detach (ctrl-b d) to come back."
		return m, m.attachCmd(row.name(), row.ConvID, sess.TmuxSession)
	}
	m.notice = "Opening remote terminal for " + row.name() + " — press ctrl-] d to come back."
	return m, m.attachCmd(row.name(), row.ConvID, "")
}

// confirmDeleteStepSelected moves the selected row one step toward removal.
// On an agent that is the lifecycle it already had: online agents are taken
// offline, while agents already offline are retired. On a non-agent session
// there is no such ladder — a session has no offline state to park in and
// nothing to retire from — so the one step is ending it. Every action asks
// first, and the target is captured here so a background refresh cannot move
// another row under the cursor before confirmation.
func (m tuiModel) confirmDeleteStepSelected() tuiModel {
	row, ok := m.selectedRow()
	if !ok {
		return m
	}
	if row.isSession() {
		if !m.operator || !m.capabilities.localSessions {
			m.notice = "Only an operator console on the daemon's host can end a session."
			return m
		}
		if m.killingSession {
			m.notice = "A session is already being ended."
			return m
		}
		m.lifecycleTarget = row
		m.mode = tuiModeConfirmKillSession
		return m
	}
	if row.agent.Online {
		if m.stopping {
			m.notice = "A stop is already in flight."
			return m
		}
		m.lifecycleTarget = row
		m.mode = tuiModeConfirmStop
		return m
	}
	if m.retiring {
		m.notice = "A retire is already in flight."
		return m
	}
	m.lifecycleTarget = row
	m.mode = tuiModeConfirmRetire
	return m
}

// stopConfirmed fires the graceful stop the operator just confirmed.
func (m tuiModel) stopConfirmed() (tuiModel, tea.Cmd) {
	row := m.lifecycleTarget.agent
	m.lifecycleTarget = tuiListRow{}
	m.mode = tuiModeList
	if row.ConvID == "" {
		return m, nil
	}
	m.stopping = true
	m.notice = "Taking " + row.name() + " offline…"
	return m, tuiStopCmd(m.api, row.ConvID, row.name())
}

// killSessionConfirmed ends the non-agent session the operator just confirmed.
// Unlike the agent verbs beside it this goes nowhere near the daemon's API:
// there is no permission to check on a session and no HTTP shape for it, so
// the operator gate captured when the prompt opened is the whole gate — the
// same arrangement attaching and the shell launch have.
func (m tuiModel) killSessionConfirmed() (tuiModel, tea.Cmd) {
	row := m.lifecycleTarget.session
	m.lifecycleTarget = tuiListRow{}
	m.mode = tuiModeList
	if row.TmuxSession == "" {
		return m, nil
	}
	m.killingSession = true
	m.notice = "Ending session " + row.name() + "…"
	return m, tuiKillSessionCmd(row)
}

// retireConfirmed fires the retire the operator just confirmed. Permission is
// the daemon's call, not the console's: the endpoint gates every caller class
// itself, so an agent-class console gets the same refusal here it would get
// over the socket rather than a second, divergent rule.
func (m tuiModel) retireConfirmed() (tuiModel, tea.Cmd) {
	row := m.lifecycleTarget.agent
	m.lifecycleTarget = tuiListRow{}
	m.mode = tuiModeList
	if row.ConvID == "" {
		return m, nil
	}
	m.retiring = true
	m.notice = "Retiring " + row.name() + "…"
	return m, tuiRetireCmd(m.api, row.ConvID, row.name())
}

// tuiStopSummary describes a landed stop in one line.
func tuiStopSummary(msg tuiStoppedMsg) string {
	detail := strings.TrimSpace(msg.res.Detail)
	withDetail := func(summary string) string {
		if detail != "" {
			summary += " Note: " + detail + "."
		}
		return summary
	}
	switch msg.res.Action {
	case "soft_stopped":
		return withDetail("Asked " + msg.agent + " to go offline — its session was asked to exit.")
	case "killed_no_soft_exit":
		return withDetail("Took " + msg.agent + " offline — its harness has no graceful exit, so the session was stopped.")
	case "skipped:already_offline":
		return withDetail(msg.agent + " was already offline.")
	case "":
		return withDetail("Asked the daemon to take " + msg.agent + " offline.")
	default:
		out := "Could not take " + msg.agent + " offline: " + msg.res.Action
		if detail != "" {
			out += " (" + detail + ")"
		}
		return out + "."
	}
}

// tuiRetireSummary describes a landed retire in one line: what the demotion
// gave up, and what happened to the pane.
func tuiRetireSummary(msg tuiRetiredMsg) string {
	out := "Retired " + msg.agent
	if groups := msg.res.Outcome.GroupsLeft; len(groups) > 0 {
		out += " — left " + strings.Join(groups, ", ")
	}
	switch msg.res.Shutdown.Action {
	case "":
		// No shutdown block at all: an older daemon, or a response shape
		// that did not carry one. Say nothing rather than guess.
	case "skipped:already_offline":
		out += "; it had no live session"
	case "soft_stopped":
		out += "; its session was asked to exit"
	default:
		// Anything else is a shutdown that did not go to plan — "error" above
		// all, which carries its reason in Detail. This console has no browser
		// to go and read that in, so the reason travels with the verdict or it
		// is lost: the operator would otherwise be told only that something
		// went wrong with a pane that is, in that case, still running.
		out += "; session: " + msg.res.Shutdown.Action
		if detail := strings.TrimSpace(msg.res.Shutdown.Detail); detail != "" {
			out += " (" + detail + ")"
		}
	}
	return out + "."
}

func (m tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.ensureCursorVisible()
		// A terminal that grew leaves the help view scrolled further than there
		// is now text below, which would show a screen of blank rows. Only the
		// open view pays for the recount; every other mode opens at the top
		// anyway.
		if m.mode == tuiModeHelp {
			m.helpOffset = min(m.helpOffset, m.helpMaxOffset(m.helpBodyLines()))
		}
		return m, nil

	case tuiTickMsg:
		cmds := []tea.Cmd{tuiTickCmd()}
		// Keep the header's dashboard link openable: a one-minute token has to
		// be replaced before it expires under the operator.
		m = m.refreshDashboardLink(time.Time(msg))
		// The usage readout rides this tick but on its own much slower cadence
		// — see tuiUsageInterval — and independently of the listing poll, so a
		// slow refresh cannot starve it.
		if m.usageDue() {
			m.usageFetching = true
			m.lastUsageAttempt = time.Now()
			cmds = append(cmds, m.usageCmd())
		}
		if !m.refreshing {
			var refresh tea.Cmd
			m, refresh = m.beginRefresh()
			cmds = append(cmds, refresh)
		}
		return m, tea.Batch(cmds...)

	case tuiUsageMsg:
		m.usageFetching = false
		if msg.err != nil {
			// A daemon without the endpoint is not a broken readout, it is a
			// readout that does not exist here: say nothing, and drop whatever
			// an earlier daemon told us — those figures have no source now.
			if tuiEndpointUnsupported(msg.err) {
				m.usageUnsupported = true
				m.usageLoaded = false
				m.usageFailed = false
				return m, nil
			}
			// Keep the last good figures if there are any: they are cached
			// readings to begin with, and one failed poll of an optional
			// readout must not blank a line the operator is reading.
			m.usageFailed = !m.usageLoaded
			return m, nil
		}
		m.usage = msg.usage
		m.usageLoaded = true
		m.usageFailed = false
		m.usageUnsupported = false
		return m, nil

	case tuiDataMsg:
		// Tests and other direct model callers may leave the generation at
		// zero; real refresh commands always carry one. A late result is
		// otherwise wholly stale, including its error and reconciliation
		// implications.
		if msg.refreshGeneration != 0 && msg.refreshGeneration < m.refreshGeneration {
			return m, nil
		}
		m.refreshing = false
		if msg.err != nil {
			m.refreshErr = "Refresh failed: " + msg.err.Error()
			return m, nil
		}
		m.refreshErr = ""
		if m.reconcilingMutation &&
			(msg.refreshGeneration == 0 || msg.refreshGeneration >= m.reconciliationRefreshGen) {
			m.reconcilingMutation = false
			m.reconciliationRefreshGen = 0
		}
		// Which row the cursor is on is decided before the listing under it
		// is replaced — see restoreCursor.
		selected, hadSelection := m.selectedRow()
		m.agents = msg.agents
		m.groups = msg.groups
		// A failed profile read keeps the last list it managed to fetch: a
		// stale picker is worth more than an empty one, and the form says so.
		if msg.profilesErr != nil {
			m.profilesErr = msg.profilesErr.Error()
		} else {
			m.profilesErr = ""
			m.profiles = msg.profiles
		}
		// Same disposition for the host's own sessions: a failed read keeps
		// the rows it last managed to list rather than dropping them out from
		// under the cursor, and the summary line says the listing is stale.
		if msg.sessionsErr != nil {
			m.sessionsErr = msg.sessionsErr.Error()
		} else {
			m.sessionsErr = ""
			m.sessions = msg.sessions
		}
		m.lastRefresh = time.Now()
		if hadSelection {
			m.restoreCursor(selected.key())
		}
		visible := m.visibleRows()
		if m.cursor >= len(visible) {
			m.cursor = max(len(visible)-1, 0)
		}
		m.ensureCursorVisible()
		return m, nil

	case tuiWorktreeResolvedMsg:
		if msg.err != nil {
			// The form is still open on the fields that produced this, so the
			// operator fixes the branch or the directory and presses enter
			// again. Nothing has been spawned.
			m.spawning = false
			m.notice = "Worktree failed: " + msg.err.Error()
			if tuiMutationOutcomeUnknown(msg.err) {
				// The request may well have landed: git creates the worktree
				// before the answer is lost, and the retries replay a recorded
				// response rather than re-cutting it. Saying only "failed"
				// would leave a directory nobody has been told about. Pressing
				// enter again converges either way — a worktree that exists on
				// the branch is reused.
				m.notice += " The worktree may have been created; asking for the same branch again reuses it."
			}
			return m, nil
		}
		m.spawnWorktree = msg.wt
		req := msg.req
		req.Cwd = msg.wt.Path
		// The form that asked has done its job, so it closes. A form the
		// operator has opened since is a different one, with different things
		// typed into it, and is theirs to keep — as is anything else they
		// opened instead.
		if m.mode == tuiModeSpawn && m.form.gen == msg.formGen {
			m.mode = tuiModeList
		}
		m.notice = tuiWorktreeReadyNotice(msg.wt) + " Spawning in group " + msg.group + "…"
		return m, tuiSpawnCmd(m.api, msg.group, req)

	case tuiSpawnedMsg:
		m.spawning = false
		if msg.err != nil {
			m.notice = "Spawn failed: " + msg.err.Error() + m.worktreeKeptNote()
			if tuiMutationOutcomeUnknown(msg.err) {
				m.notice = "Spawn outcome unknown: the connection was lost after the request; " +
					"refreshing before another action." + m.worktreeKeptNote()
				m.reconcilingMutation = true
				var refresh tea.Cmd
				m, refresh = m.beginRefresh()
				m.reconciliationRefreshGen = m.refreshGeneration
				return m, refresh
			}
			return m, nil
		}
		m.notice = tuiSpawnSummary(msg) + m.worktreeLandedNote()
		if focused, cmd := m.focusSpawned(msg); cmd != nil {
			// Going to the pane ends in a tuiAttachedMsg, which refreshes.
			return focused, cmd
		}
		// Pull the new agent in now rather than waiting out the tick.
		return m.beginRefresh()

	case tuiRetiredMsg:
		m.retiring = false
		if msg.err != nil {
			m.notice = "Retire failed: " + msg.err.Error()
			if tuiMutationOutcomeUnknown(msg.err) {
				m.notice = "Retire outcome unknown: the connection was lost after the request; refreshing before another action."
				m.reconcilingMutation = true
				var refresh tea.Cmd
				m, refresh = m.beginRefresh()
				m.reconciliationRefreshGen = m.refreshGeneration
				return m, refresh
			}
			return m, nil
		}
		m.notice = tuiRetireSummary(msg)
		// The row is gone (or offline) now — show that rather than leaving a
		// retired agent listed until the next tick.
		return m.beginRefresh()

	case tuiStoppedMsg:
		m.stopping = false
		if msg.err != nil {
			m.notice = "Take offline failed: " + msg.err.Error()
			if tuiMutationOutcomeUnknown(msg.err) {
				m.notice = "Take-offline outcome unknown: the connection was lost after the request; refreshing before another action."
				m.reconcilingMutation = true
				var refresh tea.Cmd
				m, refresh = m.beginRefresh()
				m.reconciliationRefreshGen = m.refreshGeneration
				return m, refresh
			}
			return m, nil
		}
		m.notice = tuiStopSummary(msg)
		// Reconcile promptly: a graceful stop acknowledges delivery of the
		// exit request, but the pane may take a moment to disappear.
		return m.beginRefresh()

	case tuiResumedMsg:
		m.resuming = false
		if msg.err != nil {
			m.notice = "Start failed: " + msg.err.Error()
			if tuiMutationOutcomeUnknown(msg.err) {
				m.notice = "Start outcome unknown: the connection was lost after the request; refreshing before another action."
				m.reconcilingMutation = true
				var refresh tea.Cmd
				m, refresh = m.beginRefresh()
				m.reconciliationRefreshGen = m.refreshGeneration
				return m, refresh
			}
			return m, nil
		}
		m.notice = tuiResumeSummary(msg)
		// The row is online now — show that rather than leaving it reading
		// "offline" until the next tick.
		return m.beginRefresh()

	case tuiShellStartedMsg:
		m.startingShell = false
		if msg.err != nil {
			m.notice = "Could not start a shell session: " + msg.err.Error()
			return m, nil
		}
		// A shell session is not an agent, so it lands in the listing as a
		// (session) row rather than beside the agents — and naming its tmux
		// handle is what also lets the operator find it from outside this
		// console (`tclaude session attach <handle>`).
		m.notice = "Started shell session " + msg.created.TmuxSession + " in " + msg.created.Cwd + "."
		if insideTmux() {
			m.notice += " Switching to it…"
		} else {
			m.notice += " Attaching; detach (ctrl-b d) to come back."
		}
		return m, tuiAttachToPane(msg.created.SessionID, msg.created.TmuxSession, insideTmux())

	case tuiSessionKilledMsg:
		m.killingSession = false
		if msg.err != nil {
			m.notice = "Could not end session " + msg.session + ": " + msg.err.Error()
			return m, nil
		}
		// The pane is gone, so the row is: the listing carries live sessions
		// only. Refresh now rather than leaving a dead session on screen for
		// up to a tick. The session row itself is left for the daemon's reaper
		// to mark exited, exactly as when the shell exits on its own.
		m.notice = "Ended session " + msg.session + "."
		return m.beginRefresh()

	case tuiAttachedMsg:
		if msg.err != nil {
			if msg.remote {
				m.notice = "Could not reach the remote terminal for " + msg.agent + ": " + msg.err.Error()
			} else {
				m.notice = "Could not reach " + msg.session + ": " + msg.err.Error()
			}
			return m, nil
		}
		if msg.remote {
			m.notice = "Back from the remote terminal for " + msg.agent + "."
		} else if insideTmux() {
			m.notice = "Switched to " + msg.session + " (" + msg.agent + ")."
		} else {
			m.notice = "Back from " + msg.session + " (" + msg.agent + ")."
		}
		// The pane may have ended while the operator was on it.
		return m.beginRefresh()

	case tea.KeyPressMsg:
		// The startup token block is a banner, not a mode: the first keystroke
		// retires it whatever else that keystroke does, and the help view
		// keeps the token reachable afterwards.
		m.showTokenBanner = false
		return m.handleKey(msg)
	}
	return m, nil
}

func tuiMutationOutcomeUnknown(err error) bool {
	var ambiguous *tuiAmbiguousMutationError
	return errors.As(err, &ambiguous)
}

// focusSpawned goes straight to a freshly spawned agent's pane — what the
// operator would do next anyway, and the reason they started it. It is enter
// on the new row, taken automatically: the same handover, the same gate.
//
// It stays out of the way in the two cases where that gate says no. A console
// the daemon does not classify as the human cannot attach at all (see
// attachSelected), and a spawn that has not produced a tmux session yet —
// a Codex agent held behind a startup gate — has no pane to go to. Both fall
// back to the ordinary "spawned, here it is in the listing" path.
func (m tuiModel) focusSpawned(msg tuiSpawnedMsg) (tuiModel, tea.Cmd) {
	session := msg.resp.TmuxSession
	convID := msg.resp.ConvID
	if !m.operator || !m.capabilities.attachAgent ||
		(m.capabilities.attachLocalPane && session == "") ||
		(!m.capabilities.attachLocalPane && convID == "") {
		return m, nil
	}
	name := msg.resp.AgentID
	if name == "" {
		name = msg.resp.ConvID
	}
	if name == "" {
		// Nothing has an identity yet; the pane's own name is what the
		// operator has to go on.
		name = session
	}
	if m.capabilities.attachLocalPane && insideTmux() {
		m.notice += " — switching to " + session + "…"
		return m, m.attachCmd(name, convID, session)
	}
	if m.capabilities.attachLocalPane {
		m.notice += " — attaching; detach (ctrl-b d) to come back."
	} else {
		m.notice += " — opening its remote terminal; press ctrl-] d to come back."
	}
	return m, m.attachCmd(name, convID, session)
}

// tuiSpawnSummary describes a landed spawn in one line. A Codex agent held
// behind a startup gate reports no conv-id yet, so say so rather than
// printing a blank identity.
func tuiSpawnSummary(msg tuiSpawnedMsg) string {
	id := msg.resp.AgentID
	if id == "" {
		id = msg.resp.ConvID
	}
	if id == "" {
		return fmt.Sprintf("Spawn accepted in group %s — the agent is still registering.", msg.group)
	}
	out := fmt.Sprintf("Spawned %s in group %s", id, msg.group)
	if msg.resp.TmuxSession != "" {
		out += " (tmux " + msg.resp.TmuxSession + ")"
	}
	return out
}

// tuiWorktreeReadyNotice says which of the two things the resolve step did.
// Reuse is not a corner case: naming the branch of a worktree that already
// exists is how this form picks an existing worktree, since it has no list to
// pick one from.
func tuiWorktreeReadyNotice(wt tuiWorktreeResponse) string {
	if wt.Created {
		return "Created worktree " + wt.Path + " on " + wt.Branch + "."
	}
	return "Reusing the existing worktree " + wt.Path + " on " + wt.Branch + "."
}

// worktreeLandedNote names the worktree the new agent launched in, so the
// listing's DIRECTORY column is not the first the operator hears of it.
func (m tuiModel) worktreeLandedNote() string {
	if m.spawnWorktree.Path == "" {
		return ""
	}
	return " — in " + m.spawnWorktree.Path
}

// worktreeKeptNote is what a failed spawn owes an operator whose worktree was
// created moments earlier: the directory is still there, and it is theirs to
// keep or remove.
//
// The console deliberately does not remove it. A failed spawn here is often an
// ambiguous one — the console latches reconcilingMutation precisely because it
// cannot tell a rejected request from a lost answer — and force-removing a
// directory a session may be starting up in is the one mistake that costs
// work. `tclaude worktree rm` removes it when the operator has decided.
func (m tuiModel) worktreeKeptNote() string {
	if !m.spawnWorktree.Created || m.spawnWorktree.Path == "" {
		return ""
	}
	return " The worktree " + m.spawnWorktree.Path + " was created and has been kept."
}

func (m tuiModel) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.reconcilingMutation && tuiReconciliationBlocksMutation(m.mode, msg.String()) {
		// A second modal can have opened while the original request was still
		// in flight. Once that request becomes ambiguous, cancel the stale
		// modal as well as blocking list-mode mutation shortcuts.
		m.mode = tuiModeList
		m.lifecycleTarget = tuiListRow{}
		m.notice = "Waiting for a successful refresh to reconcile the previous action before another mutation."
		return m, nil
	}

	switch m.mode {
	case tuiModeHelp:
		return m.handleHelpKey(msg)

	case tuiModeConfirmQuit:
		// Only "y" or a second Ctrl-C confirms. Enter is the reflexive dismiss key and the
		// prompt promises that anything other than "y" cancels — accepting
		// it here would turn a misread prompt into a daemon shutdown, taking
		// every managed agent's pane with it.
		switch msg.String() {
		case "y", "Y", "ctrl+c":
			return m, tea.Quit
		default:
			m.mode = tuiModeList
		}
		return m, nil

	case tuiModeConfirmStop:
		switch msg.String() {
		case "y", "Y":
			return m.stopConfirmed()
		default:
			m.mode = tuiModeList
			m.lifecycleTarget = tuiListRow{}
		}
		return m, nil

	case tuiModeConfirmKillSession:
		// Same contract as the retire prompt: only "y" goes through, and
		// ctrl-c means "get me out of this" rather than a second yes.
		switch msg.String() {
		case "y", "Y":
			return m.killSessionConfirmed()
		default:
			m.mode = tuiModeList
			m.lifecycleTarget = tuiListRow{}
		}
		return m, nil

	case tuiModeConfirmRetire:
		// Same contract as the quit prompt: only "y" goes through, and the
		// prompt says so. Ctrl-C is a cancel here rather than a second
		// confirmation — it means "get me out of this", and the destructive
		// reading of it belongs to the prompt that offers it.
		switch msg.String() {
		case "y", "Y":
			return m.retireConfirmed()
		default:
			m.mode = tuiModeList
			m.lifecycleTarget = tuiListRow{}
		}
		return m, nil

	case tuiModeSpawn:
		return m.handleSpawnKey(msg)

	case tuiModeShell:
		return m.handleShellKey(msg)
	}

	// List mode.
	visible := m.visibleRows()
	switch msg.String() {
	case "q", "esc", "ctrl+c":
		// The embedded console owns the daemon lifetime; a standalone remote
		// console owns only itself. Both ask so q remains hard to hit by
		// accident, while the prompt says exactly what will stop.
		m.mode = tuiModeConfirmQuit
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
			m.ensureCursorVisible()
		}
	case "down", "j":
		if m.cursor < len(visible)-1 {
			m.cursor++
			m.ensureCursorVisible()
		}
	case "f":
		var selectedKey string
		if m.cursor >= 0 && m.cursor < len(visible) {
			selectedKey = visible[m.cursor].key()
		}
		m.filterActive = !m.filterActive
		m.cursor = 0
		// Same key match — and the same skip of the empty one — restoreCursor
		// makes after a refresh: an identity-less placeholder row must not
		// hand the cursor to a different placeholder.
		m.restoreCursor(selectedKey)
		m.ensureCursorVisible()
		return m, nil
	case "enter":
		m.notice = ""
		return m.enterSelected()
	case "n", "N":
		m.notice = ""
		return m.openSpawnForm(), nil
	case "s", "S":
		return m.startShellSelected(), nil
	case "delete":
		m.notice = ""
		return m.confirmDeleteStepSelected(), nil
	case "r":
		if !m.refreshing {
			m.notice = ""
			return m.beginRefresh()
		}
	case "?", "h":
		m.mode = tuiModeHelp
		// Always opens at the top: the view is opened to read it, and where a
		// previous reading was left is no use two minutes later.
		m.helpOffset = 0
	}
	return m, nil
}

func tuiReconciliationBlocksMutation(mode tuiMode, key string) bool {
	switch mode {
	case tuiModeList:
		return key == "enter" || key == "delete" || strings.EqualFold(key, "n")
	case tuiModeSpawn:
		return key == "enter"
	case tuiModeConfirmStop:
		return strings.EqualFold(key, "y")
	case tuiModeConfirmRetire:
		return strings.EqualFold(key, "y")
	case tuiModeConfirmKillSession:
		return strings.EqualFold(key, "y")
	default:
		return false
	}
}

// handleHelpKey scrolls the help view, or closes it. Anything that is not a
// scroll key closes it, which is the contract the view has always had and the
// footer still states: the help is a glance rather than a mode to get stuck
// in, so the keys it swallows are only the ones it needs.
//
// The scroll keys are the listing's own — ↑/k, ↓/j, pgup/pgdn, home/g, end/G
// — so nothing new has to be learned to read the rest of a page.
func (m tuiModel) handleHelpKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	lines := m.helpBodyLines()
	maxOffset := m.helpMaxOffset(lines)
	if maxOffset <= 0 {
		// The whole text is on screen, so the footer promises that any key
		// closes it — and a scroll key must not be the exception that leaves an
		// operator pressing ↓ at a view that neither scrolls nor closes.
		m.mode = tuiModeList
		m.helpOffset = 0
		return m, nil
	}
	// A page is what is on screen less one line, so the line the eye stopped on
	// is still there after the jump.
	page := max(len(m.helpWindow(lines, m.helpOffset))-1, 1)
	switch msg.String() {
	case "up", "k":
		m.helpOffset = max(m.helpOffset-1, 0)
	case "down", "j":
		m.helpOffset = min(m.helpOffset+1, maxOffset)
	case "pgup", "ctrl+b":
		m.helpOffset = max(m.helpOffset-page, 0)
	case "pgdown", "ctrl+f":
		m.helpOffset = min(m.helpOffset+page, maxOffset)
	case "home", "g":
		m.helpOffset = 0
	case "end", "G":
		m.helpOffset = maxOffset
	default:
		m.mode = tuiModeList
		m.helpOffset = 0
	}
	return m, nil
}

func (m tuiModel) handleSpawnKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	// The candidate list belongs to the Tab that produced it: anything else
	// (including a Tab on another field) retires it, so it never lingers as a
	// stale answer to a path the operator has since edited.
	if key != "tab" || !m.completingDir() {
		m.form.dirSuggestions = nil
	}
	switch key {
	case "esc", "ctrl+c":
		// Closing the form while its worktree is being cut does not cancel the
		// spawn behind it — no mutation this console starts is cancellable —
		// it just gives up the typed fields. The outcome then lands on the
		// listing like every other one, and a second spawn is still refused
		// while the first is in flight.
		m.mode = tuiModeList
		return m, nil
	case "enter":
		return m.submitSpawn()
	case "up", "shift+tab":
		return m.moveSpawnField(-1), nil
	case "down":
		return m.moveSpawnField(1), nil
	case "tab":
		// Tab only completes a directory the operator has typed into; on the
		// field as the form left it — blank, or the group's own directory —
		// it keeps its ordinary next-field job (see completingDir).
		if m.completingDir() {
			return m.completeDir(), nil
		}
		return m.moveSpawnField(1), nil
	case "left":
		if tuiIsChoiceField(m.form.field) {
			return m.cycleChoice(-1), nil
		}
	case "right", " ":
		if tuiIsChoiceField(m.form.field) {
			return m.cycleChoice(1), nil
		}
	}
	updated, cmd := m.updateFocusedInput(msg)
	return updated, cmd
}

func (m tuiModel) handleShellKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	// The candidate list belongs to the Tab that produced it, exactly as in the
	// spawn form: anything else retires it, so it never lingers as a stale
	// answer to a path the operator has since edited.
	if key != "tab" || !m.completingShellDir() {
		m.shell.dirSuggestions = nil
	}
	switch key {
	case "esc", "ctrl+c":
		m.mode = tuiModeList
		return m, nil
	case "enter":
		return m.submitShell()
	case "up", "shift+tab":
		return m.moveShellField(-1), nil
	case "down":
		return m.moveShellField(1), nil
	case "tab":
		if m.completingShellDir() {
			return m.completeShellDir(), nil
		}
		return m.moveShellField(1), nil
	}
	return m.updateFocusedShellInput(msg)
}

// restoreCursor puts the cursor back on the row it was on, by row key, after
// a refresh has replaced the listing under it. A row that is no longer listed
// (an agent retired, a session ended, or either filtered out) leaves the
// cursor where it was, for the clamp above to bring back into range.
//
// The listing re-sorts on every poll — online agents first, then by name, then
// the sessions — so an index alone does not name a row for longer than two
// seconds. That is the same hazard the lifecycle prompts capture their target
// for, and it bites hardest right after enter starts an offline agent: that
// agent jumps from the bottom of the agents to the top, and a cursor left at
// the old index would put the next keystroke on whichever row slid into its
// place.
func (m *tuiModel) restoreCursor(key string) {
	if key == "" {
		return
	}
	for i, row := range m.visibleRows() {
		if row.key() == key {
			m.cursor = i
			return
		}
	}
}

func (m *tuiModel) ensureCursorVisible() {
	height := m.viewportHeight()
	if m.cursor < m.viewportOffset {
		m.viewportOffset = m.cursor
	}
	if m.cursor >= m.viewportOffset+height {
		m.viewportOffset = m.cursor - height + 1
	}
	if m.viewportOffset < 0 {
		m.viewportOffset = 0
	}
}

// ---- view ------------------------------------------------------------------

func (m tuiModel) View() tea.View {
	switch m.mode {
	case tuiModeHelp:
		return tea.View{Content: m.renderHelpView(), AltScreen: true}
	case tuiModeSpawn:
		return tea.View{Content: m.renderSpawnForm(), AltScreen: true}
	case tuiModeShell:
		return tea.View{Content: m.renderShellForm(), AltScreen: true}
	}
	return tea.View{Content: m.renderList(), AltScreen: true}
}

func (m tuiModel) columns() []table.Column {
	return []table.Column{
		{Header: "AGENT", MinWidth: 12, MaxWidth: 28, Weight: 0.2, Truncate: true},
		{Header: "GROUP", MinWidth: 8, MaxWidth: 20, Weight: 0.15, Truncate: true},
		{Header: "STATUS", MinWidth: 8, MaxWidth: 18, Weight: 0.15, Truncate: true},
		{Header: "HARNESS", Width: 8},
		{Header: "DIRECTORY", MinWidth: 15, Weight: 0.35, Truncate: true, TruncateMode: table.TruncateStart},
		{Header: "BRANCH", MinWidth: 10, MaxWidth: 24, Weight: 0.15, Truncate: true},
	}
}

// tuiOperatorTokenLines renders the operator-token block, mirroring the TTY
// branch of writeOperatorTokenBanner (same phrasing, same provenance note).
//
// Emitting the secret here keeps that function's rule intact rather than
// bending it: the token is shown only on a terminal the operator is sitting
// at, which is the one thing a bubbletea program is guaranteed to have.
func tuiOperatorTokenLines(tok string, src tokenSource) []string {
	if tok == "" {
		return nil
	}
	lines := []string{
		"Operator token — sign in to the web dashboard with it, or export it for the CLI:",
		"  export " + humanTokenEnvVar + "=" + strconv.Quote(tok),
	}
	switch src.kind {
	case tokenSourceKeychain:
		lines = append(lines, "  (persisted in the OS keychain — stable across restarts, export once)")
	case tokenSourceFile:
		lines = append(lines, "  (persisted at "+src.path+" — stable across restarts, export once)")
	}
	return lines
}

// writeTokenBlock writes the token lines at the given indent.
func writeTokenBlock(b *strings.Builder, lines []string, indent string) {
	for _, line := range lines {
		b.WriteString(indent + line + "\n")
	}
}

func (m tuiModel) renderList() string {
	var b strings.Builder
	b.WriteString("\n  tclaude terminal dashboard")
	if m.remoteConsole() {
		b.WriteString(" • connected to " + m.connectionLabel)
	} else if addr := m.dashboardAddressLine(); addr != "" {
		// Its own line: with the init token on it the address is too long to
		// share the title line without wrapping, and a wrapped header throws
		// off the row budget viewportHeight computes.
		b.WriteString(" • web dashboard:\n    " + addr)
	} else {
		b.WriteString(" (no web dashboard in this mode)")
	}
	b.WriteString("\n\n")
	if m.showTokenBanner {
		writeTokenBlock(&b, m.tokenLines, "  ")
		b.WriteString("  (hidden on your next keystroke — press ? to see it again)\n\n")
	}
	if m.identityWarning != "" {
		b.WriteString(m.renderWrapped(m.identityWarning) + "\n\n")
	}

	visible := m.visibleRows()
	if len(visible) == 0 {
		if m.filterActive && len(m.agents) > 0 {
			b.WriteString("  No active agents.\n")
		} else {
			b.WriteString("  " + m.emptyListingLine() + "\n")
		}
	} else {
		tbl := table.New(m.columns()...)
		tbl.SetTerminalWidth(max(m.width-4, 60))
		tbl.SelectedIndex = m.cursor
		tbl.ViewportOffset = m.viewportOffset
		tbl.ViewportHeight = m.viewportHeight()
		for _, row := range visible {
			tbl.AddRow(table.Row{Cells: []string{
				row.name(),
				row.groupCell(),
				row.statusCell(),
				row.harnessCell(),
				row.dirCell(),
				row.branchCell(),
			}})
		}
		for line := range strings.SplitSeq(tbl.Render(), "\n") {
			b.WriteString("  " + line + "\n")
		}
	}

	b.WriteString("\n  " + m.summaryLine() + "\n")
	if m.refreshErr != "" {
		b.WriteString(m.renderWrapped(m.refreshErr) + "\n")
	}
	if m.notice != "" {
		b.WriteString(m.renderWrapped(m.notice) + "\n")
	}
	b.WriteString("\n")
	if usage := m.usageLine(); usage != "" {
		b.WriteString("  " + usage + "\n")
	}
	b.WriteString("  " + m.keyHintLine() + "\n")

	if prompt := m.confirmPrompt(); prompt != "" {
		b.WriteString("\n  " + prompt + "\n")
	}
	return b.String()
}

// emptyListingLine is what an empty listing says, which depends on what this
// console was looking at: a console that lists the host's sessions has checked
// for those too, and saying only "no agents" would leave the operator
// wondering whether the shell they started is somewhere off screen.
func (m tuiModel) emptyListingLine() string {
	if m.listsLocalSessions() {
		return "No agents or sessions yet."
	}
	return "No agents yet."
}

// confirmPrompt is the question the list view is waiting on, empty when it is
// waiting on none. Every prompt is one line by contract — viewportHeight pays
// for exactly two (the blank line and the question), so a second line would
// overflow the terminal.
func (m tuiModel) confirmPrompt() string {
	switch m.mode {
	case tuiModeConfirmQuit:
		if !m.capabilities.shutdownOnQuit {
			return "Quit this remote console? [y / any other key = cancel]"
		}
		if m.startingShell {
			// A shell launch runs in THIS process, unlike an agent spawn — which
			// forks its own `tclaude session new`. Quitting mid-launch kills it
			// between writing its session row and arming the pane's exit guard, so
			// the rollback never runs and a launch-pending row is left behind.
			// Cheap to clean up, worth a word before it happens. Kept inside 80
			// columns: confirmPrompt is budgeted as exactly one line.
			return "Shell still starting — quit and shut down agentd? [y / any other key = cancel]"
		}
		if m.ownsTmuxServer {
			// This daemon started the tmux server, so quitting ends it too — but
			// only if nothing is left on it, so agent panes still outlive the quit
			// (see startTUITmuxServer). Saying so is the difference between an
			// operator who knows the empty server goes away with the console and
			// one who finds a stray tmux process later and wonders whose it is.
			return "Quit + shut down agentd (and tmux if empty)? [y / any other key = cancel]"
		}
		return "Quit and shut down agentd? [y / any other key = cancel]"
	case tuiModeConfirmStop:
		const prefix = "Take "
		const suffix = " offline? [y / any other key = cancel]"
		return prefix +
			tuiTruncate(m.lifecycleTarget.name(), m.promptNameBudget(len(prefix)+len(suffix))) +
			suffix
	case tuiModeConfirmRetire:
		const prefix = "Retire "
		const suffix = "? [y / any other key = cancel]"
		return prefix +
			tuiTruncate(m.lifecycleTarget.name(), m.promptNameBudget(len(prefix)+len(suffix))) +
			suffix
	case tuiModeConfirmKillSession:
		// "Kill" rather than the agents' softer wording on purpose: there is no
		// graceful exit to ask a shell for and nothing to resume it from
		// afterwards, so whatever is running in that pane stops now. The
		// prompt is budgeted as one line like the others, so the consequence
		// is spelled out in the help rather than here.
		const prefix = "Kill session "
		const suffix = "? [y / any other key = cancel]"
		return prefix +
			tuiTruncate(m.lifecycleTarget.name(), m.promptNameBudget(len(prefix)+len(suffix))) +
			suffix
	default:
		return ""
	}
}

// promptNameBudget is how many columns a lifecycle prompt may spend on the
// agent's name: what the terminal leaves after the indent and the fixed text,
// capped at the AGENT column's own width so a name that fits the listing reads
// identically here.
//
// The name is a conversation title — harness- or operator-authored, routinely
// long — and the prompt is budgeted as exactly one line, so an uncapped title
// is the one thing that could push the list view past the terminal's last row.
// On a terminal too narrow for even the fixed text the floor wins and the line
// wraps; that is the quit prompt's existing behaviour, not a new failure.
func (m tuiModel) promptNameBudget(fixed int) int {
	if m.width <= 0 {
		// No WindowSizeMsg yet: fall back to the listing's own cap.
		return tuiPromptNameWidth
	}
	const indent = 2
	return max(min(m.width-indent-fixed, tuiPromptNameWidth), tuiPromptNameFloor)
}

const (
	// tuiPromptNameWidth matches the AGENT column's MaxWidth.
	tuiPromptNameWidth = 28
	// tuiPromptNameFloor is the shortest name worth showing: below this the
	// prompt would name an agent the operator cannot recognise.
	tuiPromptNameFloor = 8
)

// tuiTruncate shortens s to at most width columns, marking a cut with "…".
func tuiTruncate(s string, width int) string {
	if lipgloss.Width(s) <= width {
		return s
	}
	if width <= 0 {
		return ""
	}
	runes := []rune(s)
	for len(runes) > 0 {
		candidate := string(runes) + "…"
		if lipgloss.Width(candidate) <= width {
			return candidate
		}
		runes = runes[:len(runes)-1]
	}
	return ""
}

// enterHint names what enter does on the row the cursor is on, empty when
// it would do nothing: an empty list has no row, and only an operator console
// can attach to a live agent's pane. Starting an offline one is advertised to
// every console — like retire, it goes through the daemon's own verb, and the
// daemon decides whether this caller may.
func (m tuiModel) enterHint() string {
	if m.reconcilingMutation {
		return ""
	}
	row, ok := m.selectedRow()
	if !ok {
		return ""
	}
	if row.isSession() {
		// A session row is only ever listed by a console that can go to it,
		// and it is always live, so enter always means the handover.
		if insideTmux() {
			return "enter switch to"
		}
		return "enter attach"
	}
	switch {
	case !row.agent.Online:
		return "enter start"
	case !m.operator || !m.capabilities.attachAgent:
		return ""
	case m.capabilities.attachLocalPane && insideTmux():
		return "enter switch to"
	case !m.capabilities.attachLocalPane:
		return "enter remote attach"
	default:
		return "enter attach"
	}
}

// deleteHint names what delete does on the row the cursor is on, empty when
// there is no row. The two kinds have different lifecycles behind that key —
// an agent steps offline then retired, a session just ends — so the hint is
// per-row rather than a fixed label for a non-empty list.
func (m tuiModel) deleteHint() string {
	row, ok := m.selectedRow()
	switch {
	case !ok:
		return ""
	case row.isSession():
		return "del kill session"
	default:
		// Unlike attaching, agent lifecycle moves are not statically
		// operator-only: an agent-class console can hold stop/retire
		// permissions over its own group's members, so the daemon decides and
		// the key stays advertised.
		return "del offline / retire"
	}
}

// keyHintLine names enter only when it can do something — see enterHint.
func (m tuiModel) keyHintLine() string {
	filterLabel := "f filter active"
	if m.filterActive {
		filterLabel = "f show all"
	}
	quitHint := "q quit"
	if m.capabilities.shutdownOnQuit {
		quitHint += " (shuts down agentd)"
	}
	if m.reconcilingMutation {
		return "reconciling unknown outcome • r refresh • " + filterLabel + " • ↑/↓ move • ? help • " + quitHint
	}
	newHints := "n new agent"
	if m.operator && m.capabilities.startLocalShell {
		newHints += " • s new shell"
	}
	hints := newHints + " • " + filterLabel + " • r refresh • ↑/↓ move • ? help • " + quitHint
	if del := m.deleteHint(); del != "" {
		hints = del + " • " + hints
	}
	if enter := m.enterHint(); enter != "" {
		hints = enter + " • " + hints
	}
	return hints
}

func (m tuiModel) summaryLine() string {
	online := 0
	for _, a := range m.agents {
		if a.Online {
			online++
		}
	}
	agentsShown := len(m.visibleAgents())
	shown := len(m.visibleRows())
	var line string
	if m.filterActive {
		line = fmt.Sprintf("%d active agents • %d groups", agentsShown, len(m.groups))
	} else {
		line = fmt.Sprintf("%d agents (%d online) • %d groups", agentsShown, online, len(m.groups))
	}
	// Sessions are counted separately from the agents they sit under: they are
	// a different kind of thing, and folding them into "N agents" would make
	// the roster read as bigger than it is.
	if m.listsLocalSessions() {
		line += fmt.Sprintf(" • %d sessions", len(m.sessions))
		if m.sessionsErr != "" {
			line += " (stale)"
		}
	}
	if !m.lastRefresh.IsZero() {
		line += " • updated " + tuiAgo(time.Since(m.lastRefresh))
	}
	if height := m.viewportHeight(); shown > height {
		first := m.viewportOffset + 1
		last := min(m.viewportOffset+height, shown)
		line += fmt.Sprintf(" • showing %d-%d", first, last)
	}
	if m.spawning {
		line += " • spawning…"
	}
	if m.resuming {
		line += " • starting…"
	}
	if m.stopping {
		line += " • taking offline…"
	}
	if m.startingShell {
		line += " • starting a shell…"
	}
	if m.killingSession {
		line += " • ending a session…"
	}
	return line
}

// tuiAgo renders how long ago the last successful refresh landed. It stays
// coarse on purpose: the point is telling a live listing apart from one
// frozen by a failing poll, not counting seconds.
func tuiAgo(d time.Duration) string {
	switch {
	case d < 5*time.Second:
		return "just now"
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
}

func (m tuiModel) renderSpawnForm() string {
	var b strings.Builder
	b.WriteString("\n  New agent\n\n")

	group := m.selectedGroup()
	if group == "" {
		group = "(no groups — create one with `tclaude agent groups create <name>`)"
	}
	b.WriteString(tuiChoiceLine("  Group:     ", group, "", m.form.field == tuiFieldGroup))
	b.WriteString(tuiChoiceLine("  Profile:   ", m.profileChoice(), m.profileChoiceHint(),
		m.form.field == tuiFieldProfile))
	b.WriteString(m.form.name.View() + tuiHint(m.form.name.Value() == "", "  (blank = auto-generated)"))
	b.WriteString("\n")
	b.WriteString(m.form.dir.View() + m.dirHint())
	b.WriteString("\n")
	// Always emit this line, blank or not, so a candidate list appearing and
	// disappearing doesn't shift the fields below it up and down.
	b.WriteString(m.dirSuggestionLine(m.form.dirSuggestions) + "\n")
	b.WriteString(m.worktreeLines())
	harnessChoice := tuiHarnessDefault
	if m.form.harnessIdx >= 0 && m.form.harnessIdx < len(m.form.harnessNames) {
		harnessChoice = m.form.harnessNames[m.form.harnessIdx]
	}
	b.WriteString(tuiChoiceLine("  Harness:   ", harnessChoice,
		tuiHint(harnessChoice == tuiHarnessDefault,
			"  (the profile chain decides — "+harness.DefaultName+" if nothing pins one)"),
		m.form.field == tuiFieldHarness))
	b.WriteString(m.form.brief.View() + m.briefHint())
	b.WriteString("\n")

	if m.notice != "" {
		b.WriteString("\n  " + m.notice + "\n")
	}
	// Name tab-completion only where it works, the same way keyHintLine names
	// enter only for a console that can attach.
	completeHint := ""
	if m.operator && m.capabilities.completeLocalDir {
		completeHint = "tab complete dir • "
	}
	spawnHint := "enter spawn"
	if m.operator && m.capabilities.attachAgent {
		spawnHint = "enter spawn + go to its pane"
	}
	cycleHint := "←/→ change group/profile/harness"
	if m.canCreateWorktree() {
		cycleHint = "←/→ change group/profile/worktree/harness"
	}
	b.WriteString("\n  " + spawnHint + " • ↑/↓/tab next field • " + completeHint +
		cycleHint + " • esc cancel\n")
	return b.String()
}

func (m tuiModel) renderShellForm() string {
	var b strings.Builder
	b.WriteString("\n  New shell session\n\n")
	b.WriteString("  A plain interactive shell in its own tmux session — a session, not an\n")
	b.WriteString("  agent: no conversation and no group, so it joins the list behind this\n")
	b.WriteString("  form as a \"" + tuiSessionGroupCell + "\" row. Also `tclaude session ls`.\n\n")

	b.WriteString(m.shell.dir.View() + m.shellDirHint())
	b.WriteString("\n")
	// Always emit this line, blank or not, so a candidate list appearing and
	// disappearing doesn't shift the fields below it up and down.
	b.WriteString(m.dirSuggestionLine(m.shell.dirSuggestions) + "\n")
	b.WriteString(m.shell.label.View() +
		tuiHint(m.shell.label.Value() == "",
			"  (the tmux handle: letters, digits, '-' and '_'; blank generates one)"))
	b.WriteString("\n")

	if m.notice != "" {
		b.WriteString("\n  " + m.notice + "\n")
	}
	completeHint := ""
	if m.operator && m.capabilities.completeLocalDir {
		completeHint = "tab complete dir • "
	}
	b.WriteString("\n  enter start + attach • ↑/↓/tab next field • " + completeHint + "esc cancel\n")
	return b.String()
}

// shellDirHint explains the directory field's state: a blank field lands in the
// daemon's own working directory, and an untouched prefill says it is the
// directory this console was started in rather than a path someone typed.
func (m tuiModel) shellDirHint() string {
	value := m.shell.dir.Value()
	switch {
	case value == "":
		return "  (blank = the directory agentd is running in)"
	case m.shell.dirPrefill != "" && value == m.shell.dirPrefill:
		return "  (where this console was started — edit it to start elsewhere)"
	default:
		return ""
	}
}

// worktreeLines renders the worktree picker and, when it is asking for a new
// worktree, the branch field under it.
//
// A console that may not create worktrees still gets the line, saying so: the
// field is one of the form's answers, and silently dropping it would leave an
// operator wondering whether this console simply has an older form.
func (m tuiModel) worktreeLines() string {
	if !m.canCreateWorktree() {
		// Rendered without the < > a picker has: there is nothing to cycle.
		return "  Worktree:  " + tuiWorktreeNone +
			"  (operator consoles only — the worktree is created on the daemon's host)\n"
	}
	choice := tuiWorktreeNone
	if m.form.worktreeIdx >= 0 && m.form.worktreeIdx < len(m.form.worktreeNames) {
		choice = m.form.worktreeNames[m.form.worktreeIdx]
	}
	out := tuiChoiceLine("  Worktree:  ", choice, m.worktreeChoiceHint(),
		m.form.field == tuiFieldWorktree)
	if !m.creatingWorktree() {
		return out
	}
	return out + m.form.branch.View() + m.branchHint() + "\n"
}

// worktreeChoiceHint says what each setting of the picker does, in the terms
// the operator is choosing between: where the agent ends up.
func (m tuiModel) worktreeChoiceHint() string {
	if !m.creatingWorktree() {
		return "  (the agent starts in the directory above)"
	}
	return "  (cut from the repo the directory above is in; the agent starts inside it)"
}

// branchHint explains the branch field's state: whether it is still following
// the name, that an existing branch is checked out rather than refused, and —
// on a blank one — that the submit will ask for a name rather than invent one.
func (m tuiModel) branchHint() string {
	switch {
	case strings.TrimSpace(m.form.branch.Value()) == "":
		return "  (name the branch, or set Worktree to " + tuiWorktreeNone + ")"
	case m.form.branchSynced:
		return "  (following the name — type here to choose your own)"
	default:
		return "  (an existing branch is checked out; a new one is cut from the default branch)"
	}
}

// dirHint explains the directory field's state: a blank field still lands in
// the group's default directory (the daemon's own fallback), and an untouched
// prefill says it is that same directory rather than a path someone typed —
// which is what makes it obvious it can be extended.
func (m tuiModel) dirHint() string {
	value := m.form.dir.Value()
	switch {
	case value == "":
		return "  (blank = the group's default directory)"
	case m.form.dirPrefill != "" && value == m.form.dirPrefill:
		return "  (the group's directory — add a subdirectory to start below it)"
	default:
		return ""
	}
}

// profileChoice is the profile picker's current value.
func (m tuiModel) profileChoice() string {
	if m.form.profileIdx >= 0 && m.form.profileIdx < len(m.form.profileNames) {
		return m.form.profileNames[m.form.profileIdx]
	}
	return tuiProfileDefault
}

// profileChoiceHint says what the picker's state means. "(default)" is the
// easy one to misread — it looks like "no profile", when what it actually
// does is leave the group's and the global default profile in force.
//
// An empty picker is the other misreadable state, and it has three causes
// that ask different things of the operator: the list has not arrived, the
// list could not be read, or there really are no profiles. Only the last one
// is worth pointing at `profiles create`.
func (m tuiModel) profileChoiceHint() string {
	if m.profileChoice() != tuiProfileDefault {
		return ""
	}
	if len(m.form.profileNames) <= 1 {
		switch {
		case m.profilesErr != "":
			return "  (profile list unavailable — press r to retry)"
		case m.lastRefresh.IsZero():
			return "  (profile list has not loaded yet — press r)"
		default:
			return "  (none saved — `tclaude agent profiles create`)"
		}
	}
	return "  (the group's or global default profile still applies)"
}

// dirSuggestionLine renders the Tab-completion candidates on a single line,
// dropping the ones that don't fit. A directory with many children would
// otherwise wrap and push the rest of the form around — the one thing the
// always-emitted line above is there to prevent.
func (m tuiModel) dirSuggestionLine(suggestions []string) string {
	if len(suggestions) == 0 {
		return ""
	}
	const indent = "  "
	if m.width <= len(indent) {
		// No width yet (no WindowSizeMsg): show them all rather than nothing.
		return indent + strings.Join(suggestions, "  ")
	}
	line := indent
	for i, name := range suggestions {
		next := name
		if i > 0 {
			next = "  " + name
		}
		more := fmt.Sprintf("  (+%d more)", len(suggestions)-i)
		if lipgloss.Width(line+next) > m.width {
			if lipgloss.Width(line+more) <= m.width {
				line += more
			}
			break
		}
		line += next
	}
	return line
}

// tuiChoiceLine renders one of the cycled fields, with an optional trailing
// hint. The focused one is marked with a caret rather than a color, since the
// console has no palette (the text fields mark focus with their own cursor).
func tuiChoiceLine(label, value, hint string, focused bool) string {
	if focused {
		label = "> " + strings.TrimPrefix(label, "  ")
	}
	return label + "< " + value + " >" + hint + "\n"
}

// tuiHint appends an inline explanation to an empty field.
// briefHint says what an empty Brief box actually produces. Usually nothing —
// but a selected profile carrying a task-brief default fills it, and an agent
// arriving with a brief the operator never typed, from a box that read "blank =
// no startup brief", is exactly the surprise this line exists to prevent.
func (m tuiModel) briefHint() string {
	if m.form.brief.Value() != "" {
		return ""
	}
	if prof, ok := m.selectedProfileRow(); ok && strings.TrimSpace(prof.InitialMessage) != "" {
		return "  (blank = the profile's own brief)"
	}
	return "  (blank = no startup brief)"
}

func tuiHint(show bool, hint string) string {
	if !show {
		return ""
	}
	return hint
}

// renderHelp is the whole help text: every body line with the closing hint
// under it. renderHelpView is what the console actually shows — it windows the
// same body to the terminal — so this is the text in full, whatever the
// terminal can hold of it.
func (m tuiModel) renderHelp() string {
	return strings.Join(m.helpBodyLines(), "\n") + "\n\n" + tuiHelpCloseHint + "\n"
}

// tuiHelpCloseHint is the footer under a help view that fits on the terminal,
// and tuiHelpScrollHint the one under a view that does not. Both are one row
// by contract — renderHelpView trims rather than wraps them, as the usage line
// does — so the body's row budget is fixed at tuiHelpChrome.
const (
	tuiHelpCloseHint  = "  Press any key to close."
	tuiHelpScrollHint = "  ↑/↓ scroll • pgup/pgdn page • home/end jump • any other key closes"
)

// tuiHelpChrome is what renderHelpView spends on everything that is not a body
// line: the blank line above the footer, and the footer.
const tuiHelpChrome = 2

// helpBodyLines is the help text as terminal lines, without the footer pinned
// under it. The console scrolls this slice rather than the rendered string, so
// a body taller than the terminal stays reachable to its last line.
func (m tuiModel) helpBodyLines() []string {
	var b strings.Builder
	b.WriteString("\n  tclaude terminal dashboard — keys\n\n")
	b.WriteString("  List\n")
	if m.listsLocalSessions() {
		b.WriteString("    Two kinds of row. Agents first; below them this host's plain tmux\n")
		b.WriteString("    sessions — the ones s starts here and any `tclaude session new` —\n")
		b.WriteString("    marked \"" + tuiSessionGroupCell + "\" in the GROUP column they have nothing to put in.\n")
		b.WriteString("    Live ones only: `tclaude session ls -a` has the rest.\n")
	}
	b.WriteString("    ↑/k, ↓/j   Move the cursor\n")
	b.WriteString("    enter      On an offline agent: start it again, in the directory and\n")
	b.WriteString("               conversation it was last running.\n")
	if m.capabilities.attachAgent {
		if m.capabilities.attachLocalPane {
			b.WriteString("               On a live one: go to its tmux session — switch-client when\n")
			b.WriteString("               agentd runs inside tmux, otherwise attach until you detach\n")
			b.WriteString("               with ctrl-b d.\n")
		} else {
			b.WriteString("               On a live one: stream its daemon-host tmux session through\n")
			b.WriteString("               the dashboard's authenticated terminal WebSocket. Press\n")
			b.WriteString("               ctrl-] d to close only that stream and return; press\n")
			b.WriteString("               ctrl-] ctrl-] to send a literal ctrl-] remotely.\n")
		}
		b.WriteString("               Going to a pane is operator consoles only;\n")
		b.WriteString("               starting one is whatever the daemon grants this console.\n")
	}
	if m.listsLocalSessions() {
		b.WriteString("               On a session: go to its pane the same way (it is always\n")
		b.WriteString("               live, so enter never means anything else there).\n")
	}
	b.WriteString("    n          Start a new agent\n")
	if m.capabilities.startLocalShell {
		b.WriteString("    s          Start a plain interactive shell session and attach to it.\n")
		b.WriteString("               Operator consoles only.\n")
	}
	b.WriteString("    delete     Move the selected agent one step toward removal (asks first):\n")
	b.WriteString("               a live agent is taken offline; an already-offline agent is\n")
	b.WriteString("               retired, leaving its groups and losing its grants. Retiring\n")
	b.WriteString("               keeps the conversation and any worktree.\n")
	if m.listsLocalSessions() {
		b.WriteString("               A session has no such ladder, so delete ends it: that is\n")
		b.WriteString("               `tmux kill-session` — whatever runs in the pane stops, no\n")
		b.WriteString("               graceful exit, no starting it again. It asks first too.\n")
	}
	b.WriteString("    f          Filter the list: only show active agents (toggle)\n")
	if m.listsLocalSessions() {
		b.WriteString("               Sessions are unaffected: every listed one is already live.\n")
	}
	b.WriteString("    r          Refresh now (the list also polls every 2s)\n")
	b.WriteString("    ?/h        This help\n")
	if m.capabilities.shutdownOnQuit {
		b.WriteString("    q/esc      Quit — this SHUTS DOWN the daemon (asks first)\n\n")
	} else {
		b.WriteString("    q/esc      Quit this console; agentd keeps running (asks first)\n\n")
	}
	b.WriteString("  New agent\n")
	b.WriteString("    tab/↑/↓    Next / previous field\n")
	if m.capabilities.completeLocalDir {
		b.WriteString("    tab        On a Directory you have typed into, complete the path\n")
		b.WriteString("               instead: one match completes it, several list below\n")
		b.WriteString("               the field. Operator consoles only. On the field as the\n")
		b.WriteString("               form left it, tab still moves to the next field.\n")
	}
	b.WriteString("    ←/→        Change the group, spawn profile, worktree or harness\n")
	b.WriteString("               A profile of \"(default)\" names none, which leaves the\n")
	b.WriteString("               group's and the global default profile in force.\n")
	b.WriteString("               Directory starts on the group's own default directory\n")
	b.WriteString("               (add a subdirectory to start below it) and follows the\n")
	b.WriteString("               group picker until you type a path of your own.\n")
	b.WriteString("               Worktree \"create new worktree\" cuts a git worktree in the\n")
	b.WriteString("               repo that directory is in and starts the agent inside it.\n")
	b.WriteString("               Its Branch follows the Name until you type one of your\n")
	b.WriteString("               own; naming a branch that already has a worktree reuses\n")
	b.WriteString("               that worktree instead of making another. Operator\n")
	b.WriteString("               consoles only — the worktree is created on the daemon's\n")
	b.WriteString("               host, and a failed spawn leaves it for you to keep or\n")
	b.WriteString("               remove.\n")
	if m.capabilities.attachAgent {
		b.WriteString("    enter      Spawn, then go straight to the new agent's pane —\n")
		b.WriteString("               the same move enter makes on its row. Operator\n")
		b.WriteString("               consoles only; an agent still starting up (no pane\n")
		b.WriteString("               yet) just lands in the list.\n")
	} else {
		b.WriteString("    enter      Spawn; the new agent lands in the remote listing.\n")
	}
	b.WriteString("    esc        Cancel\n\n")
	if m.capabilities.startLocalShell {
		b.WriteString("  New shell session\n")
		b.WriteString("    A session, not an agent: a plain interactive shell in its own tmux\n")
		b.WriteString("    session, with no conversation, no group and no permissions — so it\n")
		b.WriteString("    joins the list above as a \"" + tuiSessionGroupCell + "\" row rather than beside the\n")
		b.WriteString("    agents. Reach it from outside this console with\n")
		b.WriteString("    `tclaude session ls` and `tclaude session attach <handle>`.\n")
		b.WriteString("    tab/↑/↓    Next / previous field\n")
		if m.capabilities.completeLocalDir {
			b.WriteString("    tab        On a Directory you have typed into, complete the path\n")
			b.WriteString("               instead, exactly as in the new-agent form. Directory\n")
			b.WriteString("               starts on the directory this console was started in.\n")
		}
		b.WriteString("    Label names the tmux handle — letters, digits, '-' and '_', since\n")
		b.WriteString("               it is used verbatim; blank generates one.\n")
		b.WriteString("    enter      Start it, then go to its pane — the same handover enter\n")
		b.WriteString("               makes on an agent's row\n")
		b.WriteString("    esc        Cancel\n\n")
	}
	if m.remoteConsole() {
		b.WriteString("  Connected to " + m.connectionLabel + ". The console keeps polling\n")
		b.WriteString("  through outages and reconnects when agentd returns at this address.\n\n")
	} else if addr := m.dashboardAddressLine(); addr != "" {
		b.WriteString("  The web dashboard is running alongside this console:\n")
		b.WriteString("    " + addr + "\n")
		if m.dashboardLink != "" {
			b.WriteString("  That link signs you in as you open it, so there is no token to\n")
			b.WriteString("  paste. It is good for one use and about a minute, and the console\n")
			b.WriteString("  replaces it as it ages — so open the one on screen, and if the\n")
			b.WriteString("  browser says the link was already used, come back for the next.\n")
		}
		b.WriteString("  Both drive the same daemon, so human-approval requests still\n")
		b.WriteString("  appear in its Messages tab.\n\n")
	} else {
		b.WriteString("  This mode runs without the web dashboard: browser deep links and\n")
		b.WriteString("  in-dashboard human-approval requests are unavailable, so grant an\n")
		b.WriteString("  agent access with `tclaude agent permissions grant` instead.\n\n")
	}
	if len(m.tokenLines) > 0 {
		writeTokenBlock(&b, m.tokenLines, "  ")
		b.WriteString("\n")
	}
	return strings.Split(strings.TrimRight(b.String(), "\n"), "\n")
}

// helpLineRows is how many terminal rows a body line takes once the terminal
// has wrapped it. The help text is hand-wrapped to a comfortable width rather
// than the terminal's, so a narrow one wraps it again — and the viewport has
// to pay for every row a line took, or the footer is pushed off the bottom.
func (m tuiModel) helpLineRows(line string) int {
	w := lipgloss.Width(line)
	if m.width <= 0 || w <= m.width {
		return 1
	}
	return (w + m.width - 1) / m.width
}

// helpBodyRows is how many rows the help body has to fit in.
func (m tuiModel) helpBodyRows() int {
	return max(m.height-tuiHelpChrome, 1)
}

// helpWindow is the run of body lines visible at offset off: as many as the
// row budget holds. A line the budget cannot hold even on its own still shows,
// because leaving the viewport empty would strand every line after it — but
// cut to the rows there are, since overflowing the alt screen is the thing
// this viewport exists to stop.
func (m tuiModel) helpWindow(lines []string, off int) []string {
	off = min(max(off, 0), max(len(lines)-1, 0))
	budget := m.helpBodyRows()
	rows := 0
	out := make([]string, 0, len(lines)-off)
	for _, line := range lines[off:] {
		r := m.helpLineRows(line)
		if rows+r > budget {
			if len(out) > 0 {
				break
			}
			if m.width > 0 {
				line = tuiTruncate(line, budget*m.width)
			}
			return []string{line}
		}
		rows += r
		out = append(out, line)
	}
	return out
}

// helpMaxOffset is the furthest the help view scrolls: the offset that puts
// the last body line on the last body row. Zero means the whole text fits, and
// so does a console that has had no WindowSizeMsg yet — with no height to
// window to, renderHelpView shows everything rather than one line of it.
//
// It never goes past the last line, even when that line alone is taller than
// the budget: an offset with no line to name is one the range readout would
// count past the end of the text, and one that end/G would park a dead
// keystroke on.
func (m tuiModel) helpMaxOffset(lines []string) int {
	if m.height <= 0 || len(lines) == 0 {
		return 0
	}
	budget := m.helpBodyRows()
	rows := 0
	for i := len(lines) - 1; i >= 0; i-- {
		rows += m.helpLineRows(lines[i])
		if rows > budget {
			return min(i+1, len(lines)-1)
		}
	}
	return 0
}

// renderHelpView is the help text windowed to this terminal, which is what the
// console shows. Below a body that fits it says so and nothing more; below one
// that does not it names the scroll keys and where in the text the view is.
func (m tuiModel) renderHelpView() string {
	lines := m.helpBodyLines()
	maxOffset := m.helpMaxOffset(lines)
	if maxOffset <= 0 {
		return m.renderHelp()
	}
	off := min(max(m.helpOffset, 0), maxOffset)
	window := m.helpWindow(lines, off)
	body := strings.Join(window, "\n") + "\n"
	if m.height <= tuiHelpChrome {
		// No room for both a line of help and the footer under it. The help is
		// what the view is for, so the footer is what goes — and the keys it
		// would have named still work.
		return body
	}
	hint := tuiHelpScrollHint + fmt.Sprintf("  (%d–%d of %d)", off+1, off+len(window), len(lines))
	if m.width > 0 {
		hint = tuiTruncate(hint, m.width)
	}
	return body + "\n" + hint + "\n"
}
