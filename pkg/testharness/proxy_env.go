package testharness

import (
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/session"
)

// ClearModelTransportProxyEnv neutralizes the proxy routing variables for one
// test.
//
// The filtered model-transport seam inspects the HOST environment plus the
// launch's authored overrides, and refuses the launch when a routing variable
// is set — a real destination behind a proxy cannot be checked against the
// authored allow list. That refusal is the designed behavior and this helper
// does not weaken it: it only stops the SHELL THAT RAN `go test` from being a
// silent input to it. A developer or agent sandbox that exports HTTPS_PROXY
// otherwise turns every filtered-launch fixture red, including the ones whose
// whole job is to prove the seam refuses for some OTHER reason — those assert
// on the refusal text, so an ambient proxy makes them pass the wrong refusal
// or fail on the right one for the wrong reason.
//
// It is deliberately NOT a TestMain-wide unset. Test processes started INSIDE
// a filtered floor re-exec this same binary and read the proxy variables the
// floor injected as their measurement; clearing those globally would make
// those probes measure nothing.
//
// Tests that want a proxy variable as an INPUT should set it after calling
// this, or not call it at all.
func ClearModelTransportProxyEnv(t *testing.T) {
	t.Helper()
	for _, name := range session.ModelTransportProxyVariables() {
		t.Setenv(name, "")
	}
}
