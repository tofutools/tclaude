// Package tmuxcmd is the typed surface for talking to tclaude's tmux server.
//
// Callers say what they mean (HasSession, KillSession, ClientTTYs, …) with
// typed targets and get typed results and errors back. They never build a
// tmux argv and never hold an *exec.Cmd, so the same call works whether the
// command reaches tmux by forking a local client, over agentd's control-mode
// connection, through the flow-test simulator, or (the reason this package
// exists) over a transport to a runner on another host or in a pod.
//
// # Layers
//
//   - This package owns WHAT is said to tmux: verbs, targets, the tclaude
//     option and hook names, reply parsing and error classification.
//   - Package common (clcommon) owns HOW a command reaches the server: the
//     Tmux seam, control-mode routing and the socket. Nothing outside this
//     package should call that layer's raw-argv functions directly.
//
// # Targets
//
// tmux resolves a -t value differently per command (see clcommon.ExactTarget
// for the full story): a session-typed command strips a leading '=', a
// pane-typed one does not. Getting that wrong silently acts on the wrong
// session. The target types here (SessionTarget, PaneTarget, ClientTarget)
// encode the rule once, and each verb accepts only the kind its tmux command
// takes.
//
// # Errors
//
// A command tmux ran and rejected comes back as *Error. errors.Is tells the
// common reasons apart without reading tmux's text at the call site:
// ErrNotFound (no such session, window, pane or client) and ErrNoServer (no
// tmux server is running). Anything else (tmux missing, a broken connection,
// ErrTimeout) is a failure to ask, not an answer, and callers that fail
// closed must treat it as "unknown" rather than "gone".
package tmuxcmd
