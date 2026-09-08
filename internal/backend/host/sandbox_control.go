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

type sandboxControlReceipt struct {
	UnixControlIdentity
	Owner ProcessIdentity
}

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
	owner, err := identifyProcess(os.Getpid())
	if err != nil {
		return nil, fmt.Errorf("retain sandbox control creator: %w", err)
	}
	encoded, err := json.Marshal(sandboxControlReceipt{UnixControlIdentity: identity, Owner: owner})
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
	receipt, err := readSandboxControlReceipt(artifact)
	return receipt.UnixControlIdentity, err
}

// RecoverSandboxControlProcess recovers the bootstrap that created the listener,
// not an inner child discovered by its environment. Bubblewrap deliberately has
// no native environment; the protected receipt retains its exact host identity.
// Missing or incomplete receipt evidence is unknown, never a reason to relaunch.
func RecoverSandboxControlProcess(artifact SandboxChildArtifact) (*Process, error) {
	receipt, err := readSandboxControlReceipt(artifact)
	if err != nil {
		return nil, err
	}
	if receipt.Owner.PID <= 1 || receipt.Owner.ProcessGroup <= 1 || receipt.Owner.StartToken == "" {
		return nil, fmt.Errorf("sandbox control creator identity is incomplete")
	}
	return RecoverProcess(receipt.Owner)
}

func readSandboxControlReceipt(artifact SandboxChildArtifact) (sandboxControlReceipt, error) {
	input, _, err := readSandboxChild(artifact)
	if err != nil {
		return sandboxControlReceipt{}, err
	}
	if input.ControlPort == 0 {
		return sandboxControlReceipt{}, fmt.Errorf("sandbox artifact has no control endpoint")
	}
	data, err := ReadProtectedFile(artifact.Path+".control.json", 4096)
	if err != nil {
		return sandboxControlReceipt{}, err
	}
	var receipt sandboxControlReceipt
	if err := json.Unmarshal(data, &receipt); err != nil {
		return sandboxControlReceipt{}, err
	}
	if receipt.Path != sandboxControlPath(artifact) {
		return sandboxControlReceipt{}, fmt.Errorf("sandbox control receipt names a different endpoint")
	}
	observed, err := InspectUnixControl(receipt.Path)
	if err != nil || observed != receipt.UnixControlIdentity {
		return sandboxControlReceipt{}, fmt.Errorf("sandbox control receipt identity changed")
	}
	return receipt, nil
}
