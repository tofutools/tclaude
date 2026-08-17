package session

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// The harness-agnostic side of TCL-1201: every harness's tclaude-layer path
// requirements arrive as harness.LayerPathRequirement rows and pass through
// ONE fold into the launch-contract buckets. Harness-specific code answers
// what a launch needs; this file owns how those answers are validated,
// deduplicated and translated, so a socket or file can never reach a bucket
// that directory preparation later materializes.

// tclaudeLayerHarnessRequirements resolves the requirement catalog for one
// launch. It is a catalog dispatch, not a behavior branch: each arm only
// declares paths, and every declaration goes through the same fold.
//
// Claude and Codex declare nothing here today: their state root rides the
// shared StateRoot seam, their executables and runtime closures arrive as
// caller-resolved HarnessReadPaths, and Claude's Darwin scratch root keeps its
// hardened resolver (sandbox_harness_runtime_darwin.go) which establishes the
// directory itself before returning it as an ordinary write path.
func tclaudeLayerHarnessRequirements(
	input TclaudeLayerLaunchInput,
	cwd string,
	stateRoot string,
) ([]harness.LayerPathRequirement, error) {
	switch input.HarnessName {
	case harness.OpenCodeName:
		return openCodeLayerPathRequirements(input, stateRoot)
	case harness.CopilotName:
		baseline, err := copilotTclaudeLayerBaselineInput(input, cwd)
		if err != nil {
			return nil, err
		}
		return harness.CopilotLayerPathRequirements(baseline)
	default:
		return nil, nil
	}
}

// openCodeLayerPathRequirements declares the OpenCode launch requirements:
// the mutable XDG state directories (caller-supplied for an agentd-managed
// private allocation, host-derived otherwise), the read-only executable
// subtree of the legacy v2-compatible shape, and the agentd socket floor the
// executor's tool subprocesses coordinate through.
func openCodeLayerPathRequirements(
	input TclaudeLayerLaunchInput,
	stateRoot string,
) ([]harness.LayerPathRequirement, error) {
	var out []harness.LayerPathRequirement
	stateDirs := input.StateDirs
	source := "launch input"
	if len(stateDirs) == 0 {
		derived, err := tclaudeLayerOpenCodeStateDirs()
		if err != nil {
			return nil, err
		}
		stateDirs = derived
		source = "host XDG environment"
	}
	for _, path := range stateDirs {
		out = append(out, harness.LayerPathRequirement{
			Path:      path,
			Kind:      harness.LayerPathDirectory,
			Access:    harness.LayerPathWrite,
			MayCreate: true,
			Source:    source,
		})
	}
	if len(input.ReadOnlyBinds) == 0 {
		// Legacy v2-compatible shape: ~/.opencode is mutable harness state,
		// while its executable subtree is reopened read-only.
		binState, err := canonicalTclaudeLayerStatePath(
			filepath.Join(stateRoot, "bin"))
		if err != nil {
			return nil, fmt.Errorf("resolve OpenCode executable state: %w", err)
		}
		out = append(out, harness.LayerPathRequirement{
			Path:      binState,
			Kind:      harness.LayerPathDirectory,
			Access:    harness.LayerPathRead,
			MayCreate: true,
			Source:    "OpenCode executable state",
		})
	}
	// The executor's tool subprocesses are the managed agent. Keep their
	// authenticated coordination path reachable even when /tmp or an authored
	// Home deny hides the socket's ancestors.
	for _, socket := range sandboxpolicy.AgentdSocketFloor() {
		if socket = CanonicalTclaudeLayerGeneratedPath(socket); socket != "" {
			out = append(out, harness.LayerPathRequirement{
				Path:   socket,
				Kind:   harness.LayerPathSocket,
				Access: harness.LayerPathRead,
				Source: "tclaude-agentd-socket",
			})
		}
	}
	return out, nil
}

// describeNonDirectoryMode names the node kind a refused state path resolved
// to, in the requirement vocabulary, so the error reads as "a socket" rather
// than a raw mode string.
func describeNonDirectoryMode(mode os.FileMode) string {
	switch {
	case mode&os.ModeSocket != 0:
		return string(harness.LayerPathSocket)
	case mode.IsRegular():
		return string(harness.LayerPathFile)
	default:
		return mode.Type().String()
	}
}

// tclaudeLayerRequirementBuckets is the fold's output: the launch-contract
// collections the rest of spec construction already consumes. It exists so
// the translation from typed requirements to contract rows happens in exactly
// one place, with the kind invariants enforced before any bucket is filled.
type tclaudeLayerRequirementBuckets struct {
	// StateDirs are writable harness-state directories preparation may
	// mkdir. The state root itself is excluded: it is inherently the first
	// writable contract path and repeating it would make phase-0
	// normalization and preparation disagree about its representation.
	StateDirs []string
	// ReadOnlyStateDirs are creatable directories below the state root that
	// the launch reopens read-only.
	ReadOnlyStateDirs []string
	// ContractWriteDirs feed the launch contract's phase-0 write set.
	ContractWriteDirs []string
	// LaunchReadDirs and LaunchWriteDirs compose into the launch policy
	// filesystem.
	LaunchReadDirs  []string
	LaunchWriteDirs []string
}

// foldTclaudeLayerRequirements translates harness requirements into the
// launch-contract buckets. It fails closed: an unknown kind, a non-directory
// marked creatable, a writable file, or a read-only state directory outside
// the state root each refuse the launch rather than land in a bucket whose
// preparation semantics do not match the resource.
func foldTclaudeLayerRequirements(
	stateRoot string,
	requirements []harness.LayerPathRequirement,
) (tclaudeLayerRequirementBuckets, error) {
	var out tclaudeLayerRequirementBuckets
	for _, requirement := range requirements {
		path := filepath.Clean(strings.TrimSpace(requirement.Path))
		if path == "." || !filepath.IsAbs(path) {
			return tclaudeLayerRequirementBuckets{}, fmt.Errorf(
				"tclaude-layer requirement %q (%s) is not an absolute path",
				requirement.Path, requirement.Source)
		}
		if requirement.Kind != harness.LayerPathDirectory && requirement.MayCreate {
			return tclaudeLayerRequirementBuckets{}, fmt.Errorf(
				"tclaude-layer requirement %q (%s) is a %s and cannot be materialized by launch preparation",
				path, requirement.Source, requirement.Kind)
		}
		switch {
		case requirement.Kind == harness.LayerPathDirectory &&
			requirement.Access == harness.LayerPathWrite && requirement.MayCreate:
			// Plain append, not dedup: OpenCode's v3 contract consumers read
			// StateDirs positionally (four XDG rows, config at index 2), and
			// collapsing two legitimately-colliding XDG roots into one row
			// would silently skip their fail-closed validation. Duplicates are
			// harmless to preparation, and the write-set dedup that matters
			// happens in phase-0 normalization.
			if path != stateRoot {
				out.StateDirs = append(out.StateDirs, path)
			}
			out.ContractWriteDirs = append(out.ContractWriteDirs, path)
			if requirement.PolicyGrant {
				out.LaunchWriteDirs = appendUniqueDir(out.LaunchWriteDirs, path)
			}
		case requirement.Kind == harness.LayerPathDirectory &&
			requirement.Access == harness.LayerPathRead && requirement.MayCreate:
			if path == stateRoot || !sandboxpolicy.PathContainsOrEqual(stateRoot, path) {
				return tclaudeLayerRequirementBuckets{}, fmt.Errorf(
					"tclaude-layer read-only state requirement %q (%s) is not below state root %q",
					path, requirement.Source, stateRoot)
			}
			out.ReadOnlyStateDirs = append(out.ReadOnlyStateDirs, path)
			out.LaunchReadDirs = append(out.LaunchReadDirs, path)
		case requirement.Kind == harness.LayerPathDirectory &&
			requirement.Access == harness.LayerPathRead,
			requirement.Kind == harness.LayerPathFile &&
				requirement.Access == harness.LayerPathRead,
			requirement.Kind == harness.LayerPathSocket &&
				requirement.Access == harness.LayerPathRead:
			out.LaunchReadDirs = append(out.LaunchReadDirs, path)
		case requirement.Kind == harness.LayerPathSocket &&
			requirement.Access == harness.LayerPathWrite:
			// Socket nodes need writable access for connect(2), but are live
			// endpoints rather than directories the launch may materialize.
			out.LaunchWriteDirs = appendUniqueDir(out.LaunchWriteDirs, path)
		default:
			return tclaudeLayerRequirementBuckets{}, fmt.Errorf(
				"tclaude-layer requirement %q (%s) has unsupported shape: kind %q, access %q, may-create %t",
				path, requirement.Source, requirement.Kind, requirement.Access,
				requirement.MayCreate)
		}
	}
	return out, nil
}
