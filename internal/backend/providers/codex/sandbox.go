package codex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/tofutools/tclaude/internal/backend/host"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

func (p *prepared) prepareSandbox(ctx context.Context) error {
	p.command = host.ProcessSpec{Executable: p.provider.executable, Args: p.argv(), Directory: p.request.Spec.WorkingDirectory, Env: p.runtimeEnvironment()}
	if p.request.Spec.HostSandbox == nil {
		return nil
	}
	// CODEX_HOME already belongs to this provider. HOME also stays inside the
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
	if p.callback != nil {
		resources = append(resources, host.SandboxProviderResource{Path: filepath.Dir(p.callback.CredentialPath()), Access: model.SandboxFilesystemRead})
		endpoint := p.callback.Endpoint()
		if p.access == nil || endpoint != p.provider.agentSocket {
			resources = append(resources, host.SandboxProviderResource{Path: endpoint, Access: model.SandboxFilesystemRead})
		}
	}
	if p.request.Intent == ports.StartFork {
		root := filepath.Join(p.provider.privateRoot, "fork-results")
		if err := os.MkdirAll(root, 0700); err != nil {
			return err
		}
		directory, err := os.MkdirTemp(root, "fork-")
		if err != nil {
			return err
		}
		p.forkReceipt = filepath.Join(directory, "result")
		launcher := p.provider.hostSandbox.BootstrapExecutable()
		input := ForkTerminalRequest{Executable: p.provider.executable, Fork: TurnForkRequest{StateRoot: p.stateRoot, WorkingDirectory: p.request.Spec.WorkingDirectory, ThreadID: p.nativeID, LastTurnID: p.request.History.Point.Token}, Args: p.argv(), Receipt: p.forkReceipt}
		raw, err := json.Marshal(input)
		if err != nil {
			return err
		}
		p.command.Executable, p.command.Args = launcher, []string{ForkTerminalCommand, string(raw)}
		resources = append(resources, host.SandboxProviderResource{Path: launcher, Access: model.SandboxFilesystemRead}, host.SandboxProviderResource{Path: directory, Access: model.SandboxFilesystemWrite})
		if p.normalizer != nil {
			p.normalizer.SetForkReceipt(p.forkReceipt)
		}
	}
	artifact, err := p.provider.hostSandbox.ForExecution(p.request.Spec).PrepareHarness(ctx, *p.request.Spec.HostSandbox, *p.request.HostSandboxPolicy, p.command, "codex", p.stateRoot, resources...)
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
	recorded.ForkReceipt = p.forkReceipt
	if p.forkReceipt != "" {
		recorded.NativeID = ""
	}
	recorded.HostSandbox = p.artifact
	recorded.HostSandboxPolicyHash = p.request.Spec.HostSandbox.PolicyHash
	p.description.Evidence, err = encodeEvidence(recorded)
	p.description.HostSandboxPolicyHash = recorded.HostSandboxPolicyHash
	return err
}
