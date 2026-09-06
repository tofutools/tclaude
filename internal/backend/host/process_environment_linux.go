//go:build linux

package host

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
)

func processIDsWithEnvironment(key, value string) ([]int, error) {
	want := []byte(key + "=" + value)
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var matches []int
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 1 {
			continue
		}
		data, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "environ"))
		if err != nil {
			continue
		}
		for _, variable := range bytes.Split(data, []byte{0}) {
			if bytes.Equal(variable, want) {
				matches = append(matches, pid)
				break
			}
		}
	}
	return matches, nil
}
