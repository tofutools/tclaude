//go:build !windows

package nodeinfo

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/testutil"
)

func TestProbeStopsGrandchild(t *testing.T) {
	for _, cancelParent := range []bool{true, false} {
		name := "timeout"
		if cancelParent {
			name = "cancellation"
		}
		t.Run(name, func(t *testing.T) {
			dir := testutil.CanonicalTempDir(t)
			path := filepath.Join(dir, "launcher")
			// Both launchers wait on their children, like the npm Copilot loader.
			// The grandchild records readiness before blocking with stdout open.
			require.NoError(t, os.WriteFile(path, []byte(`#!/bin/sh
case "$1" in
grandchild)
  echo $$ > "$2/grandchild"
  exec /bin/sleep 60
  ;;
child)
  echo $$ > "$2/child"
  "$0" grandchild "$2" &
  wait
  ;;
*)
  "$0" child "$2" &
  wait
  ;;
esac
`), 0700))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			// Reap the fixture's surviving descendants even if the assertion fails
			// against a regressed implementation that only kills the launcher.
			t.Cleanup(func() {
				for _, name := range []string{"child", "grandchild"} {
					data, _ := os.ReadFile(filepath.Join(dir, name))
					if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
						_ = syscall.Kill(pid, syscall.SIGKILL)
					}
				}
			})
			done := make(chan string, 1)
			go func() { done <- output(ctx, path, "--version", dir) }()
			var pid int
			require.Eventually(t, func() bool {
				data, err := os.ReadFile(filepath.Join(dir, "grandchild"))
				if err != nil {
					return false
				}
				pid, err = strconv.Atoi(strings.TrimSpace(string(data)))
				return err == nil && pid > 0
			}, time.Second, 5*time.Millisecond, "grandchild must start before cancellation")
			if cancelParent {
				cancel()
			}
			select {
			case version := <-done:
				require.Empty(t, version)
			case <-time.After(3 * time.Second):
				t.Fatal("probe did not finish within its deadline")
			}
			// Orphan zombies may await the host's init reaper. They cannot run or
			// write files; distinguish those from a live orphan on Linux/macOS.
			require.Eventually(t, func() bool {
				out, err := exec.Command("/bin/ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
				state := strings.TrimSpace(string(out))
				return err != nil || state == "" || strings.HasPrefix(state, "Z")
			}, time.Second, 5*time.Millisecond, "probe grandchild is still running")
		})
	}
}
