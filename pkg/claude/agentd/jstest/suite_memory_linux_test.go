package jstest

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// guardSuiteMemory kills any suite process of the `node --test` run rooted at
// root whose resident memory passes limitKB, and reports what it killed.
//
// A suite that allocates without bound does not fail on its own: it takes the
// whole machine, and on a CI runner that ends the job with a runner shutdown
// and no word of which suite did it (tcl-ha2ux1). Killing the suite turns that
// into a named test failure; the rest of the run carries on.
func guardSuiteMemory(root int, limitKB int64) (stop func() []string) {
	var (
		mu     sync.Mutex
		killed []string
	)
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for {
			for _, pid := range childPIDs(root) {
				rss := residentKB(pid)
				if rss <= limitKB {
					continue
				}
				// Named before the kill: a killed child's cmdline reads empty
				// once it is a zombie, and not at all once reaped.
				suite := suiteOf(pid)
				if syscall.Kill(pid, syscall.SIGKILL) == nil {
					mu.Lock()
					killed = append(killed, fmt.Sprintf("%s (%d MB resident)", suite, rss/1024))
					mu.Unlock()
				}
			}
			select {
			case <-done:
				return
			case <-ticker.C:
			}
		}
	}()
	return func() []string {
		close(done)
		<-finished
		mu.Lock()
		defer mu.Unlock()
		return killed
	}
}

// childPIDs lists root's direct children: node --test runs each suite file in
// a child process of its own.
func childPIDs(root int) []int {
	stats, _ := filepath.Glob("/proc/[0-9]*/stat")
	var pids []int
	for _, path := range stats {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		// The command name is parenthesised and may hold spaces; the fields
		// after it are "state ppid ...".
		end := bytes.LastIndexByte(data, ')')
		if end < 0 {
			continue
		}
		fields := strings.Fields(string(data[end+1:]))
		if len(fields) < 2 || fields[1] != strconv.Itoa(root) {
			continue
		}
		if pid, err := strconv.Atoi(filepath.Base(filepath.Dir(path))); err == nil {
			pids = append(pids, pid)
		}
	}
	return pids
}

func residentKB(pid int) int64 {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		if rest, ok := strings.CutPrefix(line, "VmRSS:"); ok {
			kb, _ := strconv.ParseInt(strings.TrimSuffix(strings.TrimSpace(rest), " kB"), 10, 64)
			return kb
		}
	}
	return 0
}

// suiteOf names the suite file a child runs: its last argument.
func suiteOf(pid int) string {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return fmt.Sprintf("pid %d", pid)
	}
	trimmed := strings.TrimRight(string(data), "\x00")
	if trimmed == "" {
		return fmt.Sprintf("pid %d", pid)
	}
	args := strings.Split(trimmed, "\x00")
	return filepath.Base(args[len(args)-1])
}

func TestGuardSuiteMemoryKillsAndNamesARunawaySuite(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	// The shell stands in for node --test: its child is the "suite", named by
	// its last argument, and it allocates well past the limit.
	cmd := exec.Command("sh", "-c", `python3 -c 'import sys, time; b = bytearray(400 << 20); b[::4096] = b"x" * len(b[::4096]); time.sleep(30)' runaway.test.mjs; :`)
	require.NoError(t, cmd.Start())
	stop := guardSuiteMemory(cmd.Process.Pid, 100*1024)
	started := time.Now()
	require.NoError(t, cmd.Wait())
	killed := stop()
	assert.Less(t, time.Since(started), 20*time.Second, "the suite was killed, not left to finish")
	require.Len(t, killed, 1)
	assert.Contains(t, killed[0], "runaway.test.mjs")
}
