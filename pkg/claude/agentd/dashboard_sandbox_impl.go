package agentd

import (
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// dashboardSandboxImpl is the sandbox-IMPLEMENTATION catalog the spawn dialog
// and profile editor render: the closed set of implementations, which one is
// the default, and whether this host can actually run the experimental one.
//
// It is one nested object rather than the parallel sandbox_modes /
// default_sandbox / sandbox_mode_help arrays the sandbox MODE row uses, for a
// specific reason: the option copy, the experimental marking, and the
// availability disclosure have to stay together. Split across parallel fields
// they could drift, and a stale "experimental" label or a caveat that no longer
// matches the probe is exactly the overclaim epic requirement 12 forbids.
type dashboardSandboxImpl struct {
	// Options is the catalog in presentation order. Never null, so the page's
	// .map() is safe.
	Options []dashboardSandboxImplOption `json:"options"`
	// Default is the implementation a launch resolves to when nothing pins one.
	Default string `json:"default"`
	// HostAvailable reports whether this host can create the BASELINE
	// tclaude-layer namespace. It is disclosure, never permission: the dialog
	// keeps an unavailable implementation selectable and warns, because the
	// launch-time refusal is the authority and a dialog that quietly removed
	// the option would have replaced it. It is also deliberately not a promise
	// — an isolated-network launch needs strictly more than the baseline, so a
	// true here rules out a doomed launch without guaranteeing a good one.
	HostAvailable bool `json:"host_available"`
	// HostUnavailableReason names the concrete missing capability (no bwrap on
	// PATH, no unprivileged user namespaces, not Linux) so the operator can fix
	// it rather than guess. "" when HostAvailable is true.
	HostUnavailableReason string `json:"host_unavailable_reason,omitempty"`
}

type dashboardSandboxImplOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
	Descr string `json:"descr"`
	// Experimental marks an implementation that is not yet a supported posture.
	// The label carries the word too; this flag lets the UI style it.
	Experimental bool `json:"experimental,omitempty"`
}

// sandboxImplHostProbeTTL bounds how stale the DISCLOSED availability may be.
// The probe forks bwrap and the dashboard snapshot is polled continuously, so
// an uncached answer would fork on every poll. A stale answer is safe here
// precisely because this surface only discloses: the launch re-probes live and
// refuses on its own answer. Nothing that REFUSES may read this cache.
const sandboxImplHostProbeTTL = 60 * time.Second

var sandboxImplHostProbe struct {
	sync.Mutex
	checkedAt time.Time
	err       error
	valid     bool
}

// sandboxImplHostProbeNow is the clock, indirected so a test can age the cache
// without sleeping.
var sandboxImplHostProbeNow = time.Now

// cachedTclaudeLayerHostAvailability answers the DISCLOSURE question, at most
// once per TTL. Never call it from a path that refuses a launch.
func cachedTclaudeLayerHostAvailability() error {
	sandboxImplHostProbe.Lock()
	defer sandboxImplHostProbe.Unlock()
	now := sandboxImplHostProbeNow()
	if sandboxImplHostProbe.valid && now.Sub(sandboxImplHostProbe.checkedAt) < sandboxImplHostProbeTTL {
		return sandboxImplHostProbe.err
	}
	sandboxImplHostProbe.err = tclaudeLayerHostAvailability()
	sandboxImplHostProbe.checkedAt = now
	sandboxImplHostProbe.valid = true
	return sandboxImplHostProbe.err
}

// buildSandboxImplCatalog assembles the host-wide implementation catalog.
//
// The copy discipline here is load-bearing (epic requirement 12): each
// description says what the implementation DOES, never what it guarantees, and
// the platform caveat is stated rather than implied.
func buildSandboxImplCatalog() dashboardSandboxImpl {
	out := dashboardSandboxImpl{
		Options: []dashboardSandboxImplOption{
			{
				Value: string(sandboxpolicy.ImplementationHarnessBuiltin),
				Label: "Harness built-in",
				Descr: "Current behavior: the harness owns OS-level containment, using whatever sandbox it provides.",
			},
			{
				Value:        string(sandboxpolicy.ImplementationTclaudeLayer),
				Label:        "tclaude layer (experimental)",
				Experimental: true,
				Descr: "Runs the whole harness process inside a tclaude-owned bubblewrap mount namespace " +
					"and turns the harness's own sandbox off inside it. Linux only; requires bwrap and " +
					"unprivileged user namespaces.",
			},
		},
		Default: string(sandboxpolicy.ImplementationHarnessBuiltin),
	}
	if err := cachedTclaudeLayerHostAvailability(); err != nil {
		out.HostUnavailableReason = err.Error()
	} else {
		out.HostAvailable = true
	}
	return out
}

// resetSandboxImplHostProbeCache drops the disclosure cache. Test-only: a test
// that swaps the predicate must be able to observe the new answer rather than
// one cached before the swap.
func resetSandboxImplHostProbeCache() {
	sandboxImplHostProbe.Lock()
	defer sandboxImplHostProbe.Unlock()
	sandboxImplHostProbe.valid = false
	sandboxImplHostProbe.err = nil
}
