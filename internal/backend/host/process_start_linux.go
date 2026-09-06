//go:build linux

package host

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func processStartToken(pid int) (string, error) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return "", err
	}
	// comm is parenthesized and may contain spaces or parentheses. The fields
	// following its final ')' begin at stat field 3; starttime is field 22.
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return "", fmt.Errorf("malformed proc stat for pid %d", pid)
	}
	fields := strings.Fields(string(data[end+1:]))
	if len(fields) <= 19 {
		return "", fmt.Errorf("incomplete proc stat for pid %d", pid)
	}
	return fields[19], nil
}
