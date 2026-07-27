package agentd

// Terminal UI for `tclaude agentd serve --tui` — a deliberately small
// in-terminal stand-in for the web dashboard's two most-used moves: see which
// agents exist, and start a new one. It is the operator surface for a daemon
// started with no dashboard listener at all (see runServe), so it is plain
// text with no color scheme, no theming and no per-terminal palette: the
// only visual state is an inverse-video cursor row.
//
// Everything it shows or does goes through the daemon's own /v1 HTTP API
// (tuiAPI), not through the DB or the spawn internals, so the TUI cannot
// drift from — or quietly skip — the validation, permission and audit paths
// every other spawn surface runs.

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
	"runtime/debug"
	"sort"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"github.com/tofutools/tclaude/pkg/claude/agent"
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
func startServeTUI(quit *quitter) func() error {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-quit.ch
		cancel()
	}()

	prog := tea.NewProgram(newTUIModel(newTUIAPI()), tea.WithContext(ctx))
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

// ---- in-process API client -------------------------------------------------

// tuiAPI issues requests against the daemon's own /v1 mux from inside the
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
type tuiAPI struct {
	handler http.Handler
	// pid and the resolved ancestry are fixed for the daemon's lifetime, so
	// the process-tree walk runs once here rather than on every 2s poll.
	pid                int
	convID             string
	hasHarnessAncestor bool
}

func newTUIAPI() *tuiAPI {
	// A second mux instance: buildMux only registers package-level handlers
	// and holds no per-mux state, so this shares every code path with the
	// socket server without sharing its identity middleware (which would
	// overwrite the peer stamped below).
	pid := os.Getpid()
	convID, hasAncestor := convIDForPID(pid)
	return &tuiAPI{
		handler:            buildMux(),
		pid:                pid,
		convID:             convID,
		hasHarnessAncestor: hasAncestor,
	}
}

// identityWarning explains, in one line, a console the daemon will not treat
// as the operator — empty when it will. Without it, a console started from
// the wrong place just answers every keystroke with a bare 403 and no hint
// about why or what to do instead.
func (a *tuiAPI) identityWarning() string {
	switch classify(&peer{
		PID:               a.pid,
		ConvID:            a.convID,
		HasClaudeAncestor: a.hasHarnessAncestor,
		// The console always presents the live token, so it is valid
		// whenever the daemon managed to mint one.
		HumanTokenValid: currentOperatorToken() != "",
	}) {
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

func (a *tuiAPI) get(path string, out any) error {
	return a.do(http.MethodGet, path, nil, out)
}

func (a *tuiAPI) post(path string, in, out any) error {
	return a.do(http.MethodPost, path, in, out)
}

func (a *tuiAPI) do(method, path string, in, out any) error {
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
// what the spawn form picks from and posts to.
type tuiGroupRow struct {
	Name string `json:"name"`
}

// ---- model -----------------------------------------------------------------

type tuiMode int

const (
	tuiModeList tuiMode = iota
	tuiModeSpawn
	tuiModeHelp
	tuiModeConfirmQuit
)

// Spawn-form fields, in tab order.
const (
	tuiFieldGroup = iota
	tuiFieldName
	tuiFieldDir
	tuiFieldHarness
	tuiFieldBrief
	tuiSpawnFieldCount
)

// tuiHarnessDefault is the spawn form's "don't pin a harness" choice: the
// request leaves Harness empty and the daemon's own profile chain decides.
const tuiHarnessDefault = "(default)"

type tuiModel struct {
	api *tuiAPI

	agents []tuiAgentRow
	groups []tuiGroupRow

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
	// identityWarning is fixed for the console's lifetime: it says the
	// daemon will not treat this console as the operator, and why.
	identityWarning string
	// refreshing / spawning keep the periodic tick from stacking requests
	// and the operator from firing two spawns at once.
	refreshing  bool
	spawning    bool
	lastRefresh time.Time

	form tuiSpawnForm
}

// tuiSpawnForm is the "new agent" prompt. Group and harness are cycled
// choices; the rest are text inputs.
type tuiSpawnForm struct {
	field int

	groupNames []string
	groupIdx   int

	harnessNames []string
	harnessIdx   int

	name  textinput.Model
	dir   textinput.Model
	brief textinput.Model
}

type (
	tuiTickMsg time.Time
	// tuiDataMsg carries one completed refresh — both lists, or the error
	// that stopped it.
	tuiDataMsg struct {
		agents []tuiAgentRow
		groups []tuiGroupRow
		err    error
	}
	// tuiSpawnedMsg carries the outcome of one spawn request.
	tuiSpawnedMsg struct {
		group string
		resp  agent.SpawnResponse
		err   error
	}
)

func newTUIModel(api *tuiAPI) tuiModel {
	m := tuiModel{api: api}
	if api != nil {
		m.identityWarning = api.identityWarning()
	}
	return m
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
	if m.identityWarning != "" {
		chrome += 2
	}
	if m.refreshErr != "" {
		chrome++
	}
	if m.notice != "" {
		chrome++
	}
	if m.mode == tuiModeConfirmQuit {
		chrome += 2
	}
	return max(m.height-chrome, 3)
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
		sortTUIAgents(agents)
		sort.Slice(groups, func(i, j int) bool { return groups[i].Name < groups[j].Name })
		return tuiDataMsg{agents: agents, groups: groups}
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

func tuiSpawnCmd(api *tuiAPI, group string, req agent.SpawnRequest) tea.Cmd {
	return func() tea.Msg {
		var resp agent.SpawnResponse
		err := api.post("/v1/groups/"+url.PathEscape(group)+"/spawn", req, &resp)
		return tuiSpawnedMsg{group: group, resp: resp, err: err}
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

func newTUITextInput(prompt string) textinput.Model {
	ti := textinput.New()
	ti.Prompt = prompt
	ti.SetWidth(60)
	return ti
}

// openSpawnForm builds a fresh "new agent" prompt over the current group
// list, defaulting the harness to the daemon's default harness.
func (m tuiModel) openSpawnForm() tuiModel {
	form := tuiSpawnForm{
		harnessNames: tuiHarnessOptions(),
		name:         newTUITextInput("  Name:      "),
		dir:          newTUITextInput("  Directory: "),
		brief:        newTUITextInput("  Brief:     "),
	}
	for _, g := range m.groups {
		form.groupNames = append(form.groupNames, g.Name)
	}
	for i, name := range form.harnessNames {
		if name == harness.DefaultName {
			form.harnessIdx = i
			break
		}
	}
	m.form = form
	m.mode = tuiModeSpawn
	return m
}

func (m tuiModel) selectedGroup() string {
	if m.form.groupIdx < 0 || m.form.groupIdx >= len(m.form.groupNames) {
		return ""
	}
	return m.form.groupNames[m.form.groupIdx]
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

// cycleChoice steps whichever of the two choice fields is focused.
func (m tuiModel) cycleChoice(delta int) tuiModel {
	switch m.form.field {
	case tuiFieldGroup:
		if n := len(m.form.groupNames); n > 0 {
			m.form.groupIdx = ((m.form.groupIdx+delta)%n + n) % n
		}
	case tuiFieldHarness:
		if n := len(m.form.harnessNames); n > 0 {
			m.form.harnessIdx = ((m.form.harnessIdx+delta)%n + n) % n
		}
	}
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
		Harness:        m.selectedHarness(),
		InitialMessage: strings.TrimSpace(m.form.brief.Value()),
	}
	m.mode = tuiModeList
	m.spawning = true
	m.notice = "Spawning in group " + group + "…"
	return m, tuiSpawnCmd(m.api, group, req)
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
		m.agents = msg.agents
		m.groups = msg.groups
		m.lastRefresh = time.Now()
		if m.cursor >= len(m.agents) {
			m.cursor = max(len(m.agents)-1, 0)
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
		// Pull the new agent in now rather than waiting out the tick.
		m.refreshing = true
		return m, m.refreshCmd()

	case tea.KeyPressMsg:
		return m.handleKey(msg)
	}
	return m, nil
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
		// Only "y" confirms. Enter is the reflexive dismiss key and the
		// prompt promises that anything other than "y" cancels — accepting
		// it here would turn a misread prompt into a daemon shutdown, taking
		// every managed agent's pane with it.
		switch msg.String() {
		case "y", "Y":
			return m, tea.Quit
		default:
			m.mode = tuiModeList
		}
		return m, nil

	case tuiModeSpawn:
		return m.handleSpawnKey(msg)
	}

	// List mode.
	switch msg.String() {
	case "q", "esc", "ctrl+c":
		// Quitting stops the daemon, so it always asks first.
		m.mode = tuiModeConfirmQuit
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
			m.ensureCursorVisible()
		}
	case "down", "j":
		if m.cursor < len(m.agents)-1 {
			m.cursor++
			m.ensureCursorVisible()
		}
	case "n", "N":
		m.notice = ""
		return m.openSpawnForm(), nil
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
	switch msg.String() {
	case "esc", "ctrl+c":
		m.mode = tuiModeList
		return m, nil
	case "enter":
		return m.submitSpawn()
	case "up", "shift+tab":
		return m.moveSpawnField(-1), nil
	case "down", "tab":
		return m.moveSpawnField(1), nil
	case "left":
		if m.form.field == tuiFieldGroup || m.form.field == tuiFieldHarness {
			return m.cycleChoice(-1), nil
		}
	case "right", " ":
		if m.form.field == tuiFieldGroup || m.form.field == tuiFieldHarness {
			return m.cycleChoice(1), nil
		}
	}
	updated, cmd := m.updateFocusedInput(msg)
	return updated, cmd
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

func (m tuiModel) renderList() string {
	var b strings.Builder
	b.WriteString("\n  tclaude agentd — terminal UI (no web dashboard in this mode)\n\n")
	if m.identityWarning != "" {
		b.WriteString("  " + m.identityWarning + "\n\n")
	}

	if len(m.agents) == 0 {
		b.WriteString("  No agents yet.\n")
	} else {
		tbl := table.New(m.columns()...)
		tbl.SetTerminalWidth(max(m.width-4, 60))
		tbl.SelectedIndex = m.cursor
		tbl.ViewportOffset = m.viewportOffset
		tbl.ViewportHeight = m.viewportHeight()
		for _, a := range m.agents {
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
		b.WriteString("  " + m.refreshErr + "\n")
	}
	if m.notice != "" {
		b.WriteString("  " + m.notice + "\n")
	}
	b.WriteString("\n  n new agent • r refresh • ↑/↓ move • ? help • q quit (shuts down agentd)\n")

	if m.mode == tuiModeConfirmQuit {
		b.WriteString("\n  Quit and shut down agentd? [y / any other key = cancel]\n")
	}
	return b.String()
}

func (m tuiModel) summaryLine() string {
	online := 0
	for _, a := range m.agents {
		if a.Online {
			online++
		}
	}
	shown := len(m.agents)
	line := fmt.Sprintf("%d agents (%d online) • %d groups", shown, online, len(m.groups))
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
	b.WriteString(tuiChoiceLine("  Group:     ", group, m.form.field == tuiFieldGroup))
	b.WriteString(m.form.name.View() + tuiHint(m.form.name.Value() == "", "  (blank = auto-generated)"))
	b.WriteString("\n")
	b.WriteString(m.form.dir.View() + tuiHint(m.form.dir.Value() == "", "  (blank = the group's default directory)"))
	b.WriteString("\n")
	harnessChoice := tuiHarnessDefault
	if m.form.harnessIdx >= 0 && m.form.harnessIdx < len(m.form.harnessNames) {
		harnessChoice = m.form.harnessNames[m.form.harnessIdx]
	}
	b.WriteString(tuiChoiceLine("  Harness:   ", harnessChoice, m.form.field == tuiFieldHarness))
	b.WriteString(m.form.brief.View() + tuiHint(m.form.brief.Value() == "", "  (blank = no startup brief)"))
	b.WriteString("\n")

	if m.notice != "" {
		b.WriteString("\n  " + m.notice + "\n")
	}
	b.WriteString("\n  enter spawn • ↑/↓/tab next field • ←/→ change group/harness • esc cancel\n")
	return b.String()
}

// tuiChoiceLine renders one of the two cycled fields. The focused one is
// marked with a caret rather than a color, since the console has no palette
// (the text fields mark focus with their own cursor).
func tuiChoiceLine(label, value string, focused bool) string {
	if focused {
		label = "> " + strings.TrimPrefix(label, "  ")
	}
	return label + "< " + value + " >\n"
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
	b.WriteString("\n  tclaude agentd terminal UI — keys\n\n")
	b.WriteString("  List\n")
	b.WriteString("    ↑/k, ↓/j   Move the cursor\n")
	b.WriteString("    n          Start a new agent\n")
	b.WriteString("    r          Refresh now (the list also polls every 2s)\n")
	b.WriteString("    ?/h        This help\n")
	b.WriteString("    q/esc      Quit — this SHUTS DOWN the daemon (asks first)\n\n")
	b.WriteString("  New agent\n")
	b.WriteString("    tab/↑/↓    Next / previous field\n")
	b.WriteString("    ←/→        Change the group or harness\n")
	b.WriteString("    enter      Spawn\n")
	b.WriteString("    esc        Cancel\n\n")
	b.WriteString("  This mode runs without the web dashboard: browser deep links and\n")
	b.WriteString("  in-dashboard human-approval requests are unavailable, so grant an\n")
	b.WriteString("  agent access with `tclaude agent permissions grant` instead.\n\n")
	b.WriteString("  Press any key to close.\n")
	return b.String()
}
