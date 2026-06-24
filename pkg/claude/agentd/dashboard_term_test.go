package agentd

import "testing"

// TestTermSessionName_DistinctForSharedShort8Prefix is the regression guard
// for the ad hoc browser-terminal session identity: the name must derive
// from the *full* convID, not short8(convID). Two conversations whose IDs
// share the same first 8 characters must NOT map to the same tmux session,
// or `tmux new-session -A` would attach them to the same browser terminal.
func TestTermSessionName_DistinctForSharedShort8Prefix(t *testing.T) {
	// Same short8 ("abcd1234"), different full IDs.
	a := "abcd1234-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	b := "abcd1234-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	if short8(a) != short8(b) {
		t.Fatalf("test premise broken: short8(%q)=%q != short8(%q)=%q", a, short8(a), b, short8(b))
	}
	if termSessionName(a, "start") == termSessionName(b, "start") {
		t.Errorf("session names collided for distinct convIDs sharing a short8 prefix: %q", termSessionName(a, "start"))
	}
}

// TestTermSessionName_Deterministic asserts the name is a pure function of
// (convID, which) — the same agent + direction must reattach to the same
// tmux session across reconnects.
func TestTermSessionName_Deterministic(t *testing.T) {
	id := "abcd1234-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	if got1, got2 := termSessionName(id, "start"), termSessionName(id, "start"); got1 != got2 {
		t.Errorf("termSessionName not deterministic: %q != %q", got1, got2)
	}
}

// TestTermSessionName_WhichDistinguishes asserts which is part of the
// identity, so start/current/worktree terminals for one agent are separate.
func TestTermSessionName_WhichDistinguishes(t *testing.T) {
	id := "abcd1234-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	if termSessionName(id, "start") == termSessionName(id, "worktree") {
		t.Errorf("different which produced the same session name: %q", termSessionName(id, "start"))
	}
}
