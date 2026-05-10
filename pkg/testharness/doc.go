//go:build rewire

// Package testharness provides scaffolding for end-to-end flow tests
// that drive the agentd HTTP layer without spinning up real
// subprocesses, tmux servers, or sockets. See
// docs/plans/testharness-v2.md for the design rationale.
//
// The harness mocks at exactly two boundaries:
//   - clcommon.TmuxCommand — replaced by TmuxSim, which owns a
//     sessions table, routes send-keys to the attached CCSim, and
//     answers has-session against the alive flag.
//   - agentd.SpawnDetachedTclaude{New,Resume} — replaced by helpers
//     that build a CCSim, write its initial summary turn, register it
//     in TmuxSim, and write the SessionRow the daemon's poll loop
//     reads.
//
// Everything else — agentd, conv, agent, session, watch, web,
// statusbar — runs production code, reads real .jsonl files under
// $HOME (a t.TempDir per test), and refreshes its caches per its
// normal cadence.
//
// Components:
//   - World — per-test scratch HOME, fresh SQLite, TmuxSim, CCRegistry.
//   - CCSim — behavior-accurate Claude Code simulator: writes real
//     .jsonl turns in response to keystrokes (summary on start,
//     custom-title on /rename, user turn on plain text, etc.).
//   - TmuxSim — replacement for clcommon.TmuxCommand; routes
//     send-keys to the attached CCSim's Receive.
//   - Flow — Given/When/Then DSL on top of World for readable
//     scenarios.
//   - http.go — request helpers that drive the daemon's mux.
package testharness
