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
//     option and hook names, reply parsing and error classification. It is
//     one implementation, shared by every way of reaching tmux.
//   - A Transport owns HOW a command reaches a server: one method, Run, that
//     carries a tmux command and brings back its reply. clcommon.Tmux is the
//     transport of the local server (a forked client, agentd's control-mode
//     connection, or the flow-test simulator, whichever clcommon.Default
//     holds); NewClient puts the verbs on any other.
//
// Nothing outside this package calls a transport, starts a tmux process, or
// spells a tmux command line. TestNothingTalksToTmuxAroundTheVerbs enforces
// that, with the few named exceptions it lists. The one verb that hands back
// a command instead of running it is AttachCommand, because attaching needs a
// terminal the caller holds.
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
//
// A verb handed an empty target fails with ErrInvalidTarget without sending
// anything: tmux would read the empty -t as "whatever is current".
//
// Deadlines are a property of the Client (WithTimeout), not a parameter of
// each verb. The transport underneath takes the context a remote runner
// needs; a verb's caller only chooses how long it is willing to wait.
package tmuxcmd
