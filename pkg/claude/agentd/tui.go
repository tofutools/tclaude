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
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/tofutools/tclaude/pkg/claude/agent"
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
	// the token-output policy suppressed it entirely.
	operatorToken string
	tokenSource   tokenSource
	// suppressSecrets follows the token-output policy: the operator has said this
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

// ---- API seam (the in-process client is in tui_inprocess_api.go) ----------

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
// in-process implementation (inProcessTUIAPI) dispatches directly into the daemon mux;
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
	tuiFieldModel
	tuiFieldEffort
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
	// suppressSecrets follows the token-output policy: this terminal's output is
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
		agent       string
		session     string
		remote      bool
		spawnNotice string
		err         error
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

func (m tuiModel) Init() tea.Cmd {
	return tea.Batch(m.refreshCmd(), tuiTickCmd())
}

func (m tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		return m.onWindowSize(msg)
	case tuiTickMsg:
		return m.onTick(msg)
	case tuiUsageMsg:
		return m.onUsage(msg)
	case tuiDataMsg:
		return m.onData(msg)
	case tuiWorktreeResolvedMsg:
		return m.onWorktreeResolved(msg)
	case tuiSpawnedMsg:
		return m.onSpawned(msg)
	case tuiRetiredMsg:
		return m.onRetired(msg)
	case tuiStoppedMsg:
		return m.onStopped(msg)
	case tuiResumedMsg:
		return m.onResumed(msg)
	case tuiShellStartedMsg:
		return m.onShellStarted(msg)
	case tuiSessionKilledMsg:
		return m.onSessionKilled(msg)
	case tuiAttachedMsg:
		return m.onAttached(msg)
	case tea.KeyPressMsg:
		// The startup token block is a banner, not a mode: the first keystroke
		// retires it whatever else that keystroke does, and the help view
		// keeps the token reachable afterwards.
		m.showTokenBanner = false
		return m.handleKey(msg)
	case tea.PasteMsg:
		return m.deliverPaste(msg)
	}
	return m, nil
}

// deliverPaste hands pasted text to whichever form field has focus.
//
// A bracketed paste arrives as one message of its own rather than as the key
// presses that would have typed it, so it never reaches handleKey and the
// fields it is aimed at never see it. The text input already knows what to do
// with the message — it collapses the newlines and tabs a paste can carry to
// spaces, since these fields are single-line — it just has to be handed it.
//
// Outside the two forms there is no field to paste into, and pasting is not a
// way to press the list's keys: the content is dropped.
func (m tuiModel) deliverPaste(msg tea.PasteMsg) (tea.Model, tea.Cmd) {
	switch m.mode {
	case tuiModeSpawn:
		// Pasted text edits a field exactly as typing does, so the Tab
		// candidate list goes with it — same as every non-completing key in
		// handleSpawnKey, and for the same reason: it is an answer to a path
		// that may no longer be in the field.
		m.form.dirSuggestions = nil
		updated, cmd := m.updateFocusedInput(msg)
		return updated, cmd
	case tuiModeShell:
		m.shell.dirSuggestions = nil
		updated, cmd := m.updateFocusedShellInput(msg)
		return updated, cmd
	}
	return m, nil
}

// ---- view (the rendering helpers are in tui_view.go) ----------------------

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
