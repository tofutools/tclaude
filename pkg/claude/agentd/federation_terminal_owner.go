package agentd

import (
	"context"
	"fmt"
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

// fedTerminalTakenOver closes a viewer displaced by another viewer, remote or
// a terminal on this node.
const fedTerminalTakenOver = "taken_over"

// fedSizeClaimMu serialises size claims so two viewers opening at once cannot
// both find the session empty and both own it.
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
		if !stopped && other != nil && other.window == pin.window && other.serverSession == pin.serverSession {
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

// claimFedSessionSize decides whether v owns its session's window size,
// disconnecting the other clients first when takeOver asks for it. A failed
// client listing leaves v pinned and reports nothing.
func (rt *fedRuntime) claimFedSessionSize(pin *fedPanePin, v *fedTerminalView, takeOver bool) {
	fedSizeClaimMu.Lock()
	defer fedSizeClaimMu.Unlock()
	locals, err := fedLocalClients(pin.tmux)
	if err != nil {
		return
	}
	viewers := rt.fedWindowViewers(pin, v)
	if takeOver && len(locals)+len(viewers) > 0 {
		// A displaced local web terminal tears down asynchronously, detaching
		// its session's clients; count this viewer as a renderer from now on,
		// so that teardown cannot drop the renderer it is about to attach.
		fedRendererAdd(pin.tmux, 1)
		v.addCleanup(func() { fedRendererAdd(pin.tmux, -1) })
		rt.displaceFedClients(v, locals, viewers)
		if locals, err = fedLocalClients(pin.tmux); err != nil {
			return
		}
		viewers = rt.fedWindowViewers(pin, v)
	}
	v.mu.Lock()
	v.owner = len(locals)+len(viewers) == 0
	v.others = fedOthersLabels(len(locals), len(viewers))
	v.mu.Unlock()
}

// displaceFedClients closes the other viewers (each told it was taken over)
// and detaches the local clients, then waits, bounded, for the viewers to
// release their renderers and indicator entries.
func (rt *fedRuntime) displaceFedClients(self *fedTerminalView, locals []string, viewers []*fedTerminalView) {
	for _, v := range viewers {
		v.mu.Lock()
		if v.reason == "" {
			v.reason = fedTerminalTakenOver
		}
		v.mu.Unlock()
		v.cancel()
		recordFederationAudit("sessions.attach.taken_over", v.Peer, "", v.Group, fmt.Sprintf("viewer=%s by=%s", v.ID, self.ID), 200)
	}
	for _, tty := range locals {
		_ = clcommon.TmuxCommand("detach-client", "-t", tty).Run()
	}
	ctx, cancel := context.WithTimeout(self.ctx, 2*time.Second)
	defer cancel()
	for _, v := range viewers {
		rt.waitKickedTerminal(ctx, v)
	}
}

// fedResizeWindow sets window to exactly cols x rows. The window-size option
// becomes manual, which the indicator already installed and restores.
func fedResizeWindow(window string, cols, rows int) error {
	return clcommon.TmuxCommand("resize-window", "-t", window, "-x", strconv.Itoa(cols), "-y", strconv.Itoa(rows)).Run()
}

func (v *fedTerminalView) sizeOwner() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.owner
}
