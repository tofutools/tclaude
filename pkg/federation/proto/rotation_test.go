package proto

import (
	"testing"
	"time"
)

func TestIdentityRotationRequiresBothKeysAndPinnedChain(t *testing.T) {
	a, _ := NewIdentity()
	b, _ := NewIdentity()
	c, _ := NewIdentity()
	now := time.Now()
	ab, err := NewRotation(a, b, "", 1, now, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	bc, err := NewRotation(b, c, ab.ID, 2, now, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err = VerifyRotationChain([]Rotation{ab, bc}, a.ID(), a.Pub, c.ID()); err != nil {
		t.Fatal(err)
	}
	if err = VerifyRotationChain([]Rotation{ab}, c.ID(), c.Pub, b.ID()); err == nil {
		t.Fatal("accepted unpinned root")
	}
	altered := ab
	altered.NewID = c.ID()
	if altered.Verify() == nil {
		t.Fatal("accepted altered successor")
	}
	altered = ab
	altered.NewSignature = nil
	if altered.Verify() == nil {
		t.Fatal("accepted missing new-key proof")
	}
	altered = ab
	altered.ActivateAt = now.Add(-time.Hour)
	if altered.Verify() == nil {
		t.Fatal("accepted altered activation")
	}
	bc.Previous = ""
	if VerifyRotationChain([]Rotation{ab, bc}, a.ID(), a.Pub, c.ID()) == nil {
		t.Fatal("accepted broken chain")
	}
}
