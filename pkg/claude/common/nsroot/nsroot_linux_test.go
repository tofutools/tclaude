//go:build linux

package nsroot

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// requireEnv turns the environment skips into failures. CI sets it in the
// bubblewrap smoke job, so the live sandbox test cannot silently skip there.
const requireEnv = "TCLAUDE_REQUIRE_USERNS"

func skipOrFail(t *testing.T, format string, args ...any) {
	t.Helper()
	if os.Getenv(requireEnv) == "1" {
		t.Fatalf(requireEnv+"=1 but: "+format, args...)
	}
	t.Skipf(format, args...)
}

// subtree lists root and its descendants breadth-first.
func subtree(root int) []int {
	out := []int{root}
	for i := 0; i < len(out); i++ {
		tasks, _ := filepath.Glob("/proc/" + strconv.Itoa(out[i]) + "/task/*/children")
		for _, f := range tasks {
			b, _ := os.ReadFile(f)
			for _, s := range strings.Fields(string(b)) {
				if pid, err := strconv.Atoi(s); err == nil {
					out = append(out, pid)
				}
			}
		}
	}
	return out
}

// TestRootShowsTheSandboxsOwnViewAfterARename runs a bubblewrap sandbox that
// hides dir/site/secret under a tmpfs, then renames site to site2 from
// inside. The tmpfs moves with its parent, so the host path dir/site2/secret
// holds the secret while the sandbox's view of the same path is empty. The
// sandbox is detached (not this test's descendant), as an agent's sandbox is
// not the daemon's.
func TestRootShowsTheSandboxsOwnViewAfterARename(t *testing.T) {
	bwrap, err := exec.LookPath("bwrap")
	if err != nil {
		skipOrFail(t, "bwrap not available")
	}
	if out, err := exec.Command(bwrap, "--unshare-user", "--ro-bind", "/", "/", "true").CombinedOutput(); err != nil {
		skipOrFail(t, "bwrap cannot make a user namespace: %v %s", err, out)
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	for rel, body := range map[string]string{"site/secret/key.txt": "SECRET", "site/public/ok.txt": "public"} {
		p := filepath.Join(dir, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte(body), 0o644))
	}
	ready := filepath.Join(dir, "ready")
	inner := `mv "$1/site" "$1/site2" && : > "$1/ready" && exec sleep 60`
	launch := exec.Command("sh", "-c", `"$@" >/dev/null 2>&1 & echo $!`, "sh",
		bwrap, "--unshare-user", "--unshare-pid", "--ro-bind", "/", "/", "--dev", "/dev", "--proc", "/proc",
		"--bind", dir, dir, "--tmpfs", filepath.Join(dir, "site", "secret"),
		"sh", "-c", inner, "sh", dir)
	out, err := launch.Output()
	require.NoError(t, err)
	bwrapPID, err := strconv.Atoi(strings.TrimSpace(string(out)))
	require.NoError(t, err)
	t.Cleanup(func() {
		pids := subtree(bwrapPID)
		for i := len(pids) - 1; i >= 0; i-- {
			_ = syscall.Kill(pids[i], syscall.SIGKILL)
		}
	})
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			skipOrFail(t, "the sandbox did not start")
		}
		time.Sleep(50 * time.Millisecond)
	}
	host, err := os.ReadFile(filepath.Join(dir, "site2", "secret", "key.txt"))
	require.NoError(t, err)
	require.Equal(t, "SECRET", string(host), "on the host the renamed path holds the secret")

	ns, pids := SandboxCandidates(subtree(bwrapPID))
	require.NotEmpty(t, pids, "the sandbox runs in its own mount namespace")
	var root *Root
	for _, pid := range pids {
		if root, err = Open(pid, ns); err == nil {
			break
		}
	}
	require.NoError(t, err, "the daemon can open a sandbox it is not the parent of")
	defer root.Close()
	assert.True(t, root.Alive())

	secret, err := root.OpenDir(filepath.Join(dir, "site2", "secret"))
	require.NoError(t, err)
	_, err = unix.Openat(secret, "key.txt", unix.O_RDONLY|unix.O_CLOEXEC, 0)
	assert.ErrorIs(t, err, unix.ENOENT, "the tmpfs moved with the rename: the sandbox sees no secret")
	_ = unix.Close(secret)

	public, err := root.OpenDir(filepath.Join(dir, "site2", "public"))
	require.NoError(t, err)
	fd, err := unix.Openat(public, "ok.txt", unix.O_RDONLY|unix.O_CLOEXEC, 0)
	require.NoError(t, err)
	f := os.NewFile(uintptr(fd), "ok.txt")
	b, err := io.ReadAll(f)
	_ = f.Close()
	_ = unix.Close(public)
	require.NoError(t, err)
	assert.Equal(t, "public", string(b))

	_, err = root.OpenDir(filepath.Join(dir, "site"))
	assert.Error(t, err, "the old name is gone inside the sandbox")
}

func TestSandboxCandidatesTrustOnlyWhatBwrapRuns(t *testing.T) {
	// This test's own tree: not under a bwrap, not in another namespace.
	ns, pids := SandboxCandidates(subtree(os.Getpid()))
	assert.Empty(t, ns)
	assert.Empty(t, pids)
	comm, ppid, ok := procStat(os.Getpid())
	require.True(t, ok)
	assert.Equal(t, os.Getppid(), ppid)
	assert.NotEmpty(t, comm)
}

func TestOpenRefusesAProcessOutsideTheNamespace(t *testing.T) {
	self, err := MountNS(os.Getpid())
	require.NoError(t, err)
	_, err = Open(os.Getpid(), self+"-other")
	assert.Error(t, err)
	_, err = Open(1, self)
	assert.Error(t, err)
}
