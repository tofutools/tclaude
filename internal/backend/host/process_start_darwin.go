//go:build darwin

package host

import (
	"fmt"
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

func processStartToken(pid int) (string, error) {
	process, err := darwinProcess(pid)
	if err != nil {
		return "", err
	}
	started := process.Proc.P_starttime
	return strconv.FormatInt(started.Sec, 10) + ":" + strconv.FormatInt(int64(started.Usec), 10), nil
}

func processParentPID(pid int) (int, error) {
	process, err := darwinProcess(pid)
	if err != nil {
		return 0, err
	}
	return int(process.Eproc.Ppid), nil
}

func darwinProcess(pid int) (*unix.KinfoProc, error) {
	processes, err := unix.SysctlKinfoProcSlice("kern.proc.pid", pid)
	if err != nil {
		return nil, err
	}
	if len(processes) == 0 {
		return nil, os.ErrNotExist
	}
	if len(processes) != 1 || processes[0].Proc.P_pid != int32(pid) {
		return nil, fmt.Errorf("kernel returned %d mismatched records for pid %d", len(processes), pid)
	}
	return &processes[0], nil
}
