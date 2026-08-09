//go:build darwin

package agentd

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
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
	if !strings.Contains(command, " app-server --listen ") ||
		!strings.Contains(command, "unix://"+socketPath) {
		return "", fmt.Errorf("process %d argv does not target the recorded Codex app-server socket", pid)
	}
	digest := sha256.Sum256(append([]byte(strconv.Itoa(pid)+"\x00"), out...))
	return hex.EncodeToString(digest[:]), nil
}
