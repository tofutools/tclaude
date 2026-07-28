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

// SandboxPlanDescription is the stable, renderer-neutral dry-run surface.
// It intentionally contains no executable, argv, probe result, or opaque
// access-enforcement plan token.
type SandboxPlanDescription struct {
	Applicable     bool                       `json:"applicable"`
	Reason         string                     `json:"reason,omitempty"`
	NetworkPosture string                     `json:"network_posture,omitempty"`
	Entries        []SandboxPlanEntry         `json:"entries"`
	Aliases        []sandboxpolicy.MountAlias `json:"aliases"`
}

// DescribeTclaudeLayerPlan describes the already-composed launch contract.
// This is an inspection seam only: it never calls a platform wrapper, renders
// argv, creates directories, or materializes Unix-socket selectors.
func DescribeTclaudeLayerPlan(spec TclaudeLayerLaunchSpec) (SandboxPlanDescription, error) {
	plan, err := sandboxpolicy.RenderMountPlan(spec.Effective)
	if err != nil {
		return SandboxPlanDescription{}, err
	}
	out := SandboxPlanDescription{
		Applicable:     true,
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
		add(2, "profile-plan", "effective-filesystem", mountModeLabel(entry.Mode), entry.Path, entry.Path)
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
