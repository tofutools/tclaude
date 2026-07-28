package agentd

// Terminal UI for `tclaude agentd serve --tui` — a deliberately small
// in-terminal stand-in for the web dashboard's most-used moves: see which
// agents exist, start a new one, go to one's tmux session, and retire one that
// is done. On its own it
// is the whole operator surface (runServe starts no dashboard listener); it
// also runs happily beside the web dashboard when the operator asks for both.
// Either way it is plain text with no color scheme, no theming and no
// per-terminal palette — the dashboard's cosmetic re-skins (--slop, --wizard)
// are the browser's business and never reach here. The only visual state is
// an inverse-video cursor row.
//
// Everything it shows or does goes through the daemon's own /v1 HTTP API
// (tuiAPI), not through the DB or the spawn internals, so the TUI cannot
// drift from — or quietly skip — the validation, permission and audit paths
// every other spawn surface runs. The one exception is attachSelected, which
// hands this very terminal to an agent's tmux session: there is no HTTP shape
// for a local terminal takeover, so it reads the session row directly and is
// gated on the console being the operator instead.

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
}

func startServeTUI(quit *quitter, startup tuiStartup) func() error {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-quit.ch
		cancel()
	}()

	m := newTUIModel(newInProcessTUIAPI())
	m.dashboardURL = startup.dashboardURL
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
// resume) need no capability bit: both implementations support them.
type tuiCapabilities struct {
	attachLocalPane  bool
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
		handler:            buildMux(),
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
		attachLocalPane:  true,
		completeLocalDir: true,
		shutdownOnQuit:   true,
	}
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
type tuiProfileRow struct {
	Name     string `json:"name"`
	Disabled *bool  `json:"disabled,omitempty"`
}

func (p tuiProfileRow) disabled() bool { return p.Disabled != nil && *p.Disabled }

// tuiRetireResult is the subset of the retire response the console reports:
// what the demotion actually revoked, and what became of the agent's session.
type tuiRetireResult struct {
	Outcome  retireConvOutcome `json:"outcome"`
	Shutdown memberOpResult    `json:"shutdown"`
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
	tuiModeHelp
	tuiModeConfirmQuit
	tuiModeConfirmRetire
)

// Spawn-form fields, in tab order. The profile sits directly under the group
// because it is the field the others are read against: it decides what a
// blank harness (or model, or approval posture) ends up being.
const (
	tuiFieldGroup = iota
	tuiFieldProfile
	tuiFieldName
	tuiFieldDir
	tuiFieldHarness
	tuiFieldBrief
	tuiSpawnFieldCount
)

// tuiHarnessDefault is the spawn form's "don't pin a harness" choice: the
// request leaves Harness empty and the daemon's own profile chain decides.
const tuiHarnessDefault = "(default)"

// tuiProfileDefault is the same idea for the profile picker: naming no
// profile is NOT "no profile at all", it hands the choice back to the
// daemon's chain — the group's default profile, then the global one.
const tuiProfileDefault = "(default)"

type tuiModel struct {
	api tuiAPI

	agents   []tuiAgentRow
	groups   []tuiGroupRow
	profiles []tuiProfileRow

	cursor         int
	viewportOffset int
	width          int
	height         int

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
	// tokenLines is the operator-token block this console shows in place of
	// the stdout banner, empty when stdout printed it. showTokenBanner is the
	// startup presentation of that block; it goes away on the first keystroke,
	// and the help view keeps it reachable afterwards.
	tokenLines      []string
	showTokenBanner bool
	// refreshing / spawning / retiring / resuming keep the periodic tick from
	// stacking requests and the operator from firing two of the same action at
	// once.
	refreshing  bool
	spawning    bool
	retiring    bool
	resuming    bool
	lastRefresh time.Time
	// retireTarget is the agent the retire confirmation is about, captured
	// when the prompt opens rather than re-read when it is answered: the
	// listing re-sorts under the cursor every two seconds, so resolving the
	// target on "y" could retire an agent the operator never saw named.
	retireTarget tuiAgentRow

	filterActive bool

	form tuiSpawnForm
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

	name  textinput.Model
	dir   textinput.Model
	brief textinput.Model
	// dirPrefill is the value prefillDir last wrote into dir — the selected
	// group's default directory. It is what tells an untouched field from one
	// the operator has typed in, so changing the group can follow the first
	// and must leave the second alone.
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
		agents   []tuiAgentRow
		groups   []tuiGroupRow
		profiles []tuiProfileRow
		err      error
		// profilesErr is the profile listing's own failure, kept apart from
		// err: it costs the spawn form one picker, not the whole console.
		profilesErr error
	}
	// tuiSpawnedMsg carries the outcome of one spawn request.
	tuiSpawnedMsg struct {
		group string
		resp  agent.SpawnResponse
		err   error
	}
	// tuiAttachedMsg carries the outcome of putting the operator on an
	// agent's pane — after they detach, in the attach case.
	tuiAttachedMsg struct {
		agent   string
		session string
		err     error
	}
	// tuiRetiredMsg carries the outcome of one retire request.
	tuiRetiredMsg struct {
		agent string
		res   tuiRetireResult
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
		api:        api,
		refreshing: true, // Init immediately starts the first refresh.
		capabilities: tuiCapabilities{
			attachLocalPane:  true,
			completeLocalDir: true,
			shutdownOnQuit:   true,
		},
		connectionLabel: "in-process",
	}
	if api != nil {
		m.identityWarning = api.identityWarning()
		m.operator = api.isOperator()
		m.capabilities = api.capabilities()
		m.connectionLabel = api.connectionLabel()
	}
	return m
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
	return func() tea.Msg {
		var agents []tuiAgentRow
		if err := api.get("/v1/peers", &agents); err != nil {
			return tuiDataMsg{err: err}
		}
		var groups []tuiGroupRow
		if err := api.get("/v1/groups", &groups); err != nil {
			return tuiDataMsg{err: err}
		}
		// The profile list feeds one field of a form that is usually closed,
		// so its failure travels beside the listing rather than instead of it:
		// a console whose agents are all live and visible must not go blank
		// because a profile read hit the DB at a bad moment.
		var profiles []tuiProfileRow
		profilesErr := api.get("/v1/spawn-profiles", &profiles)
		sortTUIAgents(agents)
		sort.Slice(groups, func(i, j int) bool { return groups[i].Name < groups[j].Name })
		sort.Slice(profiles, func(i, j int) bool { return profiles[i].Name < profiles[j].Name })
		return tuiDataMsg{agents: agents, groups: groups, profiles: profiles, profilesErr: profilesErr}
	}
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

func tuiSpawnCmd(api tuiAPI, group string, req agent.SpawnRequest) tea.Cmd {
	return func() tea.Msg {
		var resp agent.SpawnResponse
		err := api.post("/v1/groups/"+url.PathEscape(group)+"/spawn", req, &resp)
		return tuiSpawnedMsg{group: group, resp: resp, err: err}
	}
}

// tuiRetireCmd retires one agent through the daemon's own verb. It sends no
// query parameters, which is the documented default pair for that endpoint:
// the pane is asked to exit (?shutdown), and the worktree is left alone
// (?delete_worktree) — the console never deletes an operator's work, and has
// no probe of the kind the dashboard runs before offering to.
func tuiRetireCmd(api tuiAPI, convID, name string) tea.Cmd {
	return func() tea.Msg {
		var res tuiRetireResult
		err := api.post("/v1/agent/"+url.PathEscape(convID)+"/retire", nil, &res)
		return tuiRetiredMsg{agent: name, res: res, err: err}
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
	form := tuiSpawnForm{
		harnessNames: tuiHarnessOptions(),
		profileNames: tuiProfileOptions(m.profiles),
		name:         newTUITextInput("  Name:      "),
		dir:          newTUITextInput("  Directory: "),
		brief:        newTUITextInput("  Brief:     "),
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

func (m tuiModel) selectedHarness() string {
	if m.form.harnessIdx < 0 || m.form.harnessIdx >= len(m.form.harnessNames) {
		return ""
	}
	if name := m.form.harnessNames[m.form.harnessIdx]; name != tuiHarnessDefault {
		return name
	}
	return ""
}

// moveSpawnField shifts focus through the form, wrapping, and moves the
// text-input focus with it — the group and harness fields have no input
// model behind them, so all three blur for those.
func (m tuiModel) moveSpawnField(delta int) tuiModel {
	m.form.field = ((m.form.field+delta)%tuiSpawnFieldCount + tuiSpawnFieldCount) % tuiSpawnFieldCount
	m.form.name.Blur()
	m.form.dir.Blur()
	m.form.brief.Blur()
	switch m.form.field {
	case tuiFieldName:
		m.form.name.Focus()
	case tuiFieldDir:
		m.form.dir.Focus()
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
		}
	case tuiFieldHarness:
		if n := len(m.form.harnessNames); n > 0 {
			m.form.harnessIdx = ((m.form.harnessIdx+delta)%n + n) % n
		}
	}
	return m
}

// tuiIsChoiceField reports whether field is one of the cycled pickers, which
// is what makes ←/→ (and space) change a value rather than reach a text input.
func tuiIsChoiceField(field int) bool {
	switch field {
	case tuiFieldGroup, tuiFieldProfile, tuiFieldHarness:
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
	case tuiFieldDir:
		m.form.dir, cmd = m.form.dir.Update(msg)
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
		Name:           strings.TrimSpace(m.form.name.Value()),
		Cwd:            strings.TrimSpace(m.form.dir.Value()),
		Profile:        m.selectedProfile(),
		Harness:        m.selectedHarness(),
		InitialMessage: strings.TrimSpace(m.form.brief.Value()),
	}
	m.mode = tuiModeList
	m.spawning = true
	m.notice = "Spawning in group " + group + "…"
	return m, tuiSpawnCmd(m.api, group, req)
}

// tuiAttachToPane hands this terminal to the tmux session named by target,
// indirected through a package var so tests can observe the target without a
// tmux server. inTmux reports whether agentd itself is running inside tmux.
var tuiAttachToPane = realTUIAttachToPane

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
		return tuiAttachedMsg{agent: agentName, session: tmuxSession, err: err}
	})
}

// insideTmux reports whether this process is itself a tmux client.
func insideTmux() bool { return os.Getenv("TMUX") != "" }

// selectedAgent is the row the cursor is on, and whether there is one — the
// listing can be empty, and it re-sorts under the cursor every two seconds.
func (m tuiModel) selectedAgent() (tuiAgentRow, bool) {
	visible := m.visibleAgents()
	if m.cursor < 0 || m.cursor >= len(visible) {
		return tuiAgentRow{}, false
	}
	return visible[m.cursor], true
}

// enterSelected is what enter does on a row, which depends on whether the
// agent is up: an offline one is turned back on, a live one gets this
// terminal handed to its pane. Both are the move the operator wants next
// after picking that row, and an offline agent has no pane to attach to
// anyway — enter on it used to be a dead key that only said so.
func (m tuiModel) enterSelected() (tuiModel, tea.Cmd) {
	row, ok := m.selectedAgent()
	if !ok {
		return m, nil
	}
	if !row.Online {
		return m.resumeSelected(row)
	}
	return m.attachSelected()
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
// The target is resolved from the daemon's own session rows rather than over
// the API: which tmux session an agent is on is local process state, and the
// action itself is a takeover of THIS terminal, which no HTTP shape can
// express. Both halves are therefore gated on the console being the operator,
// so a console that the daemon classifies as an agent cannot use it to reach a
// peer's pane and drive it.
func (m tuiModel) attachSelected() (tuiModel, tea.Cmd) {
	row, ok := m.selectedAgent()
	if !ok {
		return m, nil
	}
	if !m.operator {
		m.notice = "Only an operator console can attach to an agent's terminal."
		return m, nil
	}
	if !m.capabilities.attachLocalPane {
		m.notice = "This console is remote; attach to the agent from the daemon host."
		return m, nil
	}
	sess := pickAliveSession(row.ConvID)
	if sess == nil || sess.TmuxSession == "" {
		m.notice = row.name() + " has no live tmux session to attach to."
		return m, nil
	}
	if insideTmux() {
		m.notice = "Switching to " + sess.TmuxSession + "…"
	} else {
		m.notice = "Attached to " + sess.TmuxSession + " — detach (ctrl-b d) to come back."
	}
	return m, tuiAttachToPane(row.name(), sess.TmuxSession, insideTmux())
}

// confirmRetireSelected opens the retire confirmation over the selected row.
// Retiring is not undoable from here (reinstating is a CLI/dashboard move) and
// it stops the agent's pane, so it always asks first — the same rule quitting
// follows.
func (m tuiModel) confirmRetireSelected() tuiModel {
	row, ok := m.selectedAgent()
	if !ok {
		return m
	}
	if m.retiring {
		m.notice = "A retire is already in flight."
		return m
	}
	m.retireTarget = row
	m.mode = tuiModeConfirmRetire
	return m
}

// retireConfirmed fires the retire the operator just confirmed. Permission is
// the daemon's call, not the console's: the endpoint gates every caller class
// itself, so an agent-class console gets the same refusal here it would get
// over the socket rather than a second, divergent rule.
func (m tuiModel) retireConfirmed() (tuiModel, tea.Cmd) {
	row := m.retireTarget
	m.retireTarget = tuiAgentRow{}
	m.mode = tuiModeList
	if row.ConvID == "" {
		return m, nil
	}
	m.retiring = true
	m.notice = "Retiring " + row.name() + "…"
	return m, tuiRetireCmd(m.api, row.ConvID, row.name())
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
		return m, nil

	case tuiTickMsg:
		if m.refreshing {
			return m, tuiTickCmd()
		}
		m.refreshing = true
		return m, tea.Batch(m.refreshCmd(), tuiTickCmd())

	case tuiDataMsg:
		m.refreshing = false
		if msg.err != nil {
			m.refreshErr = "Refresh failed: " + msg.err.Error()
			return m, nil
		}
		m.refreshErr = ""
		// Which agent the cursor is on is decided before the listing under it
		// is replaced — see restoreCursor.
		selected, hadSelection := m.selectedAgent()
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
		m.lastRefresh = time.Now()
		if hadSelection {
			m.restoreCursor(selected.ConvID)
		}
		visible := m.visibleAgents()
		if m.cursor >= len(visible) {
			m.cursor = max(len(visible)-1, 0)
		}
		m.ensureCursorVisible()
		return m, nil

	case tuiSpawnedMsg:
		m.spawning = false
		if msg.err != nil {
			m.notice = "Spawn failed: " + msg.err.Error()
			return m, nil
		}
		m.notice = tuiSpawnSummary(msg)
		if focused, cmd := m.focusSpawned(msg); cmd != nil {
			// Going to the pane ends in a tuiAttachedMsg, which refreshes.
			return focused, cmd
		}
		// Pull the new agent in now rather than waiting out the tick.
		m.refreshing = true
		return m, m.refreshCmd()

	case tuiRetiredMsg:
		m.retiring = false
		if msg.err != nil {
			m.notice = "Retire failed: " + msg.err.Error()
			return m, nil
		}
		m.notice = tuiRetireSummary(msg)
		// The row is gone (or offline) now — show that rather than leaving a
		// retired agent listed until the next tick.
		m.refreshing = true
		return m, m.refreshCmd()

	case tuiResumedMsg:
		m.resuming = false
		if msg.err != nil {
			m.notice = "Start failed: " + msg.err.Error()
			return m, nil
		}
		m.notice = tuiResumeSummary(msg)
		// The row is online now — show that rather than leaving it reading
		// "offline" until the next tick.
		m.refreshing = true
		return m, m.refreshCmd()

	case tuiAttachedMsg:
		if msg.err != nil {
			m.notice = "Could not reach " + msg.session + ": " + msg.err.Error()
			return m, nil
		}
		if insideTmux() {
			m.notice = "Switched to " + msg.session + " (" + msg.agent + ")."
		} else {
			m.notice = "Back from " + msg.session + " (" + msg.agent + ")."
		}
		// The pane may have ended while the operator was on it.
		m.refreshing = true
		return m, m.refreshCmd()

	case tea.KeyPressMsg:
		// The startup token block is a banner, not a mode: the first keystroke
		// retires it whatever else that keystroke does, and the help view
		// keeps the token reachable afterwards.
		m.showTokenBanner = false
		return m.handleKey(msg)
	}
	return m, nil
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
	if !m.operator || !m.capabilities.attachLocalPane || session == "" {
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
	if insideTmux() {
		m.notice += " — switching to " + session + "…"
	} else {
		m.notice += " — attaching; detach (ctrl-b d) to come back."
	}
	return m, tuiAttachToPane(name, session, insideTmux())
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

func (m tuiModel) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch m.mode {
	case tuiModeHelp:
		m.mode = tuiModeList
		return m, nil

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
			m.retireTarget = tuiAgentRow{}
		}
		return m, nil

	case tuiModeSpawn:
		return m.handleSpawnKey(msg)
	}

	// List mode.
	visible := m.visibleAgents()
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
		var selectedID string
		if m.cursor >= 0 && m.cursor < len(visible) {
			selectedID = visible[m.cursor].ConvID
		}
		m.filterActive = !m.filterActive
		visible = m.visibleAgents()
		m.cursor = 0
		if selectedID != "" {
			for i, a := range visible {
				if a.ConvID == selectedID {
					m.cursor = i
					break
				}
			}
		}
		m.ensureCursorVisible()
		return m, nil
	case "enter":
		m.notice = ""
		return m.enterSelected()
	case "n", "N":
		m.notice = ""
		return m.openSpawnForm(), nil
	case "x", "X":
		m.notice = ""
		return m.confirmRetireSelected(), nil
	case "r":
		if !m.refreshing {
			m.refreshing = true
			m.notice = ""
			return m, m.refreshCmd()
		}
	case "?", "h":
		m.mode = tuiModeHelp
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

// restoreCursor puts the cursor back on the agent it was on, by conv-id,
// after a refresh has replaced the listing under it. An agent that is no
// longer listed (retired, or filtered out) leaves the cursor where it was,
// for the clamp above to bring back into range.
//
// The listing re-sorts on every poll — online agents first, then by name — so
// an index alone does not name an agent for longer than two seconds. That is
// the same hazard the retire prompt captures its target for, and it bites
// hardest right after enter starts an offline agent: that agent jumps from
// the bottom of the listing to the top, and a cursor left at the old index
// would put the next keystroke on whichever agent slid into its place.
func (m *tuiModel) restoreCursor(convID string) {
	if convID == "" {
		return
	}
	for i, a := range m.visibleAgents() {
		if a.ConvID == convID {
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
		return tea.View{Content: m.renderHelp(), AltScreen: true}
	case tuiModeSpawn:
		return tea.View{Content: m.renderSpawnForm(), AltScreen: true}
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
	if m.connectionLabel != "" && m.connectionLabel != "in-process" {
		b.WriteString(" • connected to " + m.connectionLabel)
	} else if m.dashboardURL != "" {
		b.WriteString(" • web dashboard: " + m.dashboardURL)
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

	visible := m.visibleAgents()
	if len(visible) == 0 {
		if m.filterActive && len(m.agents) > 0 {
			b.WriteString("  No active agents.\n")
		} else {
			b.WriteString("  No agents yet.\n")
		}
	} else {
		tbl := table.New(m.columns()...)
		tbl.SetTerminalWidth(max(m.width-4, 60))
		tbl.SelectedIndex = m.cursor
		tbl.ViewportOffset = m.viewportOffset
		tbl.ViewportHeight = m.viewportHeight()
		for _, a := range visible {
			tbl.AddRow(table.Row{Cells: []string{
				a.name(),
				strings.Join(a.Groups, ","),
				a.status(),
				a.State.Harness,
				a.dir(),
				a.Branch,
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
	b.WriteString("\n  " + m.keyHintLine() + "\n")

	if prompt := m.confirmPrompt(); prompt != "" {
		b.WriteString("\n  " + prompt + "\n")
	}
	return b.String()
}

// confirmPrompt is the question the list view is waiting on, empty when it is
// waiting on none. Both prompts are one line by contract — viewportHeight pays
// for exactly two (the blank line and the question), so a second line would
// overflow the terminal.
func (m tuiModel) confirmPrompt() string {
	switch m.mode {
	case tuiModeConfirmQuit:
		if !m.capabilities.shutdownOnQuit {
			return "Quit this remote console? [y / any other key = cancel]"
		}
		return "Quit and shut down agentd? [y / any other key = cancel]"
	case tuiModeConfirmRetire:
		const prefix = "Retire "
		const suffix = " and stop its session? [y / any other key = cancel]"
		return prefix +
			tuiTruncate(m.retireTarget.name(), m.promptNameBudget(len(prefix)+len(suffix))) +
			suffix
	default:
		return ""
	}
}

// promptNameBudget is how many columns the retire prompt may spend on the
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
	row, ok := m.selectedAgent()
	switch {
	case !ok:
		return ""
	case !row.Online:
		return "enter start"
	case !m.operator || !m.capabilities.attachLocalPane:
		return ""
	case insideTmux():
		return "enter switch to"
	default:
		return "enter attach"
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
	hints := "n new agent • " + filterLabel + " • r refresh • ↑/↓ move • ? help • " + quitHint
	if len(m.visibleAgents()) > 0 {
		// Unlike attaching, retire is not statically an operator-only move: an
		// agent-class console can hold agent.retire over its own group's
		// members, so the daemon decides and the key stays advertised.
		hints = "x retire • " + hints
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
	visible := m.visibleAgents()
	shown := len(visible)
	var line string
	if m.filterActive {
		line = fmt.Sprintf("%d active agents • %d groups", shown, len(m.groups))
	} else {
		line = fmt.Sprintf("%d agents (%d online) • %d groups", shown, online, len(m.groups))
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
	b.WriteString(m.dirSuggestionLine() + "\n")
	harnessChoice := tuiHarnessDefault
	if m.form.harnessIdx >= 0 && m.form.harnessIdx < len(m.form.harnessNames) {
		harnessChoice = m.form.harnessNames[m.form.harnessIdx]
	}
	b.WriteString(tuiChoiceLine("  Harness:   ", harnessChoice,
		tuiHint(harnessChoice == tuiHarnessDefault,
			"  (the profile chain decides — "+harness.DefaultName+" if nothing pins one)"),
		m.form.field == tuiFieldHarness))
	b.WriteString(m.form.brief.View() + tuiHint(m.form.brief.Value() == "", "  (blank = no startup brief)"))
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
	if m.operator && m.capabilities.attachLocalPane {
		spawnHint = "enter spawn + go to its pane"
	}
	b.WriteString("\n  " + spawnHint + " • ↑/↓/tab next field • " + completeHint +
		"←/→ change group/profile/harness • esc cancel\n")
	return b.String()
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
func (m tuiModel) dirSuggestionLine() string {
	if len(m.form.dirSuggestions) == 0 {
		return ""
	}
	const indent = "  "
	if m.width <= len(indent) {
		// No width yet (no WindowSizeMsg): show them all rather than nothing.
		return indent + strings.Join(m.form.dirSuggestions, "  ")
	}
	line := indent
	for i, name := range m.form.dirSuggestions {
		next := name
		if i > 0 {
			next = "  " + name
		}
		more := fmt.Sprintf("  (+%d more)", len(m.form.dirSuggestions)-i)
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
func tuiHint(show bool, hint string) string {
	if !show {
		return ""
	}
	return hint
}

func (m tuiModel) renderHelp() string {
	var b strings.Builder
	b.WriteString("\n  tclaude terminal dashboard — keys\n\n")
	b.WriteString("  List\n")
	b.WriteString("    ↑/k, ↓/j   Move the cursor\n")
	b.WriteString("    enter      On an offline agent: start it again, in the directory and\n")
	b.WriteString("               conversation it was last running.\n")
	if m.capabilities.attachLocalPane {
		b.WriteString("               On a live one: go to its tmux session — switch-client when\n")
		b.WriteString("               agentd runs inside tmux, otherwise attach until you detach\n")
		b.WriteString("               with ctrl-b d. Going to a pane is operator consoles only;\n")
		b.WriteString("               starting one is whatever the daemon grants this console.\n")
	} else {
		b.WriteString("               A remote console cannot attach to a live daemon-host pane.\n")
	}
	b.WriteString("    n          Start a new agent\n")
	b.WriteString("    x          Retire the selected agent (asks first): it leaves its\n")
	b.WriteString("               groups, loses its grants and its session is asked to\n")
	b.WriteString("               exit. The conversation and any worktree are kept.\n")
	b.WriteString("    f          Filter the list: only show active agents (toggle)\n")
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
	b.WriteString("    ←/→        Change the group, spawn profile or harness\n")
	b.WriteString("               A profile of \"(default)\" names none, which leaves the\n")
	b.WriteString("               group's and the global default profile in force.\n")
	b.WriteString("               Directory starts on the group's own default directory\n")
	b.WriteString("               (add a subdirectory to start below it) and follows the\n")
	b.WriteString("               group picker until you type a path of your own.\n")
	if m.capabilities.attachLocalPane {
		b.WriteString("    enter      Spawn, then go straight to the new agent's pane —\n")
		b.WriteString("               the same move enter makes on its row. Operator\n")
		b.WriteString("               consoles only; an agent still starting up (no pane\n")
		b.WriteString("               yet) just lands in the list.\n")
	} else {
		b.WriteString("    enter      Spawn; the new agent lands in the remote listing.\n")
	}
	b.WriteString("    esc        Cancel\n\n")
	if m.connectionLabel != "" && m.connectionLabel != "in-process" {
		b.WriteString("  Connected to " + m.connectionLabel + ". The console keeps polling\n")
		b.WriteString("  through outages and reconnects when agentd returns at this address.\n\n")
	} else if m.dashboardURL != "" {
		b.WriteString("  The web dashboard is running alongside this console:\n")
		b.WriteString("    " + m.dashboardURL + "\n")
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
	b.WriteString("  Press any key to close.\n")
	return b.String()
}
