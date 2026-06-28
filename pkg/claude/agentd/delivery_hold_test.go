package agentd

import (
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/session"
)

// TestIsAwaitingHumanInput pins exactly which statuses hold mail delivery:
// the two "blocked on a human" states, and nothing else. A future status
// added to the session package does not silently start (or stop) holding mail
// without this test being updated.
func TestIsAwaitingHumanInput(t *testing.T) {
	held := []string{
		session.StatusAwaitingInput,
		session.StatusAwaitingPermission,
	}
	for _, s := range held {
		if !isAwaitingHumanInput(s) {
			t.Errorf("status %q must hold mail delivery", s)
		}
	}

	notHeld := []string{
		session.StatusWorking,
		session.StatusIdle,
		session.StatusMainAgentIdle,
		session.StatusError,
		session.StatusExited,
		"",
		"something-unknown",
	}
	for _, s := range notHeld {
		if isAwaitingHumanInput(s) {
			t.Errorf("status %q must NOT hold mail delivery", s)
		}
	}
}

func TestDeliveryOutcome_Predicates(t *testing.T) {
	cases := []struct {
		o             deliveryOutcome
		wantDelivered bool
		wantHeld      bool
	}{
		{outcomeQueued, false, false},
		{outcomeDelivered, true, false},
		{outcomeHeld, false, true},
	}
	for _, c := range cases {
		if c.o.delivered() != c.wantDelivered {
			t.Errorf("outcome %d delivered()=%v, want %v", c.o, c.o.delivered(), c.wantDelivered)
		}
		if c.o.held() != c.wantHeld {
			t.Errorf("outcome %d held()=%v, want %v", c.o, c.o.held(), c.wantHeld)
		}
	}
}
