package agentd

import (
	"testing"
	"time"
)

func TestFederationSessionObserverDropsDisconnectedLowerBounds(t *testing.T) {
	// A new connection may observe the same state after an unseen working turn.
	// Keeping the prior observation would claim that unobserved gap as waiting.
	rt := &fedRuntime{
		sessionObservations: map[string]fedSessionObservation{"agent/runtime": {state: "idle", since: time.Now().Add(-time.Hour)}},
		sessionSent:         map[string]string{"peer/group": "old snapshot"},
	}
	rt.pushSessionTransitions() // No connected hub.
	if len(rt.sessionObservations) != 0 || len(rt.sessionSent) != 0 {
		t.Fatal("disconnection retained waiting lower bounds or delivery baseline")
	}
}
