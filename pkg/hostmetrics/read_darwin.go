//go:build darwin

package hostmetrics

import (
	"context"
	"golang.org/x/sys/unix"
	"os"
	"os/exec"
	"time"
)

// Fixed executables and argv only. The API never supplies a command or path
// argument; subprocesses run on the sampler, never a status request.
func macCommand(name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	return cmd.Output()
}
func readLoad() ([3]float64, error) {
	b, err := macCommand("/usr/sbin/sysctl", "-n", "vm.loadavg")
	if err != nil {
		return [3]float64{}, err
	}
	return parseLoad(string(b))
}
func readMemory() (*Memory, error) {
	total, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return nil, err
	}
	b, err := macCommand("/usr/bin/vm_stat")
	if err != nil {
		return nil, err
	}
	return parseMacMemory(string(b), total)
}

func statfsBlockSize(s *unix.Statfs_t) uint64 { return uint64(s.Bsize) }
