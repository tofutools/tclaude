package session

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/common/executil"
)

const (
	stackedSandboxProbeTimeout  = 20 * time.Second
	stackedProbeOuterSystemPath = "/usr/bin:/bin"
)

// ValidateStackedSandboxHarness is the descriptor capability gate. It runs
// before any host probing so a non-stackable harness (notably OpenCode) gets a
// stable named refusal on every platform.
func ValidateStackedSandboxHarness(h *harness.Harness) error {
	if h != nil && h.NestedSandbox != nil {
		return nil
	}
	name := "unknown"
	if h != nil {
		name = h.Name
	}
	return stackedSandboxRefusal("stacked_inner_harness_sandbox",
		fmt.Sprintf("harness %q has no reviewed nested OS-sandbox contract", name))
}

func stackedSandboxLaunchMode(h *harness.Harness) (string, string, error) {
	name := "unknown"
	if h != nil {
		name = h.Name
	}
	switch name {
	case harness.DefaultName:
		return harness.ClaudeSandboxOn, "", nil
	case harness.CodexName:
		return harness.SandboxManagedProfile, harness.CodexAgentProfile, nil
	default:
		return "", "", stackedSandboxRefusal(
			"stacked_inner_harness_sandbox",
			fmt.Sprintf("no reviewed inner sandbox forcing for harness %q", name),
		)
	}
}

// StackedSandboxAvailability performs the uncached engine identity check used
// by launch and disclosure surfaces. It is not the launch authority: a launch
// must additionally complete ProbeStackedSandbox inside its exact outer spec.
func StackedSandboxAvailability(h *harness.Harness) (harness.NestedSandboxExecutable, error) {
	if err := ValidateStackedSandboxHarness(h); err != nil {
		return harness.NestedSandboxExecutable{}, err
	}
	executable, err := h.NestedSandbox.ResolveExecutable(context.Background())
	if err != nil {
		capability, detail := harness.NestedSandboxCapability(
			err, h.NestedSandbox.CapabilityName())
		return harness.NestedSandboxExecutable{}, stackedSandboxRefusal(capability, detail)
	}
	return executable, nil
}

// ProbeStackedSandbox executes the real model-free harness engine inside the
// exact frozen outer launch spec. Success means both walls participated in an
// allowed/denied round-trip; a dependency check alone never reaches here.
func ProbeStackedSandbox(
	bwrapBinary string,
	spec TclaudeLayerLaunchSpec,
	h *harness.Harness,
	cwd string,
) (*StackedSandboxProof, error) {
	executable, err := StackedSandboxAvailability(h)
	if err != nil {
		return nil, err
	}
	proof, err := prepareStackedSandboxProof(h, executable)
	if err != nil {
		return nil, err
	}
	probe, err := h.NestedSandbox.PrepareProbe(cwd, proof.Executable)
	if err != nil {
		proof.Cleanup()
		capability, detail := harness.NestedSandboxCapability(
			err, h.NestedSandbox.CapabilityName())
		return nil, stackedSandboxRefusal(capability, detail)
	}
	if probe.Cleanup != nil {
		defer probe.Cleanup()
	}
	for _, warning := range stackedNamespaceWarnings(spec, probe.KnownPaths) {
		fmt.Fprintf(os.Stderr, "stacked sandbox namespace warning: %s\n", warning)
	}
	if err := PrepareTclaudeLayerHarnessState(spec); err != nil {
		proof.Cleanup()
		return nil, stackedSandboxRefusal("stacked_outer_launch_spec",
			fmt.Sprintf("prepare exact outer probe boundary: %v", err))
	}
	wrapped, err := WrapTclaudeLayerStackedSpec(
		bwrapBinary,
		spec,
		proof.ManifestPath,
		proof.ManifestSHA256,
		"",
		false,
		probe.Command,
	)
	if err != nil {
		proof.Cleanup()
		return nil, stackedSandboxRefusal("stacked_outer_launch_spec",
			fmt.Sprintf("render exact outer probe boundary: %v", err))
	}
	ctx, cancel := context.WithTimeout(context.Background(), stackedSandboxProbeTimeout)
	defer cancel()
	cmd := executil.CommandContextWithGrace(
		ctx,
		time.Second,
		"/bin/sh",
		"-c",
		wrapped,
	)
	cmd.Dir = cwd
	// The proof shell is a launch authority, not the interactive harness.
	// Never inherit credentials, shell startup controls, or exported Bash
	// functions that could spoof one of its fail-closed utility checks.
	cmd.Env = stackedProbeOuterEnvironment()
	output := &boundedProbeBuffer{limit: 16 * 1024}
	cmd.Stdout = output
	cmd.Stderr = output
	if err := cmd.Run(); err != nil {
		detail := fmt.Sprintf("%s round-trip failed: %v: %s",
			h.NestedSandbox.MechanismName(), err, strings.TrimSpace(output.String()))
		if ctx.Err() != nil {
			detail = fmt.Sprintf("%s round-trip exceeded %s: %s",
				h.NestedSandbox.MechanismName(), stackedSandboxProbeTimeout,
				strings.TrimSpace(output.String()))
		}
		detail += stackedAppArmorLikelyCause(output.String())
		proof.Cleanup()
		capability := h.NestedSandbox.CapabilityName()
		if probe.ClassifyFailure != nil {
			if classified := strings.TrimSpace(
				probe.ClassifyFailure(output.String()),
			); classified != "" {
				capability = classified
			}
		}
		return nil, stackedSandboxRefusal(capability, detail)
	}
	if h.Name == harness.DefaultName {
		if err := proof.completeProbe(); err != nil {
			proof.Cleanup()
			return nil, stackedSandboxRefusal(
				"stacked_claude_probe_helper",
				fmt.Sprintf("retire the sealed Go helper after the exact probe: %v", err),
			)
		}
	}
	return proof, nil
}

func stackedProbeOuterEnvironment() []string {
	return []string{"PATH=" + stackedProbeOuterSystemPath}
}

// StackedLaunchOSSandbox is recorded only after the real nested round-trip
// succeeds. Host-open retains fidelity caveats but still earns the successful
// stacked lock: two real walls are active.
func StackedLaunchOSSandbox(
	h *harness.Harness,
	posture sandboxpolicy.NetworkPosture,
) harness.LaunchOSSandbox {
	outer := "tclaude bwrap (host-open; ambient host Unix sockets reachable)"
	unverified := true
	if posture == sandboxpolicy.NetworkIsolatedWithAgentd {
		outer = "tclaude bwrap (isolated network/PIDs; constructed root)"
		unverified = false
	}
	mechanism := "unknown nested sandbox"
	if h != nil && h.NestedSandbox != nil {
		mechanism = h.NestedSandbox.MechanismName()
	}
	return harness.LaunchOSSandbox{
		State:      "on",
		Source:     "Stacked: " + outer + " + " + mechanism,
		Unverified: unverified,
	}
}

// stackedNestedNamespaceSignatures are the ways bwrap reports that it could not
// build the namespace a stacked launch's inner wall needs. They are matched
// against the probe's own output, never against a guess about the host.
var stackedNestedNamespaceSignatures = []string{
	"no permissions to create a new namespace",
	"creating new namespace failed",
	"setting up uid map",
	"unable to create new namespace",
}

// stackedAppArmorDocURL is the operator guide for the Ubuntu case, kept as one
// derived pair so a rename touches the path const alone.
const stackedAppArmorDocPath = "docs/sandboxing.md#stacked-refuses-on-apparmor-restricted-hosts"

var stackedAppArmorDocURL = "https://github.com/tofutools/tclaude/blob/main/" +
	stackedAppArmorDocPath

// likelyAppArmorNestedBwrap is the host half of the hint, indirected so a test
// can describe either host without owning the machine it runs on.
var likelyAppArmorNestedBwrap = LikelyAppArmorNestedBwrapBlock

// stackedAppArmorLikelyCause returns the one-line likely-cause pointer appended
// to a stacked refusal's detail, or "" when it would be an overclaim.
//
// Both halves must agree: the probe's OWN output must say the inner namespace
// could not be created, and the host must still carry Ubuntu's enforcing
// bwrap policy. The refusal, its capability names, and its literal shape are
// unchanged — this only tells the operator where the explanation lives, and
// says "likely" because nothing reachable from here can be certain.
func stackedAppArmorLikelyCause(output string) string {
	lowered := strings.ToLower(output)
	matched := false
	for _, signature := range stackedNestedNamespaceSignatures {
		if strings.Contains(lowered, signature) {
			matched = true
			break
		}
	}
	if !matched || !likelyAppArmorNestedBwrap() {
		return ""
	}
	return "; likely cause: Ubuntu's enforcing bwrap-userns-restrict AppArmor " +
		"policy denies capability sys_admin to bwrap's children, so the nested " +
		"bwrap cannot be built; workaround and its host-wide trade-off: " +
		stackedAppArmorDocURL
}

func stackedSandboxRefusal(capability, detail string) error {
	return fmt.Errorf(
		"stacked requested — refused: missing capability %s: %s; refusing rather than falling back to tclaude-layer or harness-builtin",
		capability, detail)
}

// StackedEngineBindingRefusal names the harness-specific failure to carry the
// probed engine bytes into the actual outer launch.
func StackedEngineBindingRefusal(h *harness.Harness, err error) error {
	name := "unknown"
	if h != nil {
		name = h.Name
	}
	return stackedSandboxRefusal("stacked_"+name+"_engine_binding", err.Error())
}

func stackedNamespaceWarnings(
	spec TclaudeLayerLaunchSpec,
	paths []string,
) []string {
	var warnings []string
	home, _ := os.UserHomeDir()
	for _, path := range paths {
		path = filepath.Clean(strings.TrimSpace(path))
		if path == "." || !filepath.IsAbs(path) {
			continue
		}
		// Constructed-root launches already carry the system runtime trees.
		system := false
		for _, root := range []string{"/usr", "/bin", "/sbin", "/lib", "/lib64", "/opt"} {
			if sandboxpolicy.PathContainsOrEqual(root, path) {
				system = true
				break
			}
		}
		if system {
			continue
		}
		access, covered := sandboxpolicy.EffectiveAccessAt(spec.Effective.Filesystem, path)
		if covered && access != sandboxpolicy.AccessDeny {
			continue
		}
		if home != "" && sandboxpolicy.PathContainsOrEqual(home, path) {
			warnings = append(warnings,
				fmt.Sprintf("nested engine path %q may be absent from the constructed outer namespace; policy is not widened automatically", path))
		}
	}
	return warnings
}

type boundedProbeBuffer struct {
	bytes.Buffer
	limit int
}

func (buffer *boundedProbeBuffer) Write(data []byte) (int, error) {
	original := len(data)
	if remaining := buffer.limit - buffer.Len(); remaining > 0 {
		if len(data) > remaining {
			data = data[:remaining]
		}
		_, _ = buffer.Buffer.Write(data)
	}
	return original, nil
}
