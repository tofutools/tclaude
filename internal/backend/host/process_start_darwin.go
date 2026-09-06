//go:build darwin

package host

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

func processStartToken(pid int) (string, error) {
	out, err := exec.Command("ps", "-o", "lstart=", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		if exitErr := new(exec.ExitError); errors.As(err, &exitErr) && len(out) == 0 {
			return "", os.ErrNotExist
		}
		return "", err
	}
	token := strings.TrimSpace(string(out))
	if token == "" {
		return "", os.ErrNotExist
	}
	return token, nil
}
