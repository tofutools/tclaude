package launchundo

import (
	"slices"
	"testing"
)

func TestUnwindRunsPendingStepsNewestFirstOnce(t *testing.T) {
	var got []string
	var u Undo
	u.Add(func() { got = append(got, "a") })
	ran := u.Add(func() { got = append(got, "b") })
	released := u.Add(func() { got = append(got, "c") })
	u.Add(func() { got = append(got, "d") })

	ran.Run()
	released.Release()
	u.Unwind()
	u.Unwind()

	if want := []string{"b", "d", "a"}; !slices.Equal(got, want) {
		t.Fatalf("undo order = %v, want %v", got, want)
	}
}

func TestCommitStopsOnlyTheDeferredUnwind(t *testing.T) {
	n := 0
	var u Undo
	u.Add(func() { n++ })
	u.Commit()
	u.UnwindUnlessCommitted()
	if n != 0 {
		t.Fatalf("committed launch unwound %d step(s)", n)
	}
	u.Unwind()
	if n != 1 {
		t.Fatalf("explicit Unwind after Commit ran %d step(s), want 1", n)
	}
}

func TestUncommittedDeferredUnwindRuns(t *testing.T) {
	n := 0
	func() {
		var u Undo
		defer u.UnwindUnlessCommitted()
		u.Add(func() { n++ })
	}()
	if n != 1 {
		t.Fatalf("uncommitted unwind ran %d step(s), want 1", n)
	}
}
