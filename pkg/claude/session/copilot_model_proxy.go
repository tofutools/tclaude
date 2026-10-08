package session

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"time"
)

var copilotModelProxyVersion = regexp.MustCompile(`GitHub Copilot CLI ([0-9]+)\.([0-9]+)\.([0-9]+)\.`)

// Refuse unverified old binaries: ignoring OFFLINE or WIRE_API would permit
// GitHub routing or select an unsupported wire format despite a chosen gateway.
func validateCopilotModelProxyVersion(binary string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, binary, "--version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("cannot verify Copilot gateway binding version; launch refused")
	}
	version := copilotModelProxyVersion.FindStringSubmatch(string(output))
	if len(version) != 4 {
		return fmt.Errorf("unrecognized Copilot version; model gateway requires Copilot CLI 1.0.91 or newer")
	}
	major, _ := strconv.Atoi(version[1])
	minor, _ := strconv.Atoi(version[2])
	patch, _ := strconv.Atoi(version[3])
	if major < 1 || major == 1 && minor == 0 && patch < 91 {
		return fmt.Errorf("model gateway requires Copilot CLI 1.0.91 or newer for HTTP Responses and offline BYOK")
	}
	return nil
}
