//go:build darwin

package agentd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/opencodeapi"
)

func readCodexAppServerProcessIdentity(pid int, socketPath string) (string, error) {
	if pid <= 1 {
		return "", fmt.Errorf("invalid Codex app-server pid %d", pid)
	}
	// lstart supplies the OS process-incarnation identity, while command binds
	// that incarnation to app-server and this generation's unique socket path.
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "lstart=", "-o", "command=").Output()
	if err != nil {
		return "", err
	}
	command := string(out)
	if !strings.Contains(command, " codex-app-server-relay ") ||
		!strings.Contains(command, "--socket "+socketPath) {
		return "", fmt.Errorf("process %d argv does not target the recorded Codex app-server socket", pid)
	}
	digest := sha256.Sum256(append([]byte(strconv.Itoa(pid)+"\x00"), out...))
	return hex.EncodeToString(digest[:]), nil
}

func discoverCodexAppServerRelayPID(ctx context.Context, socketPath string) (int, error) {
	return waitForCodexAppServerPID(ctx, filepath.Join(filepath.Dir(socketPath), "server.pid.relay"))
}

func waitForCodexAppServerPID(ctx context.Context, path string) (int, error) {
	for {
		pid, err := readCodexAppServerPID(path)
		if err == nil {
			return pid, nil
		}
		if !os.IsNotExist(err) {
			return 0, err
		}
		select {
		case <-ctx.Done():
			return 0, fmt.Errorf("wait for Codex app-server pid: %w", ctx.Err())
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func readCodexAppServerPID(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 1 {
		return 0, fmt.Errorf("invalid Codex app-server pid file %s", path)
	}
	return pid, nil
}

func discoverCodexAppServerPID(ctx context.Context, _ string, pidFile string) (int, error) {
	return waitForCodexAppServerPID(ctx, pidFile)
}

func processOwnsCodexAppServerEndpoint(pid int, endpoint string) bool {
	if !opencodeapi.ProcessOwnsEndpoint(pid, endpoint) {
		return false
	}
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "command=").Output()
	if err != nil {
		return false
	}
	command := string(out)
	return strings.Contains(command, " app-server ") &&
		strings.Contains(command, "--listen "+endpoint)
}
