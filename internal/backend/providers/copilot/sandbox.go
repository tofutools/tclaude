package copilot

import (
	"context"

	"github.com/tofutools/tclaude/internal/backend/host"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func (p *prepared) prepareSandbox(ctx context.Context) error {
	p.command = host.ProcessSpec{Executable: p.provider.executable, Args: p.argv(), Directory: p.request.Spec.WorkingDirectory, Env: p.runtimeEnvironment()}
	if p.request.Spec.HostSandbox == nil {
		return nil
	}
	// COPILOT_HOME already belongs to this provider. HOME also stays inside the
	// selected provider state rather than inheriting an unrelated daemon home.
	p.command.Env = append([]string{"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=" + p.stateRoot, "TERM=xterm-256color"}, p.command.Env...)
	resources := []host.SandboxProviderResource{{Path: p.stateRoot, Access: model.SandboxFilesystemWrite}, {Path: p.spool.Directory(), Access: model.SandboxFilesystemWrite}}
	if p.access != nil {
		endpoint, err := host.SandboxControlResource(p.provider.agentSocket, p.provider.agentSocketDirectory)
		if err != nil {
			return err
		}
		resources = append(resources, host.SandboxProviderResource{Path: p.access.Resource, Access: model.SandboxFilesystemRead}, endpoint)
	}
	artifact, err := p.provider.hostSandbox.PrepareHarness(ctx, *p.request.Spec.HostSandbox, *p.request.HostSandboxPolicy, p.command, "copilot", p.stateRoot, resources...)
	if err != nil {
		return err
	}
	p.artifact = &artifact
	p.command, err = p.provider.hostSandbox.Invocation(artifact)
	if err != nil {
		return err
	}
	recorded, err := decodeEvidence(p.description.Evidence)
	if err != nil {
		return err
	}
	recorded.HostSandbox = p.artifact
	recorded.HostSandboxPolicyHash = p.request.Spec.HostSandbox.PolicyHash
	p.description.Evidence, err = encodeEvidence(recorded)
	p.description.HostSandboxPolicyHash = recorded.HostSandboxPolicyHash
	return err
}
