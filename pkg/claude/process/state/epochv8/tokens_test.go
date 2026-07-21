package epochv8

import "testing"

func TestHandoffTokenIsBindingBoundAndOpaque(t *testing.T) {
	checkpoint := testCheckpoint(t, "token-base", []AuthoritySeed{{LocalID: "frontier", ReservationID: "reservation", NodeID: "work", Kind: AuthorityFrontier, State: AuthorityVerifiedUnclaimed}})
	owner := checkpoint.View().Authorities[0].Identity
	token, err := HandoffToken(checkpoint, owner)
	if err != nil {
		t.Fatal(err)
	}
	if len(token) != 64 || token == string(owner) {
		t.Fatalf("token is not opaque canonical digest: %q", token)
	}
	resolved, err := ResolveHandoffToken(checkpoint, token)
	if err != nil || resolved != owner {
		t.Fatalf("resolve = %q, %v", resolved, err)
	}
	other := testCheckpoint(t, "token-other", []AuthoritySeed{{LocalID: "frontier", ReservationID: "reservation", NodeID: "work", Kind: AuthorityFrontier, State: AuthorityVerifiedUnclaimed}})
	if _, err := ResolveHandoffToken(other, token); err == nil {
		t.Fatal("token survived a different binding")
	}
}
