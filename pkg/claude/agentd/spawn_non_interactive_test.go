package agentd

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

func TestRunNonInteractiveSpawnShell(t *testing.T) {
	p := spawnParams{Harness: harness.ShellName, Cwd: t.TempDir(),
		InitialMessage: "printf 'hello\\n'; printf 'warning\\n' >&2; exit 7",
		GroupContext:   "this is prose and must not enter the shell command"}
	got, fail := runNonInteractiveSpawn(context.Background(), p, 30)
	if fail != nil {
		t.Fatalf("run failed: %+v", fail)
	}
	if got.Stdout != "hello\n" || got.Stderr != "warning\n" || got.ExitCode != 7 {
		t.Fatalf("unexpected result: %+v", got)
	}
}

func TestRunNonInteractiveSpawnAppliesResourceLimit(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("resource cgroups require Linux")
	}
	previousPrepare, previousConfigure, previousRemove :=
		prepareNonInteractiveResourceCgroup, configureNonInteractiveResourceCgroup, removeNonInteractiveResourceCgroup
	t.Cleanup(func() {
		prepareNonInteractiveResourceCgroup = previousPrepare
		configureNonInteractiveResourceCgroup = previousConfigure
		removeNonInteractiveResourceCgroup = previousRemove
	})
	var prepared, configured, closed, removed, cleaned bool
	const cgroupDir = "/test/one-shot-cgroup"
	prepareNonInteractiveResourceCgroup = func(id string, limits sandboxpolicy.ResourceLimits) (string, func(), error) {
		if id == "" || limits.PIDs == nil || *limits.PIDs != 32 {
			t.Fatalf("unexpected cgroup request: id=%q limits=%+v", id, limits)
		}
		prepared = true
		return cgroupDir, func() { cleaned = true }, nil
	}
	configureNonInteractiveResourceCgroup = func(cmd *exec.Cmd, dir string) (func(), error) {
		if dir != cgroupDir || cmd == nil {
			t.Fatalf("unexpected cgroup placement: dir=%q cmd=%v", dir, cmd)
		}
		configured = true
		return func() { closed = true }, nil
	}
	removeNonInteractiveResourceCgroup = func(dir string) error {
		if dir != cgroupDir || !closed {
			t.Fatalf("cgroup removed before placement handle closed: dir=%q closed=%t", dir, closed)
		}
		removed = true
		return nil
	}
	pids := uint64(32)
	snapshot := sandboxpolicy.NewSnapshot(sandboxpolicy.EffectiveProfile{
		ResourceLimits: sandboxpolicy.ResourceLimits{PIDs: &pids},
	}, nil)
	p := spawnParams{Harness: harness.ShellName, Cwd: t.TempDir(),
		SandboxImplementation: "harness-builtin", EffectiveSandbox: &snapshot,
		InitialMessage: "printf 'limited\\n'"}
	got, fail := runNonInteractiveSpawn(context.Background(), p, 30)
	if fail != nil || got.ExitCode != 0 || got.Stdout != "limited\n" ||
		!prepared || !configured || !closed || !removed || !cleaned {
		t.Fatalf("result=%+v failure=%+v lifecycle=%t/%t/%t/%t/%t", got, fail,
			prepared, configured, closed, removed, cleaned)
	}
}

func TestRunNonInteractiveSpawnRefusesMissingResourceBoundary(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("resource cgroups require Linux")
	}
	previous := prepareNonInteractiveResourceCgroup
	t.Cleanup(func() { prepareNonInteractiveResourceCgroup = previous })
	prepareNonInteractiveResourceCgroup = func(string, sandboxpolicy.ResourceLimits) (string, func(), error) {
		return "", func() {}, errors.New("delegation unavailable")
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")
	pids := uint64(32)
	snapshot := sandboxpolicy.NewSnapshot(sandboxpolicy.EffectiveProfile{
		ResourceLimits: sandboxpolicy.ResourceLimits{PIDs: &pids},
	}, nil)
	p := spawnParams{Harness: harness.ShellName, Cwd: dir,
		SandboxImplementation: "harness-builtin", EffectiveSandbox: &snapshot,
		InitialMessage: "touch " + marker}
	_, fail := runNonInteractiveSpawn(context.Background(), p, 30)
	if fail == nil || fail.Kind != "resource_limit_init" || fail.Status != 502 {
		t.Fatalf("missing resource boundary was accepted: %+v", fail)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("child started without its resource boundary: %v", err)
	}
}

func TestRunNonInteractiveSpawnCancellationKillsResourceCgroup(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("resource cgroups require Linux")
	}
	previousPrepare, previousConfigure, previousRemove, previousKill :=
		prepareNonInteractiveResourceCgroup, configureNonInteractiveResourceCgroup,
		removeNonInteractiveResourceCgroup, killNonInteractiveResourceCgroupMembers
	t.Cleanup(func() {
		prepareNonInteractiveResourceCgroup = previousPrepare
		configureNonInteractiveResourceCgroup = previousConfigure
		removeNonInteractiveResourceCgroup = previousRemove
		killNonInteractiveResourceCgroupMembers = previousKill
	})
	const cgroupDir = "/test/one-shot-timeout"
	prepareNonInteractiveResourceCgroup = func(string, sandboxpolicy.ResourceLimits) (string, func(), error) {
		return cgroupDir, func() {}, nil
	}
	configureNonInteractiveResourceCgroup = func(*exec.Cmd, string) (func(), error) {
		return func() {}, nil
	}
	removeNonInteractiveResourceCgroup = func(string) error { return nil }
	killed := make(chan struct{}, 1)
	killNonInteractiveResourceCgroupMembers = func(dir string) error {
		if dir != cgroupDir {
			t.Errorf("unexpected cgroup kill: %q", dir)
		}
		killed <- struct{}{}
		return nil
	}
	pids := uint64(32)
	snapshot := sandboxpolicy.NewSnapshot(sandboxpolicy.EffectiveProfile{
		ResourceLimits: sandboxpolicy.ResourceLimits{PIDs: &pids},
	}, nil)
	p := spawnParams{Harness: harness.ShellName, Cwd: t.TempDir(),
		SandboxImplementation: "harness-builtin", EffectiveSandbox: &snapshot,
		InitialMessage: "sleep 5"}
	got, fail := runNonInteractiveSpawn(context.Background(), p, 1)
	if fail != nil || got.ExitCode != 124 {
		t.Fatalf("result=%+v failure=%+v", got, fail)
	}
	select {
	case <-killed:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout did not kill the prepared resource cgroup")
	}
}

func TestRunNonInteractiveSpawnBoundsEscapedOutputPipe(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("setsid smoke requires Linux")
	}
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("setsid unavailable")
	}
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	t.Cleanup(func() {
		raw, err := os.ReadFile(pidFile)
		if err != nil {
			return
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
		if err == nil {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	p := spawnParams{Harness: harness.ShellName, Cwd: dir,
		InitialMessage: "setsid sleep 10 & echo $! > " + pidFile}
	started := time.Now()
	got, fail := runNonInteractiveSpawn(context.Background(), p, 30)
	if fail != nil || got.ExitCode != 125 || time.Since(started) > 8*time.Second {
		t.Fatalf("escaped pipe holder delayed or failed run: result=%+v failure=%+v elapsed=%s",
			got, fail, time.Since(started))
	}
}

func TestRunNonInteractiveSpawnReapsBackgroundProcessGroup(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("process-state smoke requires Linux")
	}
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	t.Cleanup(func() {
		raw, err := os.ReadFile(pidFile)
		if err == nil {
			if pid, parseErr := strconv.Atoi(strings.TrimSpace(string(raw))); parseErr == nil {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
	p := spawnParams{Harness: harness.ShellName, Cwd: dir,
		InitialMessage: "sleep 10 & echo $! > " + pidFile}
	got, fail := runNonInteractiveSpawn(context.Background(), p, 30)
	if fail != nil || got.ExitCode != 125 {
		t.Fatalf("background pipe holder result=%+v failure=%+v", got, fail)
	}
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		state, readErr := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
		if os.IsNotExist(readErr) {
			return
		}
		if readErr == nil {
			parts := strings.SplitN(string(state), ")", 2)
			if len(parts) == 2 && strings.HasPrefix(strings.TrimSpace(parts[1]), "Z ") {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("background child remained running after one-shot return")
}

func TestRunNonInteractiveSpawnTimeout(t *testing.T) {
	p := spawnParams{Harness: harness.ShellName, Cwd: t.TempDir(), InitialMessage: "printf 'partial stderr\\n' >&2; sleep 5"}
	got, fail := runNonInteractiveSpawn(context.Background(), p, 1)
	if fail != nil {
		t.Fatalf("run failed: %+v", fail)
	}
	if got.ExitCode != 124 || !strings.Contains(got.Stderr, "timed out") || !strings.Contains(got.Stderr, "partial stderr") {
		t.Fatalf("unexpected result: %+v", got)
	}
}

func TestRunNonInteractiveSpawnRemovesWriteProofBeforeChildStarts(t *testing.T) {
	dir := t.TempDir()
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	const token = "one-shot-proof"
	marker := filepath.Join(real, dirWriteProofFilePrefix+token)
	if err := os.WriteFile(marker, []byte("proof"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := spawnParams{Harness: harness.ShellName, Cwd: real,
		InitialMessage:       "test ! -e " + marker + " && printf 'clean\\n'",
		CleanupDirWriteProof: true, DirWriteProofToken: token,
		DirWriteProofDirs: []string{real}}
	got, fail := runNonInteractiveSpawn(context.Background(), p, 30)
	if fail != nil || got.ExitCode != 0 || got.Stdout != "clean\n" {
		t.Fatalf("run result=%+v failure=%+v", got, fail)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("proof marker remains after run: %v", err)
	}
}

func TestRunNonInteractiveSpawnUsesHarnessInitialPrompt(t *testing.T) {
	bin := t.TempDir()
	path := filepath.Join(bin, "claude")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nfor arg do last=$arg; done\nprintf '%s' \"$last\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	p := spawnParams{Harness: harness.DefaultName, Cwd: t.TempDir(),
		InitialMessage: "task brief", GroupContext: "group guidance"}
	got, fail := runNonInteractiveSpawn(context.Background(), p, 30)
	if fail != nil {
		t.Fatalf("run failed: %+v", fail)
	}
	if got.ExitCode != 0 || got.Stdout != "group guidance\n\ntask brief" {
		t.Fatalf("unexpected result: %+v", got)
	}
}

func TestRunNonInteractiveSpawnClaudeLaunchSettings(t *testing.T) {
	bin := t.TempDir()
	path := filepath.Join(bin, "claude")
	const fake = "#!/bin/sh\nprintf '%s\\n' \"${CLAUDE_CODE_DISABLE_AUTO_MEMORY-unset}\"\nfor arg do case $arg in *crossSessionInbound*) printf 'peer-blocked\\n';; esac; done\n"
	if err := os.WriteFile(path, []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, tc := range []struct {
		autoMemory, peerMessaging bool
		want                      string
	}{
		{false, false, "1\npeer-blocked\n"},
		{true, true, "0\n"},
	} {
		p := spawnParams{Harness: harness.DefaultName, Cwd: t.TempDir(),
			InitialMessage: "task", AutoMemory: tc.autoMemory, PeerMessaging: tc.peerMessaging}
		got, fail := runNonInteractiveSpawn(context.Background(), p, 30)
		if fail != nil || got.ExitCode != 0 || got.Stdout != tc.want {
			t.Fatalf("settings autoMemory=%t peerMessaging=%t result=%+v failure=%+v", tc.autoMemory, tc.peerMessaging, got, fail)
		}
	}
}

func TestRunNonInteractiveSpawnShellTclaudeLayer(t *testing.T) {
	if err := session.TclaudeLayerServerHostAvailability(); err != nil {
		t.Skipf("tclaude layer unavailable: %v", err)
	}
	previous := runNonInteractiveLayerCommand
	runNonInteractiveLayerCommand = executeNonInteractiveCommand
	t.Cleanup(func() { runNonInteractiveLayerCommand = previous })
	t.Setenv("HOME", t.TempDir())
	snapshot := sandboxpolicy.NewSnapshot(sandboxpolicy.EffectiveProfile{}, nil)
	p := spawnParams{Harness: harness.ShellName, Cwd: t.TempDir(),
		InitialMessage: "printf 'confined\\n'", SandboxImplementation: "tclaude-layer",
		EffectiveSandbox: &snapshot}
	got, fail := runNonInteractiveSpawn(context.Background(), p, 30)
	if fail != nil {
		t.Fatalf("run failed: %+v", fail)
	}
	if got.ExitCode != 0 || got.Stdout != "confined\n" {
		t.Fatalf("unexpected result: %+v", got)
	}
}
