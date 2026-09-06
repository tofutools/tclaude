//go:build darwin

package host

import (
	"errors"
	"os/exec"
	"strconv"
	"strings"
)

func processTreeOwnsLoopbackPort(rootPID, port int) (bool, error) {
	lsof, err := exec.LookPath("lsof")
	if err != nil {
		return false, errors.New("lsof is required to prove loopback ownership on macOS")
	}
	out, err := exec.Command(lsof, "-nP", "-a", "-p", strconv.Itoa(rootPID),
		"-iTCP@127.0.0.1:"+strconv.Itoa(port), "-sTCP:LISTEN", "-Fp").Output()
	if err != nil {
		if _, ok := err.(*exec.ExitError); ok {
			return false, nil
		}
		return false, err
	}
	return strings.Contains(string(out), "p"+strconv.Itoa(rootPID)), nil
}
