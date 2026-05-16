package session

import (
	"errors"
	"strings"
	"testing"
)

// withClaudeAncestorCheck swaps ClaudeAncestorCheck for the duration of
// a test and restores it via t.Cleanup. The whole point of the seam:
// simulate "has a claude ancestor" without running under Claude Code.
func withClaudeAncestorCheck(t *testing.T, has bool) {
	t.Helper()
	prev := ClaudeAncestorCheck
	ClaudeAncestorCheck = func() bool { return has }
	t.Cleanup(func() { ClaudeAncestorCheck = prev })
}

// The guard must refuse when the ancestor check reports a claude/node
// ancestor, and the refusal must be matchable via errors.Is so callers
// can branch on it regardless of the appended human-readable text.
func TestGuardAgainstNestedSpawn_RefusesUnderClaude(t *testing.T) {
	withClaudeAncestorCheck(t, true)

	err := GuardAgainstNestedSpawn()
	if err == nil {
		t.Fatal("GuardAgainstNestedSpawn() = nil; want an error when a claude/node ancestor is present")
	}
	if !errors.Is(err, ErrNestedClaudeSpawn) {
		t.Errorf("error %v does not wrap ErrNestedClaudeSpawn", err)
	}
	// The message must explain the sanctioned alternative, not just refuse.
	if !strings.Contains(err.Error(), "tclaude agent spawn") {
		t.Errorf("guard error should point at `tclaude agent spawn`; got: %s", err.Error())
	}
}

// The guard must allow a normal invocation — no claude/node ancestor,
// e.g. a human in a plain shell or a daemon-forked `session new`.
func TestGuardAgainstNestedSpawn_AllowsWithoutClaude(t *testing.T) {
	withClaudeAncestorCheck(t, false)

	if err := GuardAgainstNestedSpawn(); err != nil {
		t.Errorf("GuardAgainstNestedSpawn() = %v; want nil when there is no claude/node ancestor", err)
	}
}

// runNew must be wired to the guard: a refused guard short-circuits
// before any tmux work, so a CC instance running bare `tclaude` /
// `tclaude session new` is stopped at the door.
func TestRunNew_BlockedUnderClaude(t *testing.T) {
	withClaudeAncestorCheck(t, true)

	err := runNew(&NewParams{})
	if err == nil {
		t.Fatal("runNew under a claude ancestor should be refused")
	}
	if !errors.Is(err, ErrNestedClaudeSpawn) {
		t.Errorf("runNew error %v does not wrap ErrNestedClaudeSpawn", err)
	}
}
