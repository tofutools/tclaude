package agentd

import (
	"errors"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

// A remote terminal follows its agent across a relaunch on the peer (a
// restart, resume or reincarnation), as a local terminal does: the browser
// redials by the agent's stable id (agt_…@peer) and this node re-resolves the
// agent's current pane from the peer's catalog. Two things lag that redial.
//
//   - The catalog still names the old launch: the peer refuses it as moved
//     (fedPaneMovedReason). This node asks the peer for a fresh catalog and
//     answers the browser "starting", so it redials until the new launch is
//     listed. The peer's own "starting" answer reads the same.
//   - The catalog lists no pane for the agent at all (the relaunch took the
//     old pane down before the new one came up). A viewer this node attached
//     moments ago is remembered (fedTerminalFollowWindow), so its redial still
//     reaches the peer, naming the agent and the group it was attached
//     through but no session: the peer answers whether the agent is starting
//     (wait), running a newer launch (moved, wait for the catalog) or not
//     running.
//
// The peer stays the authority: an open naming a stale or empty session never
// pins a newer pane (resolveFedPane), it only learns why it cannot.

// fedTerminalFollowWindow is how long after a viewer of an agent ended its
// redial may still reach the peer while the catalog lists no pane for the
// agent. It covers the browser's start wait (terminals-core.js START_WAIT_MS).
const fedTerminalFollowWindow = 2 * time.Minute

// fedCatalogRefreshEvery bounds the catalog requests a lagging catalog sends
// to one peer.
const fedCatalogRefreshEvery = 3 * time.Second

var errFedNoSharedSession = errors.New("session not shared with the requested watch/attach capability")

type fedTerminalFollow struct {
	group string
	at    time.Time
}

func fedFollowKey(peer, agentID string, readOnly bool) string {
	mode := "attach"
	if readOnly {
		mode = "watch"
	}
	return peer + "\x00" + agentID + "\x00" + mode
}

// rememberTerminalFollow records that a viewer of agentID on peer was attached
// through group just now.
func (rt *fedRuntime) rememberTerminalFollow(peer string, p proto.SessionOpenPayload) {
	if p.Shell != "" || p.Group == "" {
		return
	}
	now := time.Now()
	rt.terminalsMu.Lock()
	defer rt.terminalsMu.Unlock()
	st := rt.terminalsLocked()
	if st.follows == nil {
		st.follows = map[string]fedTerminalFollow{}
	}
	for k, f := range st.follows {
		if now.Sub(f.at) > fedTerminalFollowWindow {
			delete(st.follows, k)
		}
	}
	st.follows[fedFollowKey(peer, p.Agent, p.ReadOnly)] = fedTerminalFollow{group: p.Group, at: now}
}

// terminalFollowGroup returns the group a recent viewer of agentID on peer was
// attached through, while that group still shares the terminal.
func (rt *fedRuntime) terminalFollowGroup(peer, agentID string, readOnly bool) string {
	rt.terminalsMu.Lock()
	f, ok := rt.terminalsLocked().follows[fedFollowKey(peer, agentID, readOnly)]
	rt.terminalsMu.Unlock()
	if !ok || time.Since(f.at) > fedTerminalFollowWindow {
		return ""
	}
	cat, _, _ := fedCatalogFor(peer)
	for _, g := range fedTerminalGroups(cat) {
		if g.Name == f.group && g.HasCap(fedTerminalCap(readOnly)) {
			return g.Name
		}
	}
	return ""
}

// followTerminalTarget is the open for an agent the catalog lists no pane for,
// when a viewer of it was attached moments ago; ok is false otherwise.
func (rt *fedRuntime) followTerminalTarget(address string, readOnly bool) (*db.FederationPeer, proto.SessionOpenPayload, bool) {
	ref, sel, ok := strings.Cut(address, "@")
	if !ok || !strings.HasPrefix(ref, "agt_") {
		return nil, proto.SessionOpenPayload{}, false
	}
	peer, err := resolveFederationPeer(sel)
	if err != nil {
		return nil, proto.SessionOpenPayload{}, false
	}
	group := rt.terminalFollowGroup(peer.InstanceID, ref, readOnly)
	if group == "" {
		return nil, proto.SessionOpenPayload{}, false
	}
	return peer, proto.SessionOpenPayload{Agent: ref, Group: group, Stream: proto.NewEnvelopeID(), ReadOnly: readOnly, Cols: 80, Rows: 24}, true
}

// requestCatalogRefresh asks peer for a fresh catalog, at most once per
// fedCatalogRefreshEvery: the session updates a peer pushes on change usually
// arrive first, and this covers one that was lost.
func (rt *fedRuntime) requestCatalogRefresh(peer string) {
	now := time.Now()
	rt.terminalsMu.Lock()
	st := rt.terminalsLocked()
	if st.catalogAsked == nil {
		st.catalogAsked = map[string]time.Time{}
	}
	if last, ok := st.catalogAsked[peer]; ok && now.Sub(last) < fedCatalogRefreshEvery {
		rt.terminalsMu.Unlock()
		return
	}
	st.catalogAsked[peer] = now
	rt.terminalsMu.Unlock()
	goBackground(func() { rt.sendControl(peer, proto.KindCatalogReq, "", struct{}{}) })
}
