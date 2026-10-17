package agentd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/creack/pty"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/federation/terminal"
)

type fedPanePin struct {
	agent, conv, session, tmux, pane, window, serverSession, incarnation string
	cwd                                                                  string
	group                                                                int64
	created                                                              time.Time
}

func fedTerminalSlug(readOnly bool) string {
	if readOnly {
		return PermSessionsWatch
	}
	return PermSessionsAttach
}
func fedTerminalCap(readOnly bool) string {
	if readOnly {
		return proto.CapSessionsWatch
	}
	return proto.CapSessionsAttach
}

func resolveFedPane(peer string, p proto.SessionOpenPayload) (*fedPanePin, error) {
	trusted, _ := db.GetFederationPeer(peer)
	if trusted == nil {
		return nil, errors.New("peer no longer trusted")
	}
	g, err := db.GetAgentGroupByName(p.Group)
	if err != nil || g == nil || g.IsArchived() || !fedPeerAllows(peer, g.ID, fedTerminalSlug(p.ReadOnly)) {
		return nil, errors.New("session access not granted")
	}
	a, err := db.GetAgent(p.Agent)
	if err != nil || a == nil || !a.Active() {
		return nil, errors.New("no active target agent")
	}
	members, err := db.ListAgentGroupMembers(g.ID)
	if err != nil {
		return nil, err
	}
	member := false
	for _, m := range members {
		if m.ConvID == a.CurrentConvID {
			member = true
		}
	}
	if !member {
		return nil, errors.New("target is not a member of the granted group")
	}
	rows, err := db.FindSessionsByConvID(a.CurrentConvID)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		if r.ID != p.Session || r.Status == session.StatusExited || r.TmuxSession == "" || !session.IsTmuxSessionAlive(r.TmuxSession) {
			continue
		}
		incarnation := fedSessionIncarnation(r)
		if p.Incarnation == "" || incarnation != p.Incarnation {
			return nil, errors.New("session incarnation changed; list sessions again")
		}
		pin, err := fedPaneIdentity(r.TmuxSession)
		if err != nil {
			return nil, err
		}
		pin.cwd = r.Cwd
		pin.agent, pin.conv, pin.session, pin.tmux, pin.group, pin.created, pin.incarnation = p.Agent, a.CurrentConvID, r.ID, r.TmuxSession, g.ID, r.CreatedAt, incarnation
		return pin, nil
	}
	return nil, errors.New("advertised session is no longer live; list sessions again")
}

func fedPaneIdentity(tmuxName string) (*fedPanePin, error) {
	out, err := clcommon.TmuxCommand("display-message", "-p", "-t", clcommon.ExactTarget(tmuxName)+":0.0", "#{pane_id}\t#{window_id}\t#{session_id}\t#{session_windows}\t#{window_panes}").Output()
	if err != nil {
		return nil, errors.New("target pane unavailable")
	}
	f := strings.Split(strings.TrimSpace(string(out)), "\t")
	if len(f) != 5 || !validTmuxObject(f[0], '%') || !validTmuxObject(f[1], '@') || !validTmuxObject(f[2], '$') {
		return nil, errors.New("cannot resolve exact target pane")
	}
	if f[3] != "1" || f[4] != "1" {
		return nil, errors.New("remote attach requires a session with exactly one window and one pane")
	}
	return &fedPanePin{pane: f[0], window: f[1], serverSession: f[2]}, nil
}
func validTmuxObject(s string, prefix byte) bool {
	if len(s) < 2 || s[0] != prefix {
		return false
	}
	for _, c := range s[1:] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
func (pin *fedPanePin) authorized(peer string, p proto.SessionOpenPayload) bool {
	current, err := resolveFedPane(peer, p)
	return err == nil && *current == *pin
}

// fedPaneTerminal only reads the renderer PTY. Its master is deliberately
// exposed as io.ReadCloser: no keyboard or terminal-reply path can write it.
// All input goes through the separate pane keyboard encoder.
type fedPaneTerminal struct {
	f        io.ReadCloser
	cmd      *exec.Cmd
	pin      *fedPanePin
	once     sync.Once
	inputMu  sync.Mutex
	keyboard terminal.Keyboard
	resize   func(int, int) error
	inputAt  time.Time
	// copyMode records that a remote wheel put the pane into copy mode, so
	// Close can leave it as it found it. Cleared once the pane is seen out of
	// copy mode: a later copy mode belongs to someone else.
	copyMode bool
	// closed stops input that races Close from touching the pane again.
	closed bool
}

func openFedPaneTerminal(pin *fedPanePin, cols, rows int) (*fedPaneTerminal, error) {
	version, err := clcommon.TmuxCommand("-V").Output()
	if err != nil {
		return nil, err
	}
	var major, minor int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(version)), "tmux %d.%d", &major, &minor); err != nil || major < 3 || major == 3 && minor < 2 {
		return nil, errors.New("remote attach requires tmux 3.2 or newer (ignore-size client support)")
	}
	// The renderer's output crosses the hub in arbitrary chunks, so ask tmux
	// for synchronized redraws (xterm.js honors DEC 2026).
	cmd := clcommon.TmuxCommand("-T", clcommon.TmuxSyncFeature, "attach-session", "-f", "ignore-size", "-t", clcommon.ExactTarget(pin.tmux))
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	// Nested clients must not inherit TMUX: attach to the server explicitly.
	env := cmd.Env[:0]
	for _, v := range cmd.Env {
		if !strings.HasPrefix(v, "TMUX=") {
			env = append(env, v)
		}
	}
	cmd.Env = env
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	if err != nil {
		return nil, err
	}
	// Do not wire the PTY master write side: a writable tmux client would
	// interpret prefix bindings and commands. Only its output is exposed.
	fedRendererAdd(pin.tmux, 1)
	return &fedPaneTerminal{f: f, cmd: cmd, pin: pin, resize: func(c, r int) error { return pty.Setsize(f, &pty.Winsize{Cols: uint16(c), Rows: uint16(r)}) }}, nil
}
func (p *fedPaneTerminal) Read(b []byte) (int, error) { return p.f.Read(b) }
func (p *fedPaneTerminal) Resize(cols, rows int) error {
	return p.resize(cols, rows)
}
func (p *fedPaneTerminal) Input(b []byte) error {
	p.inputMu.Lock()
	defer p.inputMu.Unlock()
	if p.closed {
		return nil
	}
	p.inputAt = time.Now()
	keys, dropped := p.keyboard.Feed(b)
	return p.sendKeys(keys, dropped)
}
func (p *fedPaneTerminal) InputPending() bool {
	p.inputMu.Lock()
	defer p.inputMu.Unlock()
	return p.keyboard.Pending()
}
func (p *fedPaneTerminal) FlushInput() error {
	p.inputMu.Lock()
	defer p.inputMu.Unlock()
	if p.closed || time.Since(p.inputAt) < 30*time.Millisecond {
		return nil
	}
	keys, dropped := p.keyboard.Flush()
	return p.sendKeys(keys, dropped)
}
func (p *fedPaneTerminal) sendKeys(keys []terminal.Key, dropped int) error {
	if dropped > 0 {
		slog.Debug("federation: discarded unknown keyboard sequences", "count", dropped)
	}
	for i := 0; i < len(keys); i++ {
		key := keys[i]
		if key.Mouse != nil && key.Mouse.Wheel() {
			// A touchpad emits bursts: coalesce a run of identical wheel
			// buttons into one pane query and one tmux action.
			n := 1
			for i+n < len(keys) && keys[i+n].Mouse != nil && keys[i+n].Mouse.Button == key.Mouse.Button {
				n++
			}
			i += n - 1
			if err := p.wheel(*key.Mouse, n); err != nil {
				return err
			}
			continue
		}
		if key.Mouse != nil {
			// A drag is a stream of motion reports: forward a run of clicks
			// and motion with one pane query and one send-keys.
			n := 1
			for i+n < len(keys) && keys[i+n].Mouse != nil && !keys[i+n].Mouse.Wheel() {
				n++
			}
			run := make([]terminal.Mouse, n)
			for j := range run {
				run[j] = *keys[i+j].Mouse
			}
			i += n - 1
			if err := p.mouse(run); err != nil {
				return err
			}
			continue
		}
		args := []string{"send-keys", "-t", p.pin.pane}
		if key.Name != "" {
			args = append(args, key.Name)
		} else {
			args = append(args, "-l", "--", terminal.TmuxLiteralArg(key.Literal))
		}
		// Names come only from the fixed keyboard table. User-controlled text is
		// exclusively a -l argument, escaped for tmux's argv separator grammar.
		if err := clcommon.TmuxCommand(args...).Run(); err != nil {
			return err
		}
	}
	return nil
}

// fedPaneMouse is the pane state that decides where a viewer's mouse event
// goes, as tmux would see it for a real client.
type fedPaneMouse struct {
	inMode, copyMode bool
	// standard, button and all are the application's mouse tracking modes
	// (1000, 1002, 1003); sgr and utf8 its requested report encoding.
	standard, button, all, sgr, utf8 bool
	// tmuxMouse is tmux's own mouse option.
	tmuxMouse bool
	// left/top/width/height locate the pane in client coordinates (0-based),
	// with statusTop rows of status line above the window.
	left, top, width, height, statusTop int
}

func (s fedPaneMouse) app() bool { return s.standard || s.button || s.all }

func (p *fedPaneTerminal) paneMouse() (fedPaneMouse, error) {
	var s fedPaneMouse
	out, err := clcommon.TmuxCommand("display-message", "-p", "-t", p.pin.pane,
		"#{pane_in_mode} #{==:#{pane_mode},copy-mode} #{mouse_any_flag} #{mouse_sgr_flag} #{mouse_utf8_flag} #{mouse} "+
			"#{pane_left} #{pane_top} #{pane_width} #{pane_height} #{?#{==:#{status-position},top},#{status},off} "+
			"#{?mouse_standard_flag,1,0} #{?mouse_button_flag,1,0} #{?mouse_all_flag,1,0}").Output()
	if err != nil {
		return s, err
	}
	f := strings.Fields(string(out))
	if len(f) != 14 {
		return s, errors.New("unexpected tmux pane mouse state")
	}
	on := func(s string) bool { return s == "1" || s == "on" }
	s.inMode, s.copyMode, s.sgr, s.utf8, s.tmuxMouse = on(f[0]), on(f[1]), on(f[3]), on(f[4]), on(f[5])
	s.standard, s.button, s.all = on(f[11]), on(f[12]), on(f[13])
	for i, dst := range []*int{&s.left, &s.top, &s.width, &s.height} {
		v, err := strconv.Atoi(f[6+i])
		if err != nil {
			return s, errors.New("unexpected tmux pane geometry")
		}
		*dst = v
	}
	switch f[10] {
	case "off":
	case "on":
		s.statusTop = 1
	default:
		s.statusTop, _ = strconv.Atoi(f[10])
	}
	if !s.copyMode {
		p.copyMode = false
	}
	return s, nil
}

// paneXY maps client coordinates to 1-based pane coordinates; inside is false
// when the point falls outside the pane (a border or the status line).
func (s fedPaneMouse) paneXY(x, y int) (px, py int, inside bool) {
	px, py = x-s.left, y-s.top-s.statusTop
	inside = px >= 1 && px <= s.width && py >= 1 && py <= s.height
	return min(max(px, 1), s.width), min(max(py, 1), s.height), inside
}

// encode renders a report at pane coordinates in the encoding the
// application asked for, or nil when it is not representable there.
func (s fedPaneMouse) encode(m terminal.Mouse, x, y int) []byte {
	if s.sgr {
		final := byte('M')
		if m.Release {
			final = 'm'
		}
		return fmt.Appendf(nil, "\x1b[<%d;%d;%d%c", m.Button, x, y, final)
	}
	b := m.Button
	if m.Release {
		// Legacy encodings cannot name the released button.
		b = b&^3 | 3
	}
	if s.utf8 && max(b, x, y)+32 > 127 || max(b, x, y)+32 > 255 {
		return nil
	}
	return []byte{0x1b, '[', 'M', byte(b + 32), byte(x + 32), byte(y + 32)}
}

func (p *fedPaneTerminal) sendMouse(seqs [][]byte) error {
	args := []string{"send-keys", "-t", p.pin.pane, "-H"}
	for _, seq := range seqs {
		for _, b := range seq {
			args = append(args, fmt.Sprintf("%02x", b))
		}
	}
	if len(args) == 4 {
		return nil
	}
	return clcommon.TmuxCommand(args...).Run()
}

// wheel applies count wheel events the way tmux's default WheelUpPane and
// WheelDownPane bindings would for a real client: the renderer PTY is
// read-only, so tmux never sees the viewer's mouse. A pane in copy mode
// scrolls its history; an application that requested mouse reporting (Claude
// Code's fullscreen TUI) gets the event re-encoded at pane coordinates; with
// tmux's mouse option on, wheel-up enters copy mode (Codex, shells).
func (p *fedPaneTerminal) wheel(w terminal.Mouse, count int) error {
	s, err := p.paneMouse()
	if err != nil {
		return err
	}
	dir := "scroll-down"
	if w.Up() {
		dir = "scroll-up"
	}
	// tmux's copy-mode wheel binding scrolls five lines per event.
	scroll := func() error {
		return clcommon.TmuxCommand("send-keys", "-t", p.pin.pane, "-X", "-N", strconv.Itoa(5*count), dir).Run()
	}
	switch {
	case s.copyMode:
		return scroll()
	case s.inMode:
		return nil
	case s.app():
		x, y, _ := s.paneXY(w.X, w.Y)
		seq := s.encode(w, x, y)
		if seq == nil {
			return nil
		}
		seqs := make([][]byte, count)
		for i := range seqs {
			seqs[i] = seq
		}
		return p.sendMouse(seqs)
	case w.Up() && s.tmuxMouse:
		if err := clcommon.TmuxCommand("copy-mode", "-e", "-t", p.pin.pane).Run(); err != nil {
			return err
		}
		p.copyMode = true
		return scroll()
	}
	return nil
}

// mouse forwards clicks, releases and drags to an application that requested
// mouse reporting (Claude Code's clickable TUI elements), as tmux's default
// MouseDown/Up/Drag bindings would pass them on with send-keys -M. Only the
// motion the application's tracking mode asks for is sent, and a press or
// hover outside the pane is dropped; a release or drag that leaves the pane is
// clamped to its edge so the application sees the gesture end. tmux's own
// mouse actions (selection into copy mode, pane selection) are not emulated:
// with a single pane and xterm.js selection in the browser they add little.
func (p *fedPaneTerminal) mouse(run []terminal.Mouse) error {
	s, err := p.paneMouse()
	if err != nil {
		return err
	}
	if s.inMode || !s.app() {
		return nil
	}
	var seqs [][]byte
	for _, m := range run {
		drag := m.Motion() && m.Button&3 != 3
		wanted := !m.Motion() || s.all || s.button && drag
		x, y, inside := s.paneXY(m.X, m.Y)
		if !wanted || !inside && !m.Release && !drag {
			continue
		}
		if seq := s.encode(m, x, y); seq != nil {
			seqs = append(seqs, seq)
		}
	}
	return p.sendMouse(seqs)
}

func (p *fedPaneTerminal) Close() error {
	p.once.Do(func() {
		p.inputMu.Lock()
		p.closed = true
		entered := p.copyMode
		p.inputMu.Unlock()
		if entered {
			// Copy mode belongs to the pane: left active it would swallow the
			// local operator's keys and automated send-keys. Re-check first so
			// a copy mode entered by someone since is left alone as far as we
			// can tell.
			out, err := clcommon.TmuxCommand("display-message", "-p", "-t", p.pin.pane, "#{==:#{pane_mode},copy-mode}").Output()
			if err == nil && strings.TrimSpace(string(out)) == "1" {
				_ = clcommon.TmuxCommand("send-keys", "-t", p.pin.pane, "-X", "cancel").Run()
			}
		}
		_ = p.f.Close()
		hangupProcessGroup(p.cmd.Process)
		_ = p.cmd.Wait()
		fedRendererAdd(p.pin.tmux, -1)
	})
	return nil
}

const fedIndicatorOption = "@tclaude-federation-viewers"

type fedIndicatorSaved struct {
	Status, Format, Size                string
	StatusLocal, FormatLocal, SizeLocal bool
	Installed, Previous                 string
}
type fedIndicator struct {
	saved  string
	peers  map[string]string
	format string
}

var fedIndicatorMu sync.Mutex
var fedIndicators = map[string]*fedIndicator{}

func fedWindowOption(window, name string) (string, bool, error) {
	out, err := clcommon.TmuxCommand("show-options", "-w", "-q", "-t", window, name).Output()
	if err != nil {
		return "", false, err
	}
	line := strings.TrimSuffix(string(out), "\n")
	if line == "" {
		return "", false, nil
	}
	// -v returns the exact value, without tmux's display quoting.
	val, err := clcommon.TmuxCommand("show-options", "-w", "-qv", "-t", window, name).Output()
	return strings.TrimSuffix(string(val), "\n"), true, err
}
func setFedWindowOption(window, name, value string) error {
	return clcommon.TmuxCommand("set-option", "-w", "-t", window, name, value).Run()
}
func setFedIndicator(pin *fedPanePin, id, peer string, readOnly bool) (func(), error) {
	fedIndicatorMu.Lock()
	defer fedIndicatorMu.Unlock()
	mark := fedIndicators[pin.window]
	if mark == nil {
		status, sl, err := fedWindowOption(pin.window, "pane-border-status")
		if err != nil {
			return nil, err
		}
		format, fl, err := fedWindowOption(pin.window, "pane-border-format")
		if err != nil {
			return nil, err
		}
		size, sizeLocal, err := fedWindowOption(pin.window, "window-size")
		if err != nil {
			return nil, err
		}
		raw, _ := json.Marshal(fedIndicatorSaved{Status: status, Format: format, Size: size, StatusLocal: sl, FormatLocal: fl, SizeLocal: sizeLocal})
		mark = &fedIndicator{saved: string(raw), peers: map[string]string{}}
		fedIndicators[pin.window] = mark
		if err := setFedWindowOption(pin.window, fedIndicatorOption, mark.saved); err != nil {
			delete(fedIndicators, pin.window)
			return nil, err
		}

		// With no non-ignored client tmux falls back to the ignore-size viewers.
		// No dimensions means atomically pin the window's existing size as manual.
		if err := clcommon.TmuxCommand("resize-window", "-t", pin.window).Run(); err != nil {
			restoreFedIndicator(pin.window, mark.saved)
			delete(fedIndicators, pin.window)
			return nil, err
		}
	}
	mode := "WATCH"
	if !readOnly {
		mode = "INPUT"
	}
	mark.peers[id] = "REMOTE " + mode + " " + proto.SafeName(peer, false)
	if err := updateFedIndicator(pin.window, mark); err != nil {
		delete(mark.peers, id)
		if len(mark.peers) == 0 {
			restoreFedIndicator(pin.window, mark.saved)
			delete(fedIndicators, pin.window)
		}
		return nil, err
	}
	return func() {
		fedIndicatorMu.Lock()
		defer fedIndicatorMu.Unlock()
		m := fedIndicators[pin.window]
		if m == nil {
			return
		}
		delete(m.peers, id)
		if len(m.peers) == 0 {
			restoreFedIndicator(pin.window, m.saved)
			delete(fedIndicators, pin.window)
		} else {
			_ = updateFedIndicator(pin.window, m)
		}
	}, nil
}
func updateFedIndicator(window string, m *fedIndicator) error {
	if m.format != "" {
		current, _, err := fedWindowOption(window, "pane-border-format")
		status, _, statusErr := fedWindowOption(window, "pane-border-status")
		if err != nil || statusErr != nil || current != m.format || status != "top" {
			return errors.New("remote viewer indicator was changed locally")
		}
	}

	labels := []string{}
	for _, s := range m.peers {
		labels = append(labels, s)
	}
	sort.Strings(labels)
	// SafeName excludes tmux's # format introducer, so remote labels cannot
	// turn the indicator into a format command or shell expansion.
	m.format = "[" + strings.Join(labels, " | ") + "]"
	var saved fedIndicatorSaved
	if err := json.Unmarshal([]byte(m.saved), &saved); err != nil {
		return err
	}
	saved.Previous = saved.Installed
	saved.Installed = m.format
	raw, _ := json.Marshal(saved)
	m.saved = string(raw)
	if err := setFedWindowOption(window, fedIndicatorOption, m.saved); err != nil {
		return err
	}
	if err := setFedWindowOption(window, "pane-border-format", m.format); err != nil {
		return err
	}
	return setFedWindowOption(window, "pane-border-status", "top")
}
func restoreFedIndicator(window, saved string) {
	var old fedIndicatorSaved
	if json.Unmarshal([]byte(saved), &old) != nil {
		return
	}
	for _, o := range []struct {
		name, val string
		local     bool
	}{{"pane-border-status", old.Status, old.StatusLocal}, {"pane-border-format", old.Format, old.FormatLocal}, {"window-size", old.Size, old.SizeLocal}} {
		current, _, err := fedWindowOption(window, o.name)
		want := old.Installed
		if o.name == "pane-border-status" {
			want = "top"
		}
		if o.name == "window-size" {
			want = "manual"
		}
		// Preserve edits made by the local operator while a viewer was attached.
		ours := current == want || o.name == "pane-border-format" && old.Previous != "" && current == old.Previous
		if err != nil || !ours {
			continue
		}
		if o.local {
			_ = setFedWindowOption(window, o.name, o.val)
		} else {
			_ = clcommon.TmuxCommand("set-option", "-wu", "-t", window, o.name).Run()
		}
	}
	_ = clcommon.TmuxCommand("set-option", "-wu", "-t", window, fedIndicatorOption).Run()
}
func fedIndicatorPresent(window string) bool {
	fedIndicatorMu.Lock()
	defer fedIndicatorMu.Unlock()
	m := fedIndicators[window]
	if m == nil {
		return false
	}
	f, _, err := fedWindowOption(window, "pane-border-format")
	if err != nil || f != m.format {
		return false
	}
	s, _, err := fedWindowOption(window, "pane-border-status")
	size, _, sizeErr := fedWindowOption(window, "window-size")
	return err == nil && s == "top" && sizeErr == nil && size == "manual"
}

// Original options live on the tmux window so a daemon crash cannot lose the
// restore data. Only windows carrying our exact marker are touched at startup.
func cleanupFedTerminalIndicators() {
	fedIndicatorMu.Lock()
	defer fedIndicatorMu.Unlock()
	out, err := clcommon.TmuxCommand("list-windows", "-a", "-F", "#{window_id}").Output()
	if err != nil {
		return
	}
	for _, window := range strings.Fields(string(out)) {
		if !validTmuxObject(window, '@') {
			continue
		}
		saved, local, err := fedWindowOption(window, fedIndicatorOption)
		if err == nil && local && saved != "" {
			restoreFedIndicator(window, saved)
		}
	}
	fedIndicators = map[string]*fedIndicator{}
}

var _ io.ReadCloser = (*fedPaneTerminal)(nil)

// size queries the pinned pane. A pinned browser view renders at this size;
// only a view that owns the session's size (federation_terminal_owner.go)
// resizes the window.
func (pin *fedPanePin) size() (int, int, error) {
	raw, err := clcommon.TmuxCommand("display-message", "-p", "-t", pin.pane, "#{pane_width} #{pane_height}").Output()
	if err != nil {
		return 0, 0, err
	}
	var cols, rows int
	if _, err = fmt.Sscan(string(raw), &cols, &rows); err != nil || cols < 1 || rows < 1 || cols > 1000 || rows > 1000 {
		return 0, 0, errors.New("invalid pinned pane dimensions")
	}
	return cols, rows, nil
}

func (pin *fedPanePin) closureReason(peer string, p proto.SessionOpenPayload) string {
	trusted, _ := db.GetFederationPeer(peer)
	if trusted == nil {
		return "untrusted"
	}
	if !fedPeerAllows(peer, pin.group, fedTerminalSlug(p.ReadOnly)) {
		return "revoked"
	}
	a, err := db.GetAgent(pin.agent)
	if err != nil || a == nil || !a.Active() {
		return "exit"
	}
	if a.CurrentConvID != pin.conv {
		return "reincarnated"
	}
	row, err := db.LoadSession(pin.session)
	if err != nil || row == nil || row.Status == session.StatusExited || !session.IsTmuxSessionAlive(pin.tmux) {
		return "exit"
	}
	if fedSessionIncarnation(row) != pin.incarnation {
		return "reincarnated"
	}
	current, err := fedPaneIdentity(pin.tmux)
	if err != nil {
		return "exit"
	}
	if current.pane != pin.pane || current.serverSession != pin.serverSession {
		return "reincarnated"
	}
	return "revoked"
}
