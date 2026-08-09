//go:build linux

package agentd

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func readCodexAppServerProcessIdentity(pid int, socketPath string) (string, error) {
	if pid <= 1 {
		return "", errorsNewCodexProcessIdentity("invalid pid")
	}
	procDir := filepath.Join("/proc", strconv.Itoa(pid))
	cmdline, err := os.ReadFile(filepath.Join(procDir, "cmdline"))
	if err != nil {
		return "", err
	}
	args := bytes.Split(bytes.TrimSuffix(cmdline, []byte{0}), []byte{0})
	if !codexAppServerArgsTargetSocket(args, socketPath) {
		return "", fmt.Errorf("process %d argv does not target the recorded Codex app-server socket", pid)
	}
	stat, err := os.ReadFile(filepath.Join(procDir, "stat"))
	if err != nil {
		return "", err
	}
	// comm is parenthesized and may contain spaces or ')'; the last closing
	// parenthesis is the only safe boundary before the fixed-position fields.
	close := bytes.LastIndex(stat, []byte(") "))
	if close < 0 {
		return "", errorsNewCodexProcessIdentity("malformed /proc stat")
	}
	fields := strings.Fields(string(stat[close+2:]))
	// fields begins at field 3 (state); process starttime is field 22.
	if len(fields) <= 19 {
		return "", errorsNewCodexProcessIdentity("short /proc stat")
	}
	bootID, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(bytes.Join([][]byte{
		[]byte(strconv.Itoa(pid)), bytes.TrimSpace(bootID), []byte(fields[19]), cmdline,
	}, []byte{0}))
	return hex.EncodeToString(digest[:]), nil
}

func codexAppServerArgsTargetSocket(args [][]byte, socketPath string) bool {
	want := "unix://" + socketPath
	for i := 0; i+2 < len(args); i++ {
		if string(args[i]) == "app-server" && string(args[i+1]) == "--listen" && string(args[i+2]) == want {
			return true
		}
	}
	return false
}

func errorsNewCodexProcessIdentity(detail string) error {
	return fmt.Errorf("codex app-server process identity: %s", detail)
}
