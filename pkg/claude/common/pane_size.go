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
// Two rules replace that accident:
//
//  1. Every managed pane is CREATED at this size (explicit -x/-y on every
//     new-session site), never at a tmux default.
//  2. A managed pane observed with zero attached clients is steered BACK to
//     this size (agentd's normalizeUnattachedPaneSizes), so an attach changes
//     the size only while someone is actually looking.
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
