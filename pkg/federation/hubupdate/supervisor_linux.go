//go:build linux

package hubupdate

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

func DetectSupervisor(_ string) (string, error) {
	raw, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", err
	}
	unit := ""
	user := false
	for _, line := range strings.Split(string(raw), "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			continue
		}
		path := parts[2]
		segments := strings.Split(path, "/")
		if len(segments) > 0 && strings.HasSuffix(segments[len(segments)-1], ".service") {
			unit = segments[len(segments)-1]
			user = strings.Contains(path, "/user.slice/") && !strings.HasPrefix(unit, "user@")
			break
		}
	}
	if unit == "" {
		return "", fmt.Errorf("hub is not the main process of a systemd service")
	}
	args := []string{"show", unit, "--property=MainPID,Restart,ActiveState"}
	if user {
		args = append([]string{"--user"}, args...)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	raw, err = exec.CommandContext(ctx, "systemctl", args...).Output()
	if err != nil {
		return "", fmt.Errorf("cannot verify systemd service restart policy")
	}
	values := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok {
			values[key] = value
		}
	}
	if values["MainPID"] != strconv.Itoa(os.Getpid()) || values["ActiveState"] != "active" || values["Restart"] != "always" && values["Restart"] != "on-failure" {
		return "", fmt.Errorf("systemd must supervise the hub guardian as MainPID with a restart policy")
	}
	return "systemd", nil
}
