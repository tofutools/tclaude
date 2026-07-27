package agentd

import (
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
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
	// HostAvailable reports whether this host can create the interactive
	// tclaude-layer namespace and terminal relay. It is disclosure, never
	// permission: the dialog
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
	// ServerHostAvailable and ServerHostUnavailableReason are the same
	// disclosure for a relay-free, non-interactive server boundary. Keeping the
	// topology answers separate prevents a missing terminal-relay capability
	// from being presented as an OpenCode executor refusal.
	ServerHostAvailable         bool   `json:"server_host_available"`
	ServerHostUnavailableReason string `json:"server_host_unavailable_reason,omitempty"`
	// Stacked is per-harness because each descriptor owns a different real
	// engine entry point. It is disclosure only; launch always re-resolves and
	// completes the nested round-trip inside the exact outer spec.
	Stacked map[string]dashboardStackedAvailability `json:"stacked"`
	// StackedAppArmorLikely says the host most likely denies the nested bwrap
	// stacked needs, because Ubuntu's bwrap-userns-restrict AppArmor policy is
	// present and neither unloaded nor in complain mode.
	//
	// It is host-wide rather than per-harness: the denying layer is the host's
	// policy, not any engine. It is also the only field here that can warn at
	// all in this case — the per-harness availability above resolves the engine
	// and reports AVAILABLE on exactly these hosts, because nothing short of
	// the launch-time round-trip tries the nested wall. Hence "likely": the
	// dashboard may point at the guide, never decide.
	StackedAppArmorLikely bool `json:"stacked_apparmor_userns_likely,omitempty"`
}

type dashboardStackedAvailability struct {
	Available          bool   `json:"available"`
	UnavailableReason  string `json:"unavailable_reason,omitempty"`
	ExecutableIdentity string `json:"executable_identity,omitempty"`
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

type sandboxImplHostProbeCache struct {
	sync.Mutex
	checkedAt time.Time
	err       error
	valid     bool
}

var sandboxImplHostProbe sandboxImplHostProbeCache
var sandboxImplServerHostProbe sandboxImplHostProbeCache
var sandboxImplStackedProbeMu sync.Mutex
var sandboxImplStackedProbe = map[string]struct {
	checkedAt time.Time
	value     dashboardStackedAvailability
}{}

// sandboxImplHostProbeNow is the clock, indirected so a test can age the cache
// without sleeping.
var sandboxImplHostProbeNow = time.Now

// cachedTclaudeLayerHostAvailability answers the DISCLOSURE question, at most
// once per TTL. Never call it from a path that refuses a launch.
func cachedTclaudeLayerHostAvailability() error {
	return cachedSandboxImplHostAvailability(
		&sandboxImplHostProbe, tclaudeLayerHostAvailability)
}

func cachedTclaudeLayerServerHostAvailability() error {
	return cachedSandboxImplHostAvailability(
		&sandboxImplServerHostProbe, tclaudeLayerServerHostAvailability)
}

func cachedSandboxImplHostAvailability(
	cache *sandboxImplHostProbeCache,
	probe func() error,
) error {
	now := sandboxImplHostProbeNow()
	cache.Lock()
	if cache.valid && now.Sub(cache.checkedAt) < sandboxImplHostProbeTTL {
		err := cache.err
		cache.Unlock()
		return err
	}
	cache.Unlock()

	// Probe OUTSIDE the lock. The probe forks bwrap, and this runs on the
	// continuously-polled dashboard snapshot: holding the mutex across the exec
	// would let one slow probe queue every later snapshot request behind it, so
	// a wedged namespace setup would freeze the whole page rather than one
	// field. Two callers racing a stale cache may both probe; that is bounded
	// (the probe carries its own deadline) and far cheaper than the alternative.
	err := probe()

	cache.Lock()
	defer cache.Unlock()
	cache.err = err
	cache.checkedAt = now
	cache.valid = true
	return err
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
				Descr: "Runs the authoritative tool executor inside a tclaude-owned bubblewrap mount namespace " +
					"(the whole pane for interactive harnesses, or OpenCode's managed server). Linux only; " +
					"requires bwrap and unprivileged user namespaces.",
			},
			{
				Value:        string(sandboxpolicy.ImplementationStacked),
				Label:        "Stacked: tclaude + harness (experimental)",
				Experimental: true,
				Descr: "Runs the interactive harness inside tclaude's outer wall and requires a live " +
					"model-free round-trip through the harness's real nested OS sandbox. Linux Claude/Codex only.",
			},
		},
		Default: string(sandboxpolicy.ImplementationHarnessBuiltin),
		Stacked: map[string]dashboardStackedAvailability{},
	}
	if err := cachedTclaudeLayerHostAvailability(); err != nil {
		out.HostUnavailableReason = err.Error()
	} else {
		out.HostAvailable = true
	}
	if err := cachedTclaudeLayerServerHostAvailability(); err != nil {
		out.ServerHostUnavailableReason = err.Error()
	} else {
		out.ServerHostAvailable = true
	}
	for _, name := range harness.Names() {
		h, err := harness.ResolveSpawnable(name)
		if err != nil {
			continue
		}
		out.Stacked[name] = cachedStackedSandboxAvailability(h)
	}
	// Uncached on purpose: this is four stats of world-readable paths, not a
	// fork, and an operator who just unloaded the policy should see the hint
	// go away on the next poll rather than a TTL later.
	out.StackedAppArmorLikely = stackedAppArmorNestedBlockLikely()
	return out
}

func cachedStackedSandboxAvailability(h *harness.Harness) dashboardStackedAvailability {
	now := sandboxImplHostProbeNow()
	sandboxImplStackedProbeMu.Lock()
	cached, ok := sandboxImplStackedProbe[h.Name]
	if ok && now.Sub(cached.checkedAt) < sandboxImplHostProbeTTL {
		sandboxImplStackedProbeMu.Unlock()
		return cached.value
	}
	sandboxImplStackedProbeMu.Unlock()

	value := dashboardStackedAvailability{}
	executable, err := session.StackedSandboxAvailability(h)
	if err != nil {
		value.UnavailableReason = err.Error()
	} else {
		value.Available = true
		value.ExecutableIdentity = executable.Identity()
	}
	sandboxImplStackedProbeMu.Lock()
	sandboxImplStackedProbe[h.Name] = struct {
		checkedAt time.Time
		value     dashboardStackedAvailability
	}{checkedAt: now, value: value}
	sandboxImplStackedProbeMu.Unlock()
	return value
}

// resetSandboxImplHostProbeCache drops the disclosure cache. Test-only: a test
// that swaps the predicate must be able to observe the new answer rather than
// one cached before the swap.
func resetSandboxImplHostProbeCache() {
	for _, cache := range []*sandboxImplHostProbeCache{
		&sandboxImplHostProbe,
		&sandboxImplServerHostProbe,
	} {
		cache.Lock()
		cache.valid = false
		cache.err = nil
		cache.Unlock()
	}
	sandboxImplStackedProbeMu.Lock()
	sandboxImplStackedProbe = map[string]struct {
		checkedAt time.Time
		value     dashboardStackedAvailability
	}{}
	sandboxImplStackedProbeMu.Unlock()
}
