package perfstats

import "testing"

func TestSubprocessDeltaCountsOnlyTheWindow(t *testing.T) {
	CountSubprocess("tmux test-only-a")
	before := Take()
	CountSubprocess("tmux test-only-a")
	CountSubprocess("tmux test-only-b")
	CountSubprocess("tmux test-only-b")
	after := Take()

	got := SubprocessDelta(before, after)
	want := []SubprocessCount{{"tmux test-only-b", 2}, {"tmux test-only-a", 1}}
	if len(got) != len(want) {
		t.Fatalf("delta = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("delta = %+v, want %+v", got, want)
		}
	}
	if after.SubprocessTotal-before.SubprocessTotal != 3 {
		t.Fatalf("total delta = %d, want 3", after.SubprocessTotal-before.SubprocessTotal)
	}
}
