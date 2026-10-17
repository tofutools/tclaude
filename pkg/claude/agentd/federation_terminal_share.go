package agentd

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

// Shared sizing. Several clients can be attached to a session: local tmux
// clients and remote viewers. Remote viewers that share the size (ShareSize)
// report the size they could show; the window takes, per axis, the smallest
// size across every attached client — those viewers and the local clients —
// like tmux's own window-size smallest. Each sharing viewer then renders the
// window at that shared size and its browser says when another client limits
// it. Alone, a viewer is simply the minimum of one.
//
// Take-over is the exception: with the sessions.takeover grant, a viewer may
// disconnect every other client (the one-viewer rule a local web terminal's
// forced attach applies) and so have the window to itself.

// fedTerminalTakenOver closes a viewer displaced by another viewer, remote or
// a terminal on this node.
const fedTerminalTakenOver = "taken_over"

// fedRenderers counts live federation renderer clients per tmux session (see
// fedRendererActive).
var (
	fedRenderersMu sync.Mutex
	fedRenderers   = map[string]int{}
)

func fedRendererAdd(tmuxSession string, delta int) {
	fedRenderersMu.Lock()
	defer fedRenderersMu.Unlock()
	fedRenderers[tmuxSession] += delta
	if fedRenderers[tmuxSession] <= 0 {
		delete(fedRenderers, tmuxSession)
	}
}

// fedRendererActive reports whether a remote viewer's renderer is attached to
// tmuxSession, which a local web terminal's teardown must then leave attached.
func fedRendererActive(tmuxSession string) bool {
	fedRenderersMu.Lock()
	defer fedRenderersMu.Unlock()
	return fedRenderers[tmuxSession] > 0
}

// fedLocalClient is one attached tmux client other than a federation
// renderer, with its terminal size (0 when tmux did not report one).
type fedLocalClient struct {
	tty        string
	cols, rows int
}

// fedLocalClients lists tmuxSession's attached clients other than federation
// renderers, which attach with ignore-size.
func fedLocalClients(tmuxSession string) ([]fedLocalClient, error) {
	out, err := clcommon.TmuxCommand("list-clients", "-t", clcommon.ExactTarget(tmuxSession), "-F", "#{client_tty}\t#{client_flags}\t#{client_width}\t#{client_height}").Output()
	if err != nil {
		return nil, err
	}
	var clients []fedLocalClient
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(line, "\t")
		if len(f) < 2 || !strings.HasPrefix(f[0], "/dev/") || slices.Contains(strings.Split(f[1], ","), "ignore-size") {
			continue
		}
		c := fedLocalClient{tty: f[0]}
		if len(f) >= 4 {
			c.cols, _ = strconv.Atoi(f[2])
			c.rows, _ = strconv.Atoi(f[3])
		}
		clients = append(clients, c)
	}
	return clients, nil
}

// detachFedLocalClients detaches only tmuxSession's non-renderer clients.
func detachFedLocalClients(tmuxSession string) {
	clients, err := fedLocalClients(tmuxSession)
	if err != nil {
		return
	}
	for _, c := range clients {
		_ = clcommon.TmuxCommand("detach-client", "-t", c.tty).Run()
	}
}

// fedStatusLines is how many rows tmux's status line takes from every client
// of the session: a client's window area is its height minus these.
func fedStatusLines(pane string) int {
	out, err := clcommon.TmuxCommand("display-message", "-p", "-t", pane, "#{status}").Output()
	if err != nil {
		return 0
	}
	switch s := strings.TrimSpace(string(out)); s {
	case "on":
		return 1
	case "off", "":
		return 0
	default:
		if n, err := strconv.Atoi(s); err == nil && n >= 0 && n <= 5 {
			return n
		}
		return 0
	}
}

// fedWindowViewers returns the live incoming viewers of pin's window other
// than self (nil: all of them).
func (rt *fedRuntime) fedWindowViewers(pin *fedPanePin, self *fedTerminalView) []*fedTerminalView {
	rt.terminalsMu.Lock()
	var views []*fedTerminalView
	for _, v := range rt.terminalsLocked().views {
		if v != self && v.Incoming {
			views = append(views, v)
		}
	}
	rt.terminalsMu.Unlock()
	var out []*fedTerminalView
	for _, v := range views {
		v.mu.Lock()
		other, stopped := v.pin, v.stopped
		v.mu.Unlock()
		if !stopped && other != nil && v.ctx.Err() == nil && other.window == pin.window && other.serverSession == pin.serverSession {
			out = append(out, v)
		}
	}
	return out
}

// fedOthersLabels describes the other attached clients without naming peers:
// one peer learns nothing about who else a node shares its sessions with.
func fedOthersLabels(locals, viewers int) []string {
	out := []string{}
	switch {
	case locals == 1:
		out = append(out, "a local terminal")
	case locals > 1:
		out = append(out, strconv.Itoa(locals)+" local terminals")
	}
	switch {
	case viewers == 1:
		out = append(out, "another remote viewer")
	case viewers > 1:
		out = append(out, strconv.Itoa(viewers)+" other remote viewers")
	}
	return out[:min(len(out), proto.MaxSessionOthers)]
}

// sanitizeFedOthers bounds a peer's description of the other clients to a few
// short labels of safe characters.
func sanitizeFedOthers(in []string) []string {
	out := []string{}
	for _, s := range in {
		if strings.TrimSpace(s) != "" {
			out = append(out, proto.SafeName(s, false))
		}
		if len(out) == proto.MaxSessionOthers {
			break
		}
	}
	return out
}

// fedCatalogGroupHasCap reports whether peer's cached catalog grants cap on
// its group named group.
func fedCatalogGroupHasCap(peer, group, cap string) bool {
	cat, _, err := fedCatalogFor(peer)
	if err != nil || cat == nil {
		return false
	}
	for _, g := range cat.Groups {
		if g.Name == group && g.HasCap(cap) {
			return true
		}
	}
	return false
}

// planFedTakeOver describes what else is attached to v's session, and whether
// a take-over has anyone to displace. It disconnects nobody: the caller
// displaces (displaceFedClients) once the open can no longer fail on this side.
func (rt *fedRuntime) planFedTakeOver(pin *fedPanePin, v *fedTerminalView, takeOver bool) (displace bool, others []string) {
	locals, err := fedLocalClients(pin.tmux)
	if err != nil {
		return false, []string{}
	}
	viewers := rt.fedWindowViewers(pin, v)
	if takeOver {
		return len(locals)+len(viewers) > 0, []string{}
	}
	return false, fedOthersLabels(len(locals), len(viewers))
}

// displaceFedClients carries out a take-over: it closes the window's other
// viewers, each told it was taken over, and detaches the session's local
// clients. It lists both afresh, so whatever attached since the plan goes too;
// this viewer's own renderer is a federation renderer and is never listed.
// It does not wait: the closed viewers release their renderers and indicator
// entries on their own.
func (rt *fedRuntime) displaceFedClients(pin *fedPanePin, self *fedTerminalView) {
	for _, v := range rt.fedWindowViewers(pin, self) {
		v.mu.Lock()
		if v.reason == "" {
			v.reason = fedTerminalTakenOver
		}
		v.mu.Unlock()
		v.cancel()
		recordFederationAudit("sessions.attach.taken_over", v.Peer, "", v.Group, fmt.Sprintf("viewer=%s by=%s", v.ID, self.ID), 200)
	}
	detachFedLocalClients(pin.tmux)
}

// fedResizeWindow sets window to exactly cols x rows. The window-size option
// becomes manual, which the indicator already installed and restores.
func fedResizeWindow(window string, cols, rows int) error {
	return clcommon.TmuxCommand("resize-window", "-t", window, "-x", strconv.Itoa(cols), "-y", strconv.Itoa(rows)).Run()
}

// fedResizeSettle is how long size changes are coalesced: a browser drag sends
// a burst of sizes, and each recompute forks tmux commands.
const fedResizeSettle = 75 * time.Millisecond

// fedDebounce runs fn once, settle after the first of a burst of kicks.
type fedDebounce struct {
	kickCh chan struct{}
	settle time.Duration
}

func newFedDebounce(settle time.Duration) *fedDebounce {
	return &fedDebounce{kickCh: make(chan struct{}, 1), settle: settle}
}

// kick asks for a run; it never blocks.
func (d *fedDebounce) kick() {
	select {
	case d.kickCh <- struct{}{}:
	default:
	}
}

func (d *fedDebounce) run(ctx context.Context, fn func()) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-d.kickCh:
		}
		timer := time.NewTimer(d.settle)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		// Kicks during the settle are part of this burst.
		select {
		case <-d.kickCh:
		default:
		}
		fn()
	}
}

// fedWindowSizer keeps one window at the shared size of its clients while
// sharing viewers are attached.
type fedWindowSizer struct {
	rt       *fedRuntime
	pin      *fedPanePin
	key      string
	members  int
	cancel   context.CancelFunc
	debounce *fedDebounce
	applied  [2]int // the window size last set, as cols x window rows
}

var (
	fedSizersMu sync.Mutex
	fedSizers   = map[string]*fedWindowSizer{}
)

// joinFedWindowSizer registers a sharing viewer of pin's window and asks for a
// recompute; leave undoes it.
func (rt *fedRuntime) joinFedWindowSizer(pin *fedPanePin) *fedWindowSizer {
	key := pin.serverSession + "/" + pin.window
	fedSizersMu.Lock()
	s := fedSizers[key]
	if s == nil {
		ctx, cancel := context.WithCancel(rt.ctx)
		s = &fedWindowSizer{rt: rt, pin: pin, key: key, cancel: cancel, debounce: newFedDebounce(fedResizeSettle)}
		fedSizers[key] = s
		go s.debounce.run(ctx, s.apply)
	}
	s.members++
	fedSizersMu.Unlock()
	s.kick()
	return s
}

func (s *fedWindowSizer) leave() {
	fedSizersMu.Lock()
	s.members--
	last := s.members <= 0
	if last {
		s.cancel()
		if fedSizers[s.key] == s {
			delete(fedSizers, s.key)
		}
	}
	fedSizersMu.Unlock()
	if !last {
		s.kick()
	}
}

// kick asks for a recompute (coalesced): a viewer joined, left or resized, or
// the periodic poll for local clients, which come and go without telling us.
func (s *fedWindowSizer) kick() { s.debounce.kick() }

// apply sets the window to the per-axis minimum of every attached client and
// tells each sharing viewer its grid and what else is attached. Sizes are
// whole client terminals; the window gets that minus the status line, and
// each viewer's grid (its renderer client) is the full terminal size again.
func (s *fedWindowSizer) apply() {
	views := s.rt.fedWindowViewers(s.pin, nil)
	locals, err := fedLocalClients(s.pin.tmux)
	if err != nil {
		return
	}
	cols, rows := sharedFedSize(views, locals)
	status := fedStatusLines(s.pin.pane)
	if cols > 0 && rows > status {
		size := [2]int{cols, rows - status}
		if size != s.applied {
			if err := fedResizeWindow(s.pin.window, size[0], size[1]); err != nil {
				slog.Debug("federation: sizing the shared window failed", "error", err)
			} else {
				s.applied = size
			}
		}
	}
	for _, v := range views {
		if v.sharing {
			v.offerShared(cols, rows, fedOthersLabels(len(locals), len(views)-1))
		}
	}
}

// sharedFedSize is the per-axis minimum over the sharing viewers' reported
// sizes and the local clients' sizes; 0 x 0 while nobody reported one.
func sharedFedSize(views []*fedTerminalView, locals []fedLocalClient) (cols, rows int) {
	take := func(c, r int) {
		if c < 1 || r < 1 {
			return
		}
		if cols == 0 || c < cols {
			cols = c
		}
		if rows == 0 || r < rows {
			rows = r
		}
	}
	for _, v := range views {
		if v.sharing {
			v.mu.Lock()
			c, r := v.wantCols, v.wantRows
			v.mu.Unlock()
			take(c, r)
		}
	}
	for _, l := range locals {
		take(l.cols, l.rows)
	}
	return cols, rows
}

// setWant records the size a sharing viewer could show.
func (v *fedTerminalView) setWant(cols, rows int) {
	v.mu.Lock()
	v.wantCols, v.wantRows = cols, rows
	v.mu.Unlock()
}

// offerShared hands a sharing viewer its grid and the other clients; its serve
// loop applies them (sharedUpdate) when they changed.
func (v *fedTerminalView) offerShared(cols, rows int, others []string) {
	v.mu.Lock()
	changed := !slices.Equal(others, v.liveOthers) || cols > 0 && (cols != v.sharedCols || rows != v.sharedRows)
	if cols > 0 {
		v.sharedCols, v.sharedRows = cols, rows
	}
	v.liveOthers = others
	ch := v.sizeCh
	v.mu.Unlock()
	if changed && ch != nil {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// sharedUpdate returns the grid to render, and the other clients' Status body
// when they changed since the last one sent.
func (v *fedTerminalView) sharedUpdate() (cols, rows int, status []byte) {
	v.mu.Lock()
	defer v.mu.Unlock()
	cols, rows = v.sharedCols, v.sharedRows
	if !slices.Equal(v.liveOthers, v.sentOthers) {
		v.sentOthers = v.liveOthers
		status, _ = json.Marshal(proto.SessionStatus{Others: v.liveOthers})
	}
	return cols, rows, status
}
