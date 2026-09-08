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
	actual, err := materialized.LaunchSelection()
	if err != nil || !actual.Equal(selected) {
		return SandboxChildArtifact{}, fmt.Errorf("sandbox materialization does not match selected policy")
	}
	policy := materialized.Composition.Values
	if policy.FilesystemRoot != model.SandboxRootSeparate {
		return SandboxChildArtifact{}, fmt.Errorf("inherited sandbox root preparation is not configured")
	}
	if len(policy.Tmpfs) != 0 || len(policy.AgentDirectories) != 0 || len(policy.PreLaunch) != 0 || policy.Resources != (model.SandboxResources{}) || policy.HarnessConfig != model.SandboxHarnessConfigDefault || policy.DarwinAllowMachRegister {
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
	rules := append(runtimeRules, policy.Filesystem...)
	bindings, err := p.config.Inspector.BindSandboxMounts(ctx, rules)
	if err != nil {
		return SandboxChildArtifact{}, err
	}
	defer func() { _ = bindings.Close() }()
	owned, err := p.config.Inspector.BindSandboxProviderResources(ctx, resources)
	if err != nil {
		return SandboxChildArtifact{}, err
	}
	bindings.pins = append(bindings.pins, owned.pins...)
	bindings.files = append(bindings.files, owned.files...)
	bindings.providerCount = len(owned.pins)

	// The launcher owns the inherited base; authored values are literal overlays.
	child.Env = MergeEnvironment(policy.Environment.Entries(), child.Env)
	child.ExactEnvironment = true
	// Validate the complete native command before creating retained host state.
	_, arguments, err := sandboxDescriptorInvocation(p.config.Wrapper, child, bindings, privateNetwork)
	if arguments != nil {
		_ = arguments.Close()
	}
	if err != nil {
		return SandboxChildArtifact{}, err
	}
	directory, err := os.MkdirTemp(p.config.Artifacts, "launch-")
	if err != nil {
		return SandboxChildArtifact{}, err
	}
	artifact, err := p.config.Inspector.PrepareSandboxChild(directory, p.config.Wrapper, child, bindings, privateNetwork)
	if err != nil {
		_ = os.RemoveAll(directory)
		return SandboxChildArtifact{}, err
	}
	return artifact, nil
}

func (p *SandboxLaunchPreparer) Invocation(artifact SandboxChildArtifact) (ProcessSpec, error) {
	return artifact.Invocation(p.config.Bootstrap)
}

// BootstrapExecutable identifies the shipped launcher for provider child modes
// that must remain inside the same confinement as the terminal they replace.
func (p *SandboxLaunchPreparer) BootstrapExecutable() string { return p.config.Bootstrap }
