//go:build darwin

package host

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Temporary platform diagnostic; remove once the required native journey passes.
func TestSandboxDescriptorRuntimeProbe(t *testing.T) {
	wrapper, err := exec.LookPath("sandbox-exec")
	require.NoError(t, err)
	inspector, err := NewSandboxPathInspector([]string{t.TempDir()})
	require.NoError(t, err)
	rules, err := SandboxRuntimeRules()
	require.NoError(t, err)
	bound, err := inspector.BindSandboxMounts(context.Background(), rules)
	require.NoError(t, err)
	defer func() { _ = bound.Close() }()
	invocation, _, err := sandboxDescriptorInvocation(wrapper, ProcessSpec{Executable: "/usr/bin/true", Directory: "/", ExactEnvironment: true}, bound, true)
	require.NoError(t, err)
	profileIndex := -1
	for i, arg := range invocation.Args {
		if arg == "-p" {
			profileIndex = i + 1
			break
		}
	}
	require.Positive(t, profileIndex)
	original := invocation.Args[profileIndex]
	for _, variant := range []string{"baseline", "full", "read", "write", "network", "read-data", "read-metadata", "read-xattr", "root-open"} {
		lines := []string{}
		for _, line := range strings.Split(original, "\n") {
			if strings.HasPrefix(line, "(deny ") {
				switch variant {
				case "baseline":
					continue
				case "read", "write", "network":
					if !strings.Contains(line, "(deny file-"+variant) && !(variant == "network" && strings.Contains(line, "(deny network-")) {
						continue
					}
				case "read-data", "read-metadata", "read-xattr":
					if !strings.Contains(line, "(deny file-read*") {
						continue
					}
					line = strings.Replace(line, "file-read*", "file-"+variant, 1)
				case "root-open":
					if line == `(deny file-read-data (literal "/"))` {
						continue
					}
				}
			}
			lines = append(lines, line)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		args := append([]string(nil), invocation.Args...)
		args[profileIndex] = strings.Join(lines, "\n")
		command := exec.CommandContext(ctx, wrapper, args...)
		command.Env = []string{}
		output, err := command.CombinedOutput()
		cancel()
		t.Logf("runtime probe %s: error=%v output=%s", variant, err, output)
		if variant == "baseline" {
			require.NoError(t, err)
		}
	}
}
