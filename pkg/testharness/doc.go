//go:build rewire

// Package testharness provides scaffolding for end-to-end flow tests
// that drive the agentd HTTP layer without spinning up real
// subprocesses, tmux servers, or sockets. See
// docs/plans/testing-strategy.md for the rationale.
//
// Phase 1 owns:
//   - World — per-test scratch HOME, fresh SQLite, FakeTmux, CCSimulator.
//   - FakeTmux — rewires clcommon.TmuxCommand to a no-op cmd that
//     records send-keys / kill-session / new-session calls and answers
//     has-session based on a fake session table.
//   - CCSimulator — synthesises CC's side of the spawn handshake: a
//     session row with conv-id materialised + tmux session marked alive.
//   - http.go — request helpers that drive the daemon's mux.
//
// Phase 2 will broaden to FakeClock + os.UserHomeDir rewires + more
// scenarios.
package testharness
