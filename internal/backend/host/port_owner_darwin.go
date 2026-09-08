//go:build darwin

package host

import (
	"errors"
	"os/exec"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

func processTreeOwnsLoopbackPort(rootPID, port int) (bool, error) {
	lsof, err := exec.LookPath("lsof")
	if err != nil {
		return false, errors.New("lsof is required to prove loopback ownership on macOS")
	}
	pids, err := darwinProcessTreePIDs(rootPID)
	if err != nil {
		return false, err
	}
	rawPIDs := make([]string, len(pids))
	for index, pid := range pids {
		rawPIDs[index] = strconv.Itoa(pid)
	}
	out, err := exec.Command(lsof, "-nP", "-a", "-p", strings.Join(rawPIDs, ","),
		"-iTCP@127.0.0.1:"+strconv.Itoa(port), "-sTCP:LISTEN", "-Fp").Output()
	if err != nil {
		if _, ok := err.(*exec.ExitError); ok {
			return false, nil
		}
		return false, err
	}
	owned := make(map[string]bool, len(rawPIDs))
	for _, pid := range rawPIDs {
		owned[pid] = true
	}
	for _, line := range strings.Fields(string(out)) {
		if strings.HasPrefix(line, "p") && owned[strings.TrimPrefix(line, "p")] {
			return true, nil
		}
	}
	return false, nil
}

func darwinProcessTreePIDs(rootPID int) ([]int, error) {
	// /bin/ps is set-ID on macOS and cannot be executed in a Seatbelt
	// child. Read the same kernel process records as retained identity checks.
	processes, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, err
	}
	children := make(map[int][]int)
	for _, process := range processes {
		pid, parent := int(process.Proc.P_pid), int(process.Eproc.Ppid)
		if pid > 0 && parent >= 0 {
			children[parent] = append(children[parent], pid)
		}
	}
	result := []int{rootPID}
	seen := map[int]bool{rootPID: true}
	for index := 0; index < len(result); index++ {
		for _, child := range children[result[index]] {
			if child > 1 && !seen[child] {
				seen[child] = true
				result = append(result, child)
			}
		}
	}
	return result, nil
}
