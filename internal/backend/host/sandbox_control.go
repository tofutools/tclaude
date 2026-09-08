//go:build linux || darwin

package host

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
)

// SandboxControlFDArgument is replaced only as a complete argv entry after the
// owning bootstrap has bound the retained control listener, before native exec.
const SandboxControlFDArgument = "__TCLAUDE_SANDBOX_CONTROL_FD__"

func validateSandboxControlArguments(arguments []string, port int) error {
	count := 0
	for _, argument := range arguments {
		if argument == SandboxControlFDArgument {
			count++
		}
	}
	if port < 0 || port > 65535 || port == 0 && count != 0 || port != 0 && count != 1 {
		return fmt.Errorf("sandbox control requires one retained descriptor argument and a valid port")
	}
	return nil
}

func sandboxControlPath(artifact SandboxChildArtifact) string {
	return filepath.Join(filepath.Dir(artifact.Path), "control")
}

// createSandboxControl is called only after claiming the one-shot artifact.
// Its private directory is deliberately not part of the native mount set.
func createSandboxControl(artifact SandboxChildArtifact) (*os.File, error) {
	path := sandboxControlPath(artifact)
	if _, err := os.Lstat(path); err == nil || !os.IsNotExist(err) {
		return nil, fmt.Errorf("sandbox control socket already exists or cannot be inspected")
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	listener.SetUnlinkOnClose(false)
	defer func() { _ = listener.Close() }()
	if err := os.Chmod(path, 0600); err != nil {
		return nil, err
	}
	identity, err := InspectUnixControl(path)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(identity)
	if err != nil {
		return nil, err
	}
	if err := WriteProtectedFile(artifact.Path+".control.json", encoded); err != nil {
		return nil, err
	}
	return listener.File()
}

// ReadSandboxControl returns only the endpoint published by this immutable
// artifact's bootstrap. Process.DialUnixControl must additionally prove its
// live owner before the provider sends credentials or requests.
func ReadSandboxControl(artifact SandboxChildArtifact) (UnixControlIdentity, error) {
	input, _, err := readSandboxChild(artifact)
	if err != nil {
		return UnixControlIdentity{}, err
	}
	if input.ControlPort == 0 {
		return UnixControlIdentity{}, fmt.Errorf("sandbox artifact has no control endpoint")
	}
	data, err := ReadProtectedFile(artifact.Path+".control.json", 4096)
	if err != nil {
		return UnixControlIdentity{}, err
	}
	var identity UnixControlIdentity
	if err := json.Unmarshal(data, &identity); err != nil {
		return UnixControlIdentity{}, err
	}
	if identity.Path != sandboxControlPath(artifact) {
		return UnixControlIdentity{}, fmt.Errorf("sandbox control receipt names a different endpoint")
	}
	observed, err := InspectUnixControl(identity.Path)
	if err != nil || observed != identity {
		return UnixControlIdentity{}, fmt.Errorf("sandbox control receipt identity changed")
	}
	return identity, nil
}
