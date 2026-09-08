package claude

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/tofutools/tclaude/internal/backend/host"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

func (p *prepared) prepareSandbox(ctx context.Context) error {
	p.command = host.ProcessSpec{Executable: p.provider.executable, Args: p.argv(), Directory: p.request.Spec.WorkingDirectory, Env: p.runtimeEnvironment()}
	useHome := p.provider.nativeHomeExplicit || p.request.Spec.HostSandbox != nil
	if p.request.Intent == ports.StartContinue && len(p.request.PriorEvidence.Payload) != 0 {
		prior, err := decodeEvidence(p.request.PriorEvidence)
		if err != nil {
			return err
		}
		if prior.NativeHome != "" {
			if prior.NativeHome != p.provider.nativeHome {
				return fmt.Errorf("claude continuation configuration root changed")
			}
			useHome = true
		}
	}
	if useHome {
		if err := os.MkdirAll(p.provider.nativeHome, 0700); err != nil {
			return err
		}
		p.nativeHome = p.provider.nativeHome
		p.command.Env = append(p.command.Env, "CLAUDE_CONFIG_DIR="+p.nativeHome)
		recorded, err := decodeEvidence(p.describe.Evidence)
		if err != nil {
			return err
		}
		recorded.NativeHome = p.nativeHome
		p.describe.Evidence, err = encodeEvidence(recorded)
		if err != nil {
			return err
		}
	}
	if p.request.Spec.HostSandbox == nil {
		return nil
	}
	// This explicit root keeps credentials/history stable through recovery;
	// the backend's other private state never becomes a directory grant.
	p.command.Env = append([]string{"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=" + p.nativeHome, "TERM=xterm-256color"}, p.command.Env...)
	resources := []host.SandboxProviderResource{{Path: p.nativeHome, Access: model.SandboxFilesystemWrite}, {Path: p.spool.Directory(), Access: model.SandboxFilesystemWrite}}
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
	artifact, err := p.provider.hostSandbox.ForExecution(p.request.Spec).PrepareHarness(ctx, *p.request.Spec.HostSandbox, *p.request.HostSandboxPolicy, p.command, "claude", p.nativeHome, resources...)
	if err != nil {
		return err
	}
	p.artifact = &artifact
	p.command, err = p.provider.hostSandbox.Invocation(artifact)
	if err != nil {
		return err
	}
	recorded, err := decodeEvidence(p.describe.Evidence)
	if err != nil {
		return err
	}
	recorded.NativeHome = p.nativeHome
	recorded.HostSandbox = p.artifact
	recorded.HostSandboxPolicyHash = p.request.Spec.HostSandbox.PolicyHash
	p.describe.Evidence, err = encodeEvidence(recorded)
	p.describe.HostSandboxPolicyHash = recorded.HostSandboxPolicyHash
	return err
}
