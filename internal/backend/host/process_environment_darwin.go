//go:build darwin

package host

import (
	"os/exec"
	"strconv"
	"strings"
)

func processIDsWithEnvironment(key, value string) ([]int, error) {
	out, err := exec.Command("ps", "eww", "-axo", "pid=,command=").Output()
	if err != nil {
		return nil, err
	}
	want := key + "=" + value
	var matches []int
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || !containsField(fields[1:], want) {
			continue
		}
		pid, parseErr := strconv.Atoi(fields[0])
		if parseErr == nil && pid > 1 {
			matches = append(matches, pid)
		}
	}
	return matches, nil
}

func containsField(fields []string, want string) bool {
	for _, field := range fields {
		if field == want {
			return true
		}
	}
	return false
}
