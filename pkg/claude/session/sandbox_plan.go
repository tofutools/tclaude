package session

import (
	"fmt"
	"os"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// SandboxPlanDisposition describes what an applier would do with a recorded
// path at inspection time. Inspection never creates a missing path.
type SandboxPlanDisposition string

const (
	SandboxPlanPresent          SandboxPlanDisposition = "present"
	SandboxPlanMissingWouldSkip SandboxPlanDisposition = "missing-would-skip"
	SandboxPlanHidden           SandboxPlanDisposition = "hidden"
)

// SandboxPlanEntry is one operator-facing row in the four-class outer-layer
// contract. Class is semantic precedence, not bubblewrap/Seatbelt argv order.
type SandboxPlanEntry struct {
	Class       int                    `json:"class"`
	ClassName   string                 `json:"class_name"`
	Origin      string                 `json:"origin"`
	Mode        string                 `json:"mode"`
	Source      string                 `json:"source,omitempty"`
	Target      string                 `json:"target"`
	Disposition SandboxPlanDisposition `json:"disposition"`
}

// SandboxPlanUnavailableEntry preserves a recorded policy row whose
// launch-time disposition was not persisted. It deliberately has no
// Disposition field: inventing a fourth disposition or observing the path now
// would blur recorded facts with current prediction.
type SandboxPlanUnavailableEntry struct {
	Class     int    `json:"class"`
	ClassName string `json:"class_name"`
	Origin    string `json:"origin"`
	Mode      string `json:"mode"`
	Target    string `json:"target"`
	Reason    string `json:"reason"`
}

// SandboxPlanDescription is the stable, renderer-neutral dry-run surface.
// It intentionally contains no executable, argv, probe result, or opaque
// access-enforcement plan token.
type SandboxPlanDescription struct {
	Applicable         bool                          `json:"applicable"`
	Reason             string                        `json:"reason,omitempty"`
	Coverage           string                        `json:"coverage,omitempty"`
	Unavailable        []string                      `json:"unavailable,omitempty"`
	UnavailableEntries []SandboxPlanUnavailableEntry `json:"unavailable_entries,omitempty"`
	NetworkPosture     string                        `json:"network_posture,omitempty"`
	Entries            []SandboxPlanEntry            `json:"entries"`
	Aliases            []sandboxpolicy.MountAlias    `json:"aliases"`
}

// DescribeTclaudeLayerPlan describes the already-composed launch contract.
// This is an inspection seam only: it never calls a platform wrapper, renders
// argv, creates directories, or materializes Unix-socket selectors.
//
// profileEffective is the unfiltered resolved profile value. The launch
// builder deliberately drops missing positive binds before applying them;
// inspection retains those rows so it can report missing-would-skip.
func DescribeTclaudeLayerPlan(
	spec TclaudeLayerLaunchSpec,
	profileEffective sandboxpolicy.EffectiveProfile,
) (SandboxPlanDescription, error) {
	plan, err := sandboxpolicy.RenderMountPlan(profileEffective)
	if err != nil {
		return SandboxPlanDescription{}, err
	}
	composed, err := sandboxpolicy.RenderMountPlan(spec.Effective)
	if err != nil {
		return SandboxPlanDescription{}, err
	}
	plan.NetworkPosture = composed.NetworkPosture
	out := SandboxPlanDescription{
		Applicable:     true,
		Coverage:       "composed",
		NetworkPosture: networkPostureLabel(plan.NetworkPosture),
		Entries:        []SandboxPlanEntry{},
		Aliases:        append([]sandboxpolicy.MountAlias(nil), plan.Aliases...),
	}
	var observationErr error
	add := func(class int, className, origin, mode, source, target string) {
		disposition, err := sandboxPlanDisposition(mode, source)
		if err != nil {
			if observationErr == nil {
				observationErr = fmt.Errorf("inspect %s source %q: %w", origin, source, err)
			}
			return
		}
		out.Entries = append(out.Entries, SandboxPlanEntry{
			Class:       class,
			ClassName:   className,
			Origin:      origin,
			Mode:        mode,
			Source:      source,
			Target:      target,
			Disposition: disposition,
		})
	}

	phase0, err := tclaudeLayerPhase0WriteDirs(spec.Contract, spec.Effective)
	if err != nil {
		return SandboxPlanDescription{}, err
	}
	for _, path := range phase0 {
		add(1, "launch-contract", "required-write", "rw", path, path)
	}
	for _, path := range spec.Contract.ReadOnlyStateDirs {
		add(1, "launch-contract", "harness-state-readonly", "ro", path, path)
	}

	for _, entry := range plan.Entries {
		// Source is the host authority and Target is where it lands inside the
		// sandbox. They differ for a mount_path grant, and the row must show both
		// so a dry-run reader can tell a projected mount from a same-path one.
		add(2, "profile-plan", "effective-filesystem", mountModeLabel(entry.Mode),
			entry.SourcePath(), entry.Path)
	}

	protected, err := sandboxpolicy.ProtectedPaths()
	if err != nil {
		return SandboxPlanDescription{}, err
	}
	for _, path := range protected {
		add(3, "protected-baseline", "protected-root", "hide", "", path)
	}
	for _, entry := range spec.Contract.PrivateWriteDirs {
		add(3, "protected-baseline", "daemon-private-parent", "hide", "", entry.Parent)
		add(3, "protected-baseline", "daemon-private-current", "rw", entry.Current, entry.Current)
	}
	for _, path := range spec.Contract.FinalHideDirs {
		add(3, "protected-baseline", "daemon-final-hide", "hide", "", path)
	}
	for _, bind := range spec.Contract.ReadOnlyBinds {
		add(3, "protected-baseline", "daemon-final-readonly", "ro", bind.Source, bind.Target)
	}
	if spec.Contract.MaterializedUnixSocketPaths != nil {
		for _, path := range *spec.Contract.MaterializedUnixSocketPaths {
			add(3, "protected-baseline", "materialized-unix-socket", "ro", path, path)
		}
	}

	tmuxDir, err := clcommon.TclaudeTmuxSocketDir()
	if err != nil {
		return SandboxPlanDescription{}, fmt.Errorf("describe tmux host-control path: %w", err)
	}
	add(4, "host-control", "tmux-socket-directory", "hide", "", tmuxDir)
	if observationErr != nil {
		return SandboxPlanDescription{}, observationErr
	}
	return out, nil
}

// DescribeRecordedEffectivePlan reports only authority actually frozen in an
// older session row. tclaude did not persist the full outer launch contract,
// so reconstructing per-session attachment/OpenCode/Git rows would invent
// audit facts from mutable current state. The unavailable classes are explicit
// until a future launch format records that contract.
func DescribeRecordedEffectivePlan(
	effective sandboxpolicy.EffectiveProfile,
) (SandboxPlanDescription, error) {
	plan, err := sandboxpolicy.RenderMountPlan(effective)
	if err != nil {
		return SandboxPlanDescription{}, err
	}
	out := SandboxPlanDescription{
		Applicable: true,
		Coverage:   "recorded-effective-only",
		Unavailable: []string{
			"launch-contract: not recorded at launch — unavailable; use hypothetical mode with explicit --cwd and --for inputs",
			"daemon-final: not recorded at launch — unavailable; use hypothetical mode with explicit --cwd and --for inputs",
			"positive profile dispositions: not recorded at launch — unavailable; use hypothetical mode with explicit --cwd and --for inputs",
		},
		NetworkPosture: networkPostureLabel(plan.NetworkPosture),
		Entries:        []SandboxPlanEntry{},
		Aliases:        append([]sandboxpolicy.MountAlias(nil), plan.Aliases...),
	}
	var observationErr error
	add := func(class int, className, origin, mode, source, target string) {
		disposition, dispositionErr := sandboxPlanDisposition(mode, source)
		if dispositionErr != nil {
			if observationErr == nil {
				observationErr = fmt.Errorf(
					"inspect %s source %q: %w", origin, source, dispositionErr)
			}
			return
		}
		out.Entries = append(out.Entries, SandboxPlanEntry{
			Class: class, ClassName: className, Origin: origin, Mode: mode,
			Source: source, Target: target, Disposition: disposition,
		})
	}
	for _, entry := range plan.Entries {
		mode := mountModeLabel(entry.Mode)
		if entry.Mode != sandboxpolicy.MountHide {
			target := entry.Path
			if entry.IsRemapped() {
				target = fmt.Sprintf("%s (from %s)", entry.Path, entry.Source)
			}
			out.UnavailableEntries = append(out.UnavailableEntries,
				SandboxPlanUnavailableEntry{
					Class: 2, ClassName: "profile-plan",
					Origin: "recorded-effective-filesystem",
					Mode:   mode, Target: target,
					Reason: "launch-time presence was not recorded — disposition unavailable; use hypothetical mode",
				})
			continue
		}
		add(2, "profile-plan", "recorded-effective-filesystem",
			mode, entry.Path, entry.Path)
	}
	protected, err := sandboxpolicy.ProtectedPaths()
	if err != nil {
		return SandboxPlanDescription{}, err
	}
	for _, path := range protected {
		add(3, "protected-baseline", "protected-root", "hide", "", path)
	}
	tmuxDir, err := clcommon.TclaudeTmuxSocketDir()
	if err != nil {
		return SandboxPlanDescription{}, fmt.Errorf(
			"describe tmux host-control path: %w", err)
	}
	add(4, "host-control", "tmux-socket-directory", "hide", "", tmuxDir)
	if observationErr != nil {
		return SandboxPlanDescription{}, observationErr
	}
	return out, nil
}

func sandboxPlanDisposition(mode, source string) (SandboxPlanDisposition, error) {
	if mode == "hide" {
		return SandboxPlanHidden, nil
	}
	if source == "" {
		return SandboxPlanMissingWouldSkip, nil
	}
	if _, err := os.Stat(source); err == nil {
		return SandboxPlanPresent, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	return SandboxPlanMissingWouldSkip, nil
}

func mountModeLabel(mode sandboxpolicy.MountMode) string {
	switch mode {
	case sandboxpolicy.MountRO:
		return "ro"
	case sandboxpolicy.MountRW:
		return "rw"
	default:
		return "hide"
	}
}

func networkPostureLabel(posture sandboxpolicy.NetworkPosture) string {
	switch posture {
	case sandboxpolicy.NetworkIsolatedWithAgentd:
		return "isolated-with-agentd"
	case sandboxpolicy.NetworkFiltered:
		return "filtered"
	default:
		return "host-open"
	}
}
