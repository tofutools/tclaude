package common

// The canonical terminal size for managed harness panes (TCL-1136).
//
// Before this existed, a managed pane's size was an accident of its history:
// detached spawns got tmux's default-size (80x24, or whatever the operator's
// tmux.conf says), interactive launches inherited the launching terminal, and
// — because the server runs with `window-size latest` — any client that ever
// attached (browser terminal, native attach) resized the window to itself
// and LEFT it there on detach. Fleet sizes diverged purely by attachment
// history, and the divergence was implicated in real failures (the Copilot
// stdin-wedge panes were exactly the ones carrying a stale attached-client
// size).
//
// One rule replaces that accident: every managed pane is CREATED at this
// size (explicit -x/-y on every new-session site), never at a tmux default.
// After creation the size follows the attached clients (`window-size
// latest`) and simply stays where the last client left it — detach-time
// steering back to canonical (a client-detached hook, an immediate
// normalize on returning attach paths, and a periodic agentd sweep) was
// removed: the hook segfaulted the shared tmux server under mass detach,
// and none of it solved the drift problem it targeted.
//
// 200x50 rather than tmux's 80x24: it is close to what real browser/desktop
// terminals actually are, so the attach→fit→detach→normalize cycle moves the
// window less (smaller SIGWINCH deltas for the harness to re-layout through),
// and wide panes render modern TUIs and forensic screen captures better. One
// constant for every harness — OpenCode's runtime previously pinned its own
// 80x24 here.
const (
	CanonicalAgentPaneWidth  = 200
	CanonicalAgentPaneHeight = 50
)
