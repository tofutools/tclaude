package agentd

import (
	"runtime"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
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
	// Platform is the OS agentd is running on. The sandbox-profile editor uses
	// it as the default target for effective-policy evaluation; the dashboard
	// may be open in a remote browser whose own OS is different.
	Platform string `json:"platform"`
	// Options is the catalog in presentation order. Never null, so the page's
	// .map() is safe.
	Options []dashboardSandboxImplOption `json:"options"`
	// Default is the implementation a launch resolves to when nothing pins one.
	Default string `json:"default"`
	// HostAvailable reports whether the interactive tclaude-layer's TOOLING IS
	// INSTALLED — bubblewrap on PATH plus pidfd for the terminal relay (Linux),
	// or sandbox-exec (macOS). It is disclosure, never permission: the dialog
	// keeps an unavailable implementation selectable and warns, because the
	// launch-time refusal is the authority and a dialog that quietly removed
	// the option would have replaced it.
	//
	// It is deliberately NOT a promise, and the gap is wider than it looks: a
	// true here means the tool is present, not that it works. A host with bwrap
	// installed but unprivileged user namespaces disabled reports true and
	// still fails the launch. That is the accepted cost of keeping this off the
	// 2-second poll — verifying it means FORKING the sandbox engine, which is
	// what the launch does, posture-exact, where the answer decides something.
	// So a false here rules out an obviously-doomed launch; a true rules
	// nothing in.
	HostAvailable bool `json:"host_available"`
	// HostUnavailableReason names the concrete missing tool (no bwrap on PATH,
	// no pidfd, not Linux) so the operator can fix it rather than guess. "" when
	// HostAvailable is true.
	HostUnavailableReason string `json:"host_unavailable_reason,omitempty"`
	// ServerHostAvailable and ServerHostUnavailableReason are the same
	// installed-not-working disclosure for a relay-free, non-interactive server
	// boundary. Keeping the topology answers separate prevents a missing
	// terminal-relay capability from being presented as an OpenCode executor
	// refusal.
	ServerHostAvailable         bool   `json:"server_host_available"`
	ServerHostUnavailableReason string `json:"server_host_unavailable_reason,omitempty"`
	// Stacked is per-harness because each descriptor owns a different real
	// engine entry point, and carries only harnesses that HAVE a nested sandbox
	// — OpenCode owns none, so stacking is not a capability it can lack and it
	// is absent rather than reported unavailable. Same installed-not-working
	// semantics as the fields above: it says the engine is on PATH, not that it
	// sandboxes. Launch always re-resolves and completes the nested round-trip
	// inside the exact outer spec.
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
	StackedAppArmorLikely bool `json:"stacked_apparmor_nested_bwrap_likely,omitempty"`
}

// dashboardStackedAvailability discloses whether a harness's nested-sandbox
// engine is INSTALLED. It carried an ExecutableIdentity (path|version|size|
// mtime) until the poll stopped executing `<engine> --version` to obtain it —
// the field had no reader in the dashboard, so paying a process launch per
// minute to populate it bought nothing. The launch still freezes the exact
// identity it probes, where it is load-bearing.
type dashboardStackedAvailability struct {
	Available         bool   `json:"available"`
	UnavailableReason string `json:"unavailable_reason,omitempty"`
}

type dashboardSandboxImplOption struct {
	Value string `json:"value"`
	// Label and Descr may contain the literal "{harness}" placeholder, which the
	// renderer substitutes with the DISPLAY NAME of the harness currently
	// selected in the dialog ("Claude Code", "Codex CLI", "OpenCode"). The
	// catalog is host-wide while the harness selection changes client-side
	// without a refetch, so the substitution cannot happen here — but the copy
	// still lives in exactly one place, which is what keeps the option text from
	// drifting away from what the implementation does. The placeholder is the
	// only templating this copy has; nothing else is interpolated.
	//
	// Both fields are substituted by the dashboard's shared renderer, so a Descr
	// written with the placeholder is safe. Today only Label reaches the screen
	// (the dialogs render a bare <select>); any new surface that starts showing
	// Descr must go through that renderer rather than the raw catalog.
	Label string `json:"label"`
	Descr string `json:"descr"`
	// Experimental marks an implementation that is not yet a supported posture.
	// The label carries the word too; this flag lets the UI style it.
	Experimental bool `json:"experimental,omitempty"`
}

func buildSandboxImplCatalog() dashboardSandboxImpl {
	out := dashboardSandboxImpl{
		Platform: runtime.GOOS,
		Options: []dashboardSandboxImplOption{
			{
				Value: string(sandboxpolicy.ImplementationHarnessBuiltin),
				Label: "{harness} built-in",
				Descr: "Current behavior: {harness} owns OS-level containment, using whatever sandbox it provides.",
			},
			{
				Value:        string(sandboxpolicy.ImplementationTclaudeLayer),
				Label:        "tclaude built-in OS sandbox (experimental)",
				Experimental: true,
				Descr: "Runs the authoritative tool executor inside a tclaude-owned bubblewrap mount namespace " +
					"(the whole pane for interactive harnesses, or OpenCode's managed server). Linux only; " +
					"requires bwrap and unprivileged user namespaces.",
			},
			{
				Value:        string(sandboxpolicy.ImplementationStacked),
				Label:        "Stacked: tclaude + {harness} (experimental)",
				Experimental: true,
				Descr: "Runs {harness} inside tclaude's outer wall and requires a live " +
					"model-free round-trip through {harness}'s real nested OS sandbox. Linux Claude/Codex only.",
			},
			{
				Value: string(sandboxpolicy.ImplementationResourceOnly),
				Label: "Resource limits only",
				Descr: "No OS-level access confinement, but the launch runs in its own cgroup " +
					"carrying the profile's CPU and memory limits, so one runaway agent cannot " +
					"exhaust the host. Linux only; needs no bwrap or namespaces. Pair the limits " +
					"with {harness} built-in instead if you also want its access confinement.",
			},
			{
				Value: string(sandboxpolicy.ImplementationOff),
				Label: "Off",
				Descr: "Disables OS-level sandbox confinement for this launch.",
			},
		},
		Default: string(sandboxpolicy.ImplementationHarnessBuiltin),
		Stacked: map[string]dashboardStackedAvailability{},
	}
	if err := sandboxToolPresence(sandboxToolLayerHost, tclaudeLayerHostPresence); err != nil {
		out.HostUnavailableReason = err.Error()
	} else {
		out.HostAvailable = true
	}
	if err := sandboxToolPresence(
		sandboxToolLayerServerHost, tclaudeLayerServerHostPresence,
	); err != nil {
		out.ServerHostUnavailableReason = err.Error()
	} else {
		out.ServerHostAvailable = true
	}
	// Only harnesses that own a nested sandbox appear here. OpenCode has none,
	// so "stacked" is not a capability it can lack — it is simply not a member
	// of this map, which is what SupportsNestedSandbox already gates.
	for _, name := range harness.Names() {
		h, err := harness.ResolveSpawnable(name)
		if err != nil || !h.SupportsNestedSandbox() {
			continue
		}
		value := dashboardStackedAvailability{}
		if err := sandboxToolPresence(stackedEngineToolKey(h.Name), func() error {
			return stackedEnginePresence(h)
		}); err != nil {
			value.UnavailableReason = err.Error()
		} else {
			value.Available = true
		}
		out.Stacked[name] = value
	}
	// Uncached on purpose: this is four stats of world-readable paths, not a
	// fork, and an operator who just unloaded the policy should see the hint
	// go away on the next poll rather than a TTL later.
	out.StackedAppArmorLikely = stackedAppArmorNestedBlockLikely()
	return out
}
