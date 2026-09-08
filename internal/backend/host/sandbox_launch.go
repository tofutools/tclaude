//go:build linux || darwin

package host

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/sandboxpolicy"
)

// SandboxLaunchConfig is supplied by composition, never by a public request.
// Wrapper and Bootstrap name trusted executables; Artifacts is private host state.
type SandboxLaunchConfig struct {
	Inspector *SandboxPathInspector
	Wrapper   string
	Bootstrap string
	Artifacts string
}

type SandboxLaunchPreparer struct{ config SandboxLaunchConfig }

func NewSandboxLaunchPreparer(config SandboxLaunchConfig) (*SandboxLaunchPreparer, error) {
	if config.Inspector == nil || !filepath.IsAbs(config.Wrapper) || !filepath.IsAbs(config.Bootstrap) || !filepath.IsAbs(config.Artifacts) {
		return nil, fmt.Errorf("sandbox launch requires explicit trusted host configuration")
	}
	for _, path := range []string{config.Wrapper, config.Bootstrap} {
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
			return nil, fmt.Errorf("sandbox executable is unavailable: %s", path)
		}
	}
	info, err := os.Stat(config.Artifacts)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("sandbox artifact root must be an existing private directory")
	}
	canonical, err := filepath.EvalSymlinks(config.Artifacts)
	if err != nil {
		return nil, err
	}
	protected := false
	for _, root := range config.Inspector.roots {
		relative, err := filepath.Rel(root.path, canonical)
		if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative) {
			protected = true
		}
	}
	if !protected {
		return nil, fmt.Errorf("sandbox artifacts must reside beneath a protected host root")
	}
	return &SandboxLaunchPreparer{config: config}, nil
}

// Prepare compiles an exact resolved policy into a retained child artifact.
// Additional policy engines must be implemented here before their options can
// launch; none of the authored axes may be silently dropped.
func (p *SandboxLaunchPreparer) Prepare(ctx context.Context, selected model.SandboxSelection, materialized sandboxpolicy.PolicyMaterialization, child ProcessSpec, resources ...SandboxProviderResource) (SandboxChildArtifact, error) {
	return p.prepare(ctx, selected, materialized, child, "", "", 0, resources...)
}

// PrepareHarness retains the shared configuration floor alongside a provider's
// writable native state. Harness and root come from trusted provider composition.
func (p *SandboxLaunchPreparer) PrepareHarness(ctx context.Context, selected model.SandboxSelection, materialized sandboxpolicy.PolicyMaterialization, child ProcessSpec, harness, nativeRoot string, resources ...SandboxProviderResource) (SandboxChildArtifact, error) {
	return p.prepare(ctx, selected, materialized, child, harness, nativeRoot, 0, resources...)
}

// PrepareControl retains one trusted local server endpoint independently of
// authored network access. The native command must consume its inherited socket.
func (p *SandboxLaunchPreparer) PrepareControl(ctx context.Context, selected model.SandboxSelection, materialized sandboxpolicy.PolicyMaterialization, child ProcessSpec, port int, resources ...SandboxProviderResource) (SandboxChildArtifact, error) {
	if port < 1 || port > 65535 {
		return SandboxChildArtifact{}, fmt.Errorf("invalid sandbox control port")
	}
	return p.prepare(ctx, selected, materialized, child, "", "", port, resources...)
}

func (p *SandboxLaunchPreparer) prepare(ctx context.Context, selected model.SandboxSelection, materialized sandboxpolicy.PolicyMaterialization, child ProcessSpec, harness, nativeRoot string, controlPort int, resources ...SandboxProviderResource) (SandboxChildArtifact, error) {
	actual, err := materialized.LaunchSelection()
	if err != nil || !actual.Equal(selected) {
		return SandboxChildArtifact{}, fmt.Errorf("sandbox materialization does not match selected policy")
	}
	if err := validateSandboxControlArguments(child.Args, controlPort); err != nil {
		return SandboxChildArtifact{}, err
	}
	policy := materialized.Composition.Values
	if policy.FilesystemRoot != model.SandboxRootSeparate {
		return SandboxChildArtifact{}, fmt.Errorf("inherited sandbox root preparation is not configured")
	}
	if len(policy.AgentDirectories) != 0 || policy.Resources != (model.SandboxResources{}) || (harness == "" && policy.HarnessConfig != model.SandboxHarnessConfigDefault) || policy.DarwinAllowMachRegister {
		return SandboxChildArtifact{}, fmt.Errorf("selected sandbox requires additional native policy preparation")
	}
	if len(materialized.Composition.SocketAll) != 0 {
		return SandboxChildArtifact{}, fmt.Errorf("selected sandbox requires Unix socket policy preparation")
	}
	privateNetwork := false
	for _, conjunct := range materialized.Composition.NetworkAll {
		network := conjunct.Policy
		if len(network.Allow) != 0 || len(network.Deny) != 0 {
			return SandboxChildArtifact{}, fmt.Errorf("selected sandbox requires filtered network preparation")
		}
		if network.Baseline == model.SandboxNetworkDeny {
			privateNetwork = true
		}
	}
	if materialized.Composition.NetworkNamespace == "private" && !privateNetwork {
		return SandboxChildArtifact{}, fmt.Errorf("selected sandbox requires a private network route")
	}
	runtimeRules, err := SandboxRuntimeRules()
	if err != nil {
		return SandboxChildArtifact{}, err
	}
	rules := runtimeRules
	var denied []model.SandboxFilesystemRule
	for _, rule := range policy.Filesystem {
		if rule.Access == model.SandboxFilesystemDeny {
			denied = append(denied, rule)
		} else {
			rules = append(rules, rule)
		}
	}
	bindings, err := p.config.Inspector.BindSandboxMounts(ctx, rules)
	if err != nil {
		return SandboxChildArtifact{}, err
	}
	defer func() { _ = bindings.Close() }()
	floorStart := len(resources)
	if harness != "" {
		// Admit provider declarations before creating any missing floor directories.
		checked, err := p.config.Inspector.BindSandboxProviderResources(ctx, resources)
		if err != nil {
			return SandboxChildArtifact{}, err
		}
		_ = checked.Close()
		floor, err := prepareSandboxHarnessFloor(harness, nativeRoot, policy, resources)
		if err != nil {
			return SandboxChildArtifact{}, err
		}
		resources = append(append([]SandboxProviderResource(nil), resources...), floor...)
	}
	owned, err := p.config.Inspector.BindSandboxProviderResources(ctx, resources)
	if err != nil {
		return SandboxChildArtifact{}, err
	}
	for _, pin := range owned.pins[floorStart:] {
		if pin.Source != pin.Guest {
			_ = owned.Close()
			return SandboxChildArtifact{}, fmt.Errorf("sandbox configuration floor changed during preparation")
		}
	}
	bindings.pins = append(bindings.pins, owned.pins...)
	bindings.files = append(bindings.files, owned.files...)
	bindings.providerCount = len(owned.pins)
	bindings.controlPort = controlPort
	bindings.overlays, err = p.config.Inspector.prepareSandboxOverlays(ctx, denied, policy.Tmpfs, bindings, child.Executable, child.Directory)
	if err != nil {
		return SandboxChildArtifact{}, err
	}

	directory, err := os.MkdirTemp(p.config.Artifacts, "launch-")
	if err != nil {
		return SandboxChildArtifact{}, err
	}
	retained := false
	defer func() {
		if !retained {
			_ = os.RemoveAll(directory)
		}
	}()
	// The launcher owns the inherited base; authored values are literal overlays.
	child.Env = MergeEnvironment(policy.Environment.Entries(), child.Env)
	child.ExactEnvironment = true
	child, err = sandboxSetupCommand(child, policy.PreLaunch)
	if err != nil {
		return SandboxChildArtifact{}, err
	}
	if len(policy.PreLaunch) != 0 {
		// Keep potentially large Bash source out of argv. Grant this exact
		// retained file read-only, never the private artifact directory.
		setupPath := filepath.Join(directory, "setup.bash")
		if err := os.WriteFile(setupPath, []byte(child.Args[4]), 0600); err != nil {
			return SandboxChildArtifact{}, err
		}
		setup, err := p.config.Inspector.BindSandboxProviderResources(ctx, []SandboxProviderResource{{Path: setupPath, Access: model.SandboxFilesystemRead}})
		if err != nil {
			return SandboxChildArtifact{}, err
		}
		bindings.pins = append(bindings.pins, setup.pins...)
		bindings.files = append(bindings.files, setup.files...)
		bindings.providerCount += len(setup.pins)
		child.Args = append([]string{"--noprofile", "--norc", "-p", setupPath}, child.Args[6:]...)
	}
	// Validate the complete native command before retaining the launch artifact.
	_, arguments, err := sandboxDescriptorInvocation(p.config.Wrapper, child, bindings, privateNetwork)
	if arguments != nil {
		_ = arguments.Close()
	}
	if err != nil {
		return SandboxChildArtifact{}, err
	}
	artifact, err := p.config.Inspector.PrepareSandboxChild(directory, p.config.Wrapper, child, bindings, privateNetwork)
	if err != nil {
		_ = os.RemoveAll(directory)
		return SandboxChildArtifact{}, err
	}
	retained = true
	return artifact, nil
}

func (p *SandboxLaunchPreparer) Invocation(artifact SandboxChildArtifact) (ProcessSpec, error) {
	return artifact.Invocation(p.config.Bootstrap)
}

// BootstrapExecutable identifies the shipped launcher for provider child modes
// that must remain inside the same confinement as the terminal they replace.
func (p *SandboxLaunchPreparer) BootstrapExecutable() string { return p.config.Bootstrap }
