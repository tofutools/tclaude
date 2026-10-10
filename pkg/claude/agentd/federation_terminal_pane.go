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
	cmd := clcommon.TmuxCommand("attach-session", "-f", "ignore-size", "-t", clcommon.ExactTarget(pin.tmux))
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
	return &fedPaneTerminal{f: f, cmd: cmd, pin: pin, resize: func(c, r int) error { return pty.Setsize(f, &pty.Winsize{Cols: uint16(c), Rows: uint16(r)}) }}, nil
}
func (p *fedPaneTerminal) Read(b []byte) (int, error) { return p.f.Read(b) }
func (p *fedPaneTerminal) Resize(cols, rows int) error {
	return p.resize(cols, rows)
}
func (p *fedPaneTerminal) Input(b []byte) error {
	p.inputMu.Lock()
	defer p.inputMu.Unlock()
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
	if time.Since(p.inputAt) < 30*time.Millisecond {
		return nil
	}
	keys, dropped := p.keyboard.Flush()
	return p.sendKeys(keys, dropped)
}
func (p *fedPaneTerminal) sendKeys(keys []terminal.Key, dropped int) error {
	if dropped > 0 {
		slog.Debug("federation: discarded unknown keyboard sequences", "count", dropped)
	}
	for _, key := range keys {
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
func (p *fedPaneTerminal) Close() error {
	p.once.Do(func() { _ = p.f.Close(); hangupProcessGroup(p.cmd.Process); _ = p.cmd.Wait() })
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

// size queries the pinned pane; a browser renderer never chooses target size.
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
