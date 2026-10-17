package agentd

import (
	"context"
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

// One attached viewer per session: a local web terminal force-attaches
// (`attach-session -d`, dashboard_term.go) and so displaces every other client.
// A remote viewer follows the same rule. Alone on the session it owns the
// window size, so the pane fits the viewer instead of keeping whatever size a
// detached session last had. With others attached it opens pinned to their
// size, is told who they are, and may reopen with TakeOver to disconnect them.
//
// Ownership is decided once, at open: a pinned view is not promoted when the
// others later leave (reopening it claims the size).

// fedTerminalTakenOver closes a viewer displaced by another viewer, remote or
// a terminal on this node.
const fedTerminalTakenOver = "taken_over"

// fedSizeClaimMu serialises the count-and-mark step of size claims, so two
// viewers opening at once cannot both find the session empty and both own it.
// It covers only that step: displacement and its tmux commands run unlocked.
var fedSizeClaimMu sync.Mutex

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

// fedLocalClients lists the ttys of tmuxSession's attached clients other than
// federation renderers, which attach with ignore-size.
func fedLocalClients(tmuxSession string) ([]string, error) {
	out, err := clcommon.TmuxCommand("list-clients", "-t", clcommon.ExactTarget(tmuxSession), "-F", "#{client_tty}\t#{client_flags}").Output()
	if err != nil {
		return nil, err
	}
	var ttys []string
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		tty, flags, ok := strings.Cut(line, "\t")
		if !ok || !strings.HasPrefix(tty, "/dev/") || slices.Contains(strings.Split(flags, ","), "ignore-size") {
			continue
		}
		ttys = append(ttys, tty)
	}
	return ttys, nil
}

// detachFedLocalClients detaches only tmuxSession's non-renderer clients.
func detachFedLocalClients(tmuxSession string) {
	ttys, err := fedLocalClients(tmuxSession)
	if err != nil {
		return
	}
	for _, tty := range ttys {
		_ = clcommon.TmuxCommand("detach-client", "-t", tty).Run()
	}
}

// fedWindowViewers returns the other live incoming viewers of pin's window.
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
	var out []string
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
	var out []string
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

// planFedSessionSize decides, without disconnecting anyone, whether v will own
// its session's window size. Alone it owns it. With others attached it owns it
// only when takeOver asks for it, and then reports true: the caller displaces
// them (displaceFedClients) once the open can no longer fail on this side.
// Otherwise v stays pinned and v.others says what else is attached. A failed
// client listing leaves v pinned and reports nothing.
func (rt *fedRuntime) planFedSessionSize(pin *fedPanePin, v *fedTerminalView, takeOver bool) (displace bool) {
	fedSizeClaimMu.Lock()
	defer fedSizeClaimMu.Unlock()
	locals, err := fedLocalClients(pin.tmux)
	if err != nil {
		return false
	}
	viewers := rt.fedWindowViewers(pin, v)
	alone := len(locals)+len(viewers) == 0
	v.mu.Lock()
	defer v.mu.Unlock()
	v.owner = alone || takeOver
	if !v.owner {
		v.others = fedOthersLabels(len(locals), len(viewers))
	}
	return !alone && takeOver
}

// displaceFedClients carries out a take-over: it closes the window's other
// viewers, each told it was taken over, and detaches the session's local
// clients. It lists both afresh, so whatever attached since the plan goes too;
// this viewer's own renderer is a federation renderer and is never listed.
// It does not wait: the closed viewers release their renderers and indicator
// entries on their own, and this viewer owns the size from here.
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

// fedResizeSettle is how long an owner's window resizes are coalesced: a
// browser drag sends a burst of sizes, and each would fork a tmux command.
const fedResizeSettle = 75 * time.Millisecond

// fedSizeCoalescer applies only the latest of a burst of sizes, settle after
// the first one arrives, and never re-applies the size already applied.
type fedSizeCoalescer struct {
	mu         sync.Mutex
	cols, rows int
	applied    [2]int
	kick       chan struct{}
	settle     time.Duration
	apply      func(cols, rows int) error
}

func newFedSizeCoalescer(settle time.Duration, apply func(cols, rows int) error) *fedSizeCoalescer {
	return &fedSizeCoalescer{kick: make(chan struct{}, 1), settle: settle, apply: apply}
}

// set records the latest size; it never blocks.
func (c *fedSizeCoalescer) set(cols, rows int) {
	c.mu.Lock()
	c.cols, c.rows = cols, rows
	c.mu.Unlock()
	select {
	case c.kick <- struct{}{}:
	default:
	}
}

// run applies sizes until ctx ends. A failed apply is logged, not fatal: the
// viewer keeps working at the window's previous size.
func (c *fedSizeCoalescer) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.kick:
		}
		timer := time.NewTimer(c.settle)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		c.mu.Lock()
		size := [2]int{c.cols, c.rows}
		c.mu.Unlock()
		if size == c.applied {
			continue
		}
		if err := c.apply(size[0], size[1]); err != nil {
			slog.Debug("federation: sizing the target window failed", "error", err)
			continue
		}
		c.applied = size
	}
}

func (v *fedTerminalView) sizeOwner() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.owner
}
