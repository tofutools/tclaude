//go:build darwin

package hubupdate

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var launchLabel = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,199}$`)

func DetectSupervisor(label string) (string, error) {
	if !launchLabel.MatchString(label) {
		return "", fmt.Errorf("launchd requires the host's --supervisor-label")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for _, domain := range []string{"gui/" + strconv.Itoa(os.Getuid()), "system"} {
		raw, err := exec.CommandContext(ctx, "launchctl", "print", domain+"/"+label).Output()
		if err != nil {
			continue
		}
		pid := false
		keepalive := false
		plist := ""
		for _, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(line)
			pid = pid || line == "pid = "+strconv.Itoa(os.Getpid())
			if strings.HasPrefix(line, "path = ") {
				plist = strings.Trim(strings.TrimPrefix(line, "path = "), "\"")
			}
			if strings.HasPrefix(line, "properties = ") {
				keepalive = strings.Contains(line, "keepalive")
			}
		}
		if pid && keepalive && strings.HasPrefix(plist, "/") {
			value, err := exec.CommandContext(ctx, "plutil", "-extract", "KeepAlive", "raw", "-o", "-", plist).Output()
			if err != nil || strings.TrimSpace(string(value)) != "true" {
				continue
			}
			return "launchd", nil
		}
	}
	return "", fmt.Errorf("launchd must supervise this hub guardian with KeepAlive enabled")
}
