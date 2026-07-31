package agentd

import (
	"sync"

	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// sandbox_tooling_presence.go — the dashboard's sandbox-capability disclosure,
// reduced to "is the tooling installed" and latched once it is.
//
// It used to be a live capability probe run on the 2-second /api/snapshot poll,
// behind a 60-second cache. On expiry one poll paid, serially:
//
//   - `bwrap` forked twice to build throwaway namespaces (interactive + server
//     boundary), each bounded by a 5s timeout;
//   - `claude --version` and `codex --version` EXECUTED — booting the real
//     Node CLIs to read a version string — each bounded by a 3s timeout.
//
// Measured on the author's host: ~88 ms for `claude --version` alone, ~96 ms
// for the engine identities together, against a ~13 ms baseline request. That
// is one poll in thirty stalling on process startup it did not need, and under
// load the timeouts allow it to reach multiple seconds.
//
// None of that execution is required to disclose. The actionable half of the
// answer — "the tool is not installed" — is a PATH lookup, and the half that
// needs execution is re-derived at launch anyway, live and posture-exact, by
// sandboxImplementationHostFailure. So the poll now answers only the cheap
// half, and:
//
//   - a tool that is FOUND is latched and never looked up again;
//   - only tools still MISSING are re-checked, so an operator installing bwrap
//     mid-session still sees the disclosure clear on the next poll;
//   - when every tool is found there is nothing left to check and the poll
//     stops touching the filesystem entirely;
//   - a launch that fails on host capability calls
//     invalidateSandboxToolingPresence, which resumes checking — the one way a
//     latched-present tool can be observed to have gone away.
//
// The semantic cost is stated plainly at the wire fields: available now means
// INSTALLED, not WORKING. Nothing that refuses a launch may read this.

// sandboxToolKey identifies one required external tool in the presence table.
// Stable strings rather than an enum so a harness-derived key can be composed.
type sandboxToolKey string

const (
	sandboxToolLayerHost       sandboxToolKey = "tclaude-layer-host"
	sandboxToolLayerServerHost sandboxToolKey = "tclaude-layer-server-host"
)

// stackedEngineToolKey is the per-harness nested-sandbox engine key. Only
// harnesses that OWN a nested sandbox get one: OpenCode has no sandbox of its
// own, so stacking is meaningless for it and it is absent from this table
// rather than reported as a missing tool.
func stackedEngineToolKey(harnessName string) sandboxToolKey {
	return sandboxToolKey("stacked-engine:" + harnessName)
}

// The presence predicates are indirected so flow tests can describe a host
// shape without owning the machine they run on. They mirror the launch-side
// seams in sandbox_implementation.go, and the test hook swaps both together so
// a test cannot accidentally describe a host whose disclosure and refusal
// disagree.
var (
	tclaudeLayerHostPresence       = session.TclaudeLayerHostToolingPresence
	tclaudeLayerServerHostPresence = session.TclaudeLayerServerHostToolingPresence
	stackedEnginePresence          = func(h *harness.Harness) error {
		if err := session.ValidateStackedSandboxHarness(h); err != nil {
			return err
		}
		return h.NestedSandbox.EnginePresence()
	}
)

var sandboxToolingMu sync.Mutex

// sandboxToolingFound is the latch: a key present here has been found and is
// never looked up again until invalidateSandboxToolingPresence clears it.
var sandboxToolingFound = map[sandboxToolKey]bool{}

// sandboxToolingMissing caches the refusal for a tool that was looked for and
// not found, so repeated reads within one poll share one answer. Unlike the
// found set this is re-derived on every poll — that is what lets an operator
// who installs the tool see the disclosure clear.
var sandboxToolingMissing = map[sandboxToolKey]error{}

// sandboxToolPresence answers one tool's presence, doing no work at all once
// the tool has been found. probe is only called while the tool is missing.
func sandboxToolPresence(key sandboxToolKey, probe func() error) error {
	sandboxToolingMu.Lock()
	if sandboxToolingFound[key] {
		sandboxToolingMu.Unlock()
		return nil
	}
	sandboxToolingMu.Unlock()

	// Probe OUTSIDE the lock. A PATH lookup is cheap but still touches the
	// filesystem, and holding the mutex across it would serialize every
	// concurrent snapshot behind one slow stat on a wedged mount.
	err := probe()

	sandboxToolingMu.Lock()
	defer sandboxToolingMu.Unlock()
	if err == nil {
		sandboxToolingFound[key] = true
		delete(sandboxToolingMissing, key)
		return nil
	}
	sandboxToolingMissing[key] = err
	return err
}

// invalidateSandboxToolingPresence drops every latch so the next poll looks
// again. Called when a LAUNCH refuses on host capability: that is the only
// evidence the daemon gets that a tool it already found may have gone away
// (or that the live probe disagrees with mere presence, which the operator
// should see reflected rather than a stale green).
func invalidateSandboxToolingPresence() {
	sandboxToolingMu.Lock()
	defer sandboxToolingMu.Unlock()
	clear(sandboxToolingFound)
	clear(sandboxToolingMissing)
}

// resetSandboxImplHostProbeCache drops the disclosure latches. Test-only: a
// test that swaps a presence predicate must be able to observe the new answer
// rather than one latched before the swap.
func resetSandboxImplHostProbeCache() {
	invalidateSandboxToolingPresence()
}
