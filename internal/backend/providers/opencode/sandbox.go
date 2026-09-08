package opencode

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"

	"github.com/tofutools/tclaude/internal/backend/host"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

func (p *prepared) prepareSandbox(ctx context.Context) error {
	port := p.listener.Addr().(*net.TCPAddr).Port
	p.command = host.ProcessSpec{Executable: p.provider.executable,
		Args:      []string{"serve", "--hostname", "127.0.0.1", "--port", strconv.Itoa(port), "--pure"},
		Directory: p.request.Spec.WorkingDirectory,
		Env: append(append(p.provider.runtimeEnvironment(p.stateRoot), p.request.Spec.Environment.Entries()...),
			"OPENCODE_SERVER_USERNAME="+serverUsername, "OPENCODE_SERVER_PASSWORD="+p.password,
			attemptMarkerKey+"="+p.attemptMark,
			"TCLAUDE_BACKEND_CREDENTIAL_FILE="+accessResource(p.access),
			"TCLAUDE_BACKEND_SOCKET="+agentSocket(p.access, p.provider.agentSocket)),
	}
	if p.request.Spec.HostSandbox == nil {
		return nil
	}
	if err := prepareSandboxStateDirectories(p.stateRoot); err != nil {
		return err
	}
	bootstrap := p.provider.hostSandbox.BootstrapExecutable()
	resources := []host.SandboxProviderResource{
		{Path: p.stateRoot, Access: model.SandboxFilesystemWrite},
		{Path: p.provider.executable, Access: model.SandboxFilesystemRead},
		{Path: bootstrap, Access: model.SandboxFilesystemRead},
	}
	relay := ServerRelayRequest{Target: p.listener.Addr().String(), Executable: p.command.Executable, Args: p.command.Args}
	if p.request.Intent == ports.StartFork {
		input := filepath.Join(p.stateRoot, ".tclaude-fork-input.json")
		raw, err := os.ReadFile(input)
		if err != nil {
			return err
		}
		digest := sha256.Sum256(raw)
		relay.Import = &ServerRelayImport{Path: input, Digest: hex.EncodeToString(digest[:]), NativeID: p.request.History.Native.Reference, WorkingDir: p.request.Spec.WorkingDirectory}
		if p.request.History.Point != nil && p.request.History.Point.Kind == model.HistoryPointBeforeMessage {
			relay.Import.BeforeMessage = p.request.History.Point.Token
		}
		resources = append(resources, host.SandboxProviderResource{Path: input, Access: model.SandboxFilesystemRead})
	}
	if p.access != nil {
		endpoint, err := host.SandboxControlResource(p.provider.agentSocket, p.provider.agentSocketDirectory)
		if err != nil {
			return err
		}
		resources = append(resources, host.SandboxProviderResource{Path: p.access.Resource, Access: model.SandboxFilesystemRead}, endpoint)
	}
	raw, err := json.Marshal(relay)
	if err != nil {
		return err
	}
	p.command.Executable = bootstrap
	p.command.Args = []string{ServerRelayCommand, string(raw), host.SandboxControlFDArgument}
	p.command.Env = append([]string{"PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin", "HOME=" + p.stateRoot, "TMPDIR=" + p.stateRoot}, p.command.Env...)
	artifact, err := p.provider.hostSandbox.PrepareControl(ctx, *p.request.Spec.HostSandbox, *p.request.HostSandboxPolicy, p.command, port, resources...)
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
	recorded.HostSandbox, recorded.HostSandboxPolicyHash = p.artifact, p.request.Spec.HostSandbox.PolicyHash
	p.description.HostSandboxPolicyHash = recorded.HostSandboxPolicyHash
	p.description.Evidence, err = encodeEvidence(recorded)
	return err
}

// Allocate the native XDG roots through the owned directory descriptor. Native
// startup need not traverse protected host ancestors to discover/create them.
func prepareSandboxStateDirectories(stateRoot string) error {
	root, err := os.OpenRoot(stateRoot)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	for _, base := range []string{"data", "config", "cache", "state"} {
		for _, name := range []string{base, filepath.Join(base, "opencode")} {
			if err := root.Mkdir(name, 0700); err != nil && !os.IsExist(err) {
				return err
			}
			info, err := root.Lstat(name)
			if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("OpenCode private XDG path is not an owned directory: %s", name)
			}
		}
	}
	return nil
}
