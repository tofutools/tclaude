//go:build linux || darwin

package agentd

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/tofutools/tclaude/pkg/launcher"
	"github.com/tofutools/tclaude/pkg/testutil"
)

func TestRelaunchHelperArgv(t *testing.T) {
	for _, tc := range []struct {
		args   []string
		detach bool
		want   string
	}{
		{[]string{"tclaude", "agentd", "serve", "--dashboard-port=8123"}, true,
			"tclaude agentd relaunch detach /new/tclaude -- tclaude agentd serve --dashboard-port=8123"},
		{[]string{"/usr/bin/tclaude-agentd", "serve"}, false,
			"/usr/bin/tclaude-agentd relaunch wait /new/tclaude -- /usr/bin/tclaude-agentd serve"},
	} {
		got, ok := relaunchHelperArgv("/new/tclaude", tc.args, tc.detach)
		if !ok || strings.Join(got, " ") != tc.want {
			t.Errorf("relaunchHelperArgv(%q) = %q %v, want %q", tc.args, got, ok, tc.want)
		}
		// What the helper reads back is the original command line.
		mode, exe, argv, err := parseRelaunchArgs(got[len(got)-len(tc.args)-3:])
		if err != nil || exe != "/new/tclaude" || strings.Join(argv, " ") != strings.Join(tc.args, " ") || (mode == relaunchDetach) != tc.detach {
			t.Errorf("parseRelaunchArgs: %s %s %q %v", mode, exe, argv, err)
		}
	}
	if _, ok := relaunchHelperArgv("/x", []string{"tclaude", "agentd", "profile"}, false); ok {
		t.Error("a non-serve command line must not relaunch")
	}
	for _, bad := range [][]string{
		{"wait", "/x"},
		{"bogus", "/x", "--", "a"},
		{"wait", "rel/x", "--", "a"},
		{"wait", "/x", "a", "b"},
	} {
		if _, _, _, err := parseRelaunchArgs(bad); err == nil {
			t.Errorf("parseRelaunchArgs(%q) accepted", bad)
		}
	}
}

func shellPath(t *testing.T) string {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	return sh
}

func TestRelaunchWaitReturnsTheChildsExitStatus(t *testing.T) {
	sh := shellPath(t)
	if code := runRelaunch(relaunchWait, sh, []string{"sh", "-c", "exit 3"}); code != 3 {
		t.Fatalf("exit status = %d, want 3", code)
	}
}

func TestRelaunchWaitForwardsSignals(t *testing.T) {
	sh := shellPath(t)
	cmd := exec.Command(sh, "-c", `trap 'exit 7' TERM; echo ready; while :; do sleep 0.05; done`)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// The trap is set once the child says so; a signal before it would
	// kill sh outright.
	if line, _ := bufio.NewReader(out).ReadString('\n'); line != "ready\n" {
		t.Fatalf("child said %q", line)
	}
	sigs := make(chan os.Signal, 1)
	sigs <- syscall.SIGTERM
	code, sig, forwarded := relaunchWaitChild(cmd, sigs)
	if code != 7 || sig != 0 || forwarded != syscall.SIGTERM {
		t.Fatalf("code=%d sig=%v, want 7 from the forwarded SIGTERM", code, sig)
	}
}

func TestRelaunchWaitReportsAChildKilledBySignal(t *testing.T) {
	sh := shellPath(t)
	cmd := exec.Command(sh, "-c", "kill -KILL $$")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if code, sig, _ := relaunchWaitChild(cmd, make(chan os.Signal)); sig != syscall.SIGKILL {
		t.Fatalf("code=%d sig=%v, want SIGKILL", code, sig)
	}
}

func TestRelaunchDetachStartsTheChildInItsOwnSession(t *testing.T) {
	sh := shellPath(t)
	dir := testutil.CanonicalTempDir(t)
	marker := filepath.Join(dir, "started")
	// The child records whether it leads its own process group, as the
	// leader of a new session does (pgid == pid; ps has no portable sid).
	script := `pg=$(ps -o pgid= -p $$ | tr -d ' '); [ "$pg" = "$$" ] && echo own > "$1.tmp" || echo shared > "$1.tmp"; mv "$1.tmp" "$1"`
	if code := runRelaunch(relaunchDetach, sh, []string{"sh", "-c", script, "sh", marker}); code != 0 {
		t.Fatalf("detach exit status = %d", code)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		data, err := os.ReadFile(marker)
		if err == nil {
			if got := strings.TrimSpace(string(data)); got != "own" {
				t.Fatalf("detached child session: %s", got)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("detached child never ran")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestRelaunchHelperProcess is the waiting helper when the test binary is
// re-run as one (TCLAUDE_TEST_RELAUNCH_HELPER set); a no-op otherwise.
func TestRelaunchHelperProcess(t *testing.T) {
	if os.Getenv("TCLAUDE_TEST_RELAUNCH_HELPER") == "" {
		return
	}
	sh, _ := exec.LookPath("sh")
	os.Exit(runRelaunch(relaunchWait, sh, []string{"sh", "-c", "kill -HUP $$; echo alive"}))
}

func TestRelaunchWaitKeepsSignalsIgnoredAtStart(t *testing.T) {
	sh := shellPath(t)
	// nohup's SIGHUP ignore must reach the relaunched daemon, as an exec in
	// place keeps it: the child signals itself and must survive.
	cmd := exec.Command(sh, "-c", `trap '' HUP; exec "$0" -test.run='^TestRelaunchHelperProcess$'`, os.Args[0])
	cmd.Env = append(os.Environ(), "TCLAUDE_TEST_RELAUNCH_HELPER=1")
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "alive") {
		t.Fatalf("relaunched child did not keep SIGHUP ignored: err=%v out=%q", err, out)
	}
}

func TestRelaunchWaitStartsAgentdAgainOnHandBack(t *testing.T) {
	sh := shellPath(t)
	prevFresh := relaunchRestartEnviron
	t.Cleanup(func() { relaunchRestartEnviron = prevFresh })
	relaunchRestartEnviron = func(environ []string) []string { return environ }
	dir := testutil.CanonicalTempDir(t)
	runs := filepath.Join(dir, "runs")
	// The first agentd hands back (as relaunchHandBack makes it); the one
	// the helper starts in its place exits 5. Each records the helper PID
	// it was told and its parent.
	script := `echo "$` + relaunchHelperVar + ` $PPID" >> "$1"; [ "$(wc -l < "$1")" -ge 2 ] && exit 5; exit ` + strconv.Itoa(relaunchHandBackExit)
	if code := runRelaunch(relaunchWait, sh, []string{"sh", "-c", script, "sh", runs}); code != 5 {
		t.Fatalf("exit status = %d, want 5 from the agentd started after the hand-back", code)
	}
	data, err := os.ReadFile(runs)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	want := strconv.Itoa(os.Getpid()) + " " + strconv.Itoa(os.Getpid())
	if len(lines) != 2 || lines[0] != want || lines[1] != want {
		t.Fatalf("runs = %q, want two runs told the helper PID and parented by it (%q)", lines, want)
	}
}

// TestRelaunchHandBackHelperProcess is the waiting helper when the test
// binary is re-run as one (TCLAUDE_TEST_RELAUNCH_HANDBACK set): its agentd
// is sent SIGINT while it stops for a restart, ignores it, and hands back.
func TestRelaunchHandBackHelperProcess(t *testing.T) {
	if os.Getenv("TCLAUDE_TEST_RELAUNCH_HANDBACK") == "" {
		return
	}
	sh, _ := exec.LookPath("sh")
	script := `echo run >> "$1"; trap '' INT; kill -INT $PPID; sleep 0.5; exit ` + strconv.Itoa(relaunchHandBackExit)
	os.Exit(runRelaunch(relaunchWait, sh, []string{"sh", "-c", script, "sh", os.Getenv("TCLAUDE_TEST_RELAUNCH_HANDBACK")}))
}

func TestRelaunchWaitStopsOnASignalDuringTheHandBack(t *testing.T) {
	runs := filepath.Join(testutil.CanonicalTempDir(t), "runs")
	cmd := exec.Command(os.Args[0], "-test.run=^TestRelaunchHandBackHelperProcess$")
	cmd.Env = append(os.Environ(), "TCLAUDE_TEST_RELAUNCH_HANDBACK="+runs)
	err := cmd.Run()
	ws, _ := cmd.ProcessState.Sys().(syscall.WaitStatus)
	// Dying of the re-raised SIGINT, or 128+SIGINT if the exit wins.
	stoppedOnINT := ws.Signaled() && ws.Signal() == syscall.SIGINT || ws.ExitStatus() == 128+int(syscall.SIGINT)
	if err == nil || !stoppedOnINT {
		t.Fatalf("helper: err=%v status=%v, want it to stop on the SIGINT", err, cmd.ProcessState)
	}
	if data, _ := os.ReadFile(runs); strings.Count(string(data), "run") != 1 {
		t.Fatalf("agentd ran %d times, want once (no restart after Ctrl+C)", strings.Count(string(data), "run"))
	}
}

func TestRelaunchCommandTellsOnlyAWaitedForAgentdTheHelper(t *testing.T) {
	prev := launchEnviron
	t.Cleanup(func() { launchEnviron = prev })
	launchEnviron = []string{"HOME=/h"}
	helperVars := func(env []string) []string {
		var out []string
		for _, kv := range env {
			if strings.HasPrefix(kv, relaunchHelperVar+"=") {
				out = append(out, kv)
			}
		}
		return out
	}
	wait := relaunchCommand(relaunchWait, "/x", []string{"x"}, launchEnviron)
	if got, want := helperVars(wait.Env), []string{relaunchHelperVar + "=" + strconv.Itoa(os.Getpid())}; !slices.Equal(got, want) {
		t.Errorf("wait env helper vars = %q, want %q", got, want)
	}
	if got := helperVars(relaunchCommand(relaunchDetach, "/x", []string{"x"}, launchEnviron).Env); len(got) != 0 {
		t.Errorf("a detached agentd has nothing to hand back to, got %q", got)
	}
	if !slices.Equal(launchEnviron, []string{"HOME=/h"}) {
		t.Errorf("relaunchCommand changed launchEnviron: %q", launchEnviron)
	}
}

func TestRelaunchHandBackDecision(t *testing.T) {
	prevPID, prevParent := relaunchHelperPID, relaunchParentPID
	t.Cleanup(func() { relaunchHelperPID, relaunchParentPID = prevPID, prevParent })
	for _, tc := range []struct {
		name            string
		helper, parent  int
		path, startPath string
		want            bool
	}{
		{"started by a waiting helper", 864, 864, "/bin/tclaude", "/bin/tclaude", true},
		{"no helper (started from the shell)", 0, 5638, "/bin/tclaude", "/bin/tclaude", false},
		{"helper gone, reparented", 864, 1, "/bin/tclaude", "/bin/tclaude", false},
		{"variable inherited from elsewhere", 864, 4242, "/bin/tclaude", "/bin/tclaude", false},
		{"self-update moved the binary", 864, 864, "/new/tclaude", "/bin/tclaude", false},
	} {
		relaunchHelperPID = tc.helper
		relaunchParentPID = func() int { return tc.parent }
		if got := relaunchHandBack(tc.path, tc.startPath); got != tc.want {
			t.Errorf("%s: relaunchHandBack = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestRelaunchRestartHandsBackToItsHelper(t *testing.T) {
	prevPID, prevParent, prevExit := relaunchHelperPID, relaunchParentPID, restartExit
	updateRestartState.Lock()
	prevState := struct {
		requested       bool
		path, startPath string
		plan            restartPlan
	}{updateRestartState.requested, updateRestartState.path, updateRestartState.startPath, updateRestartState.plan}
	updateRestartState.requested, updateRestartState.path, updateRestartState.startPath = true, "/bin/tclaude", "/bin/tclaude"
	updateRestartState.plan = restartPlan{relaunch: true}
	updateRestartState.Unlock()
	t.Cleanup(func() {
		relaunchHelperPID, relaunchParentPID, restartExit = prevPID, prevParent, prevExit
		updateRestartState.Lock()
		updateRestartState.requested, updateRestartState.path, updateRestartState.startPath, updateRestartState.plan = prevState.requested, prevState.path, prevState.startPath, prevState.plan
		updateRestartState.Unlock()
	})
	relaunchHelperPID = 864
	relaunchParentPID = func() int { return 864 }
	exited, code := false, 0
	restartExit = func(c int) { exited, code = true, c }
	if err := performSelfUpdateRestart(); err != nil {
		t.Fatal(err)
	}
	if !exited || code != relaunchHandBackExit {
		t.Fatalf("exited=%v code=%d, want an exit with %d (hand back, no second helper)", exited, code, relaunchHandBackExit)
	}
}

// The waiting helper starts agentd again after a hand-back the way a fresh
// start would: with the login shell's environment as it is now, not the
// one the helper was started with. The first start carries a refresh
// failure the restart that started the helper handed it.
func TestRelaunchWaitStartsAgentdWithAFreshEnvironmentOnHandBack(t *testing.T) {
	sh := shellPath(t)
	runs := filepath.Join(testutil.CanonicalTempDir(t), "runs")
	prevEnviron, prevFresh := launchEnviron, relaunchRestartEnviron
	t.Cleanup(func() { launchEnviron, relaunchRestartEnviron = prevEnviron, prevFresh })
	launchEnviron = []string{"PATH=/old/bin:/usr/bin:/bin"}
	t.Setenv(launcher.LoginEnvErrorVar, "restarting: no answer")
	refreshed := 0
	relaunchRestartEnviron = func(environ []string) []string {
		refreshed++
		if !slices.Equal(environ, launchEnviron) {
			t.Errorf("refreshed from %q, want the helper's launch environment", environ)
		}
		return []string{"PATH=/fresh/bin:/usr/bin:/bin"}
	}
	script := `echo "$PATH ${` + launcher.LoginEnvErrorVar + `:-none}" >> "$1"; [ "$(wc -l < "$1")" -ge 2 ] && exit 5; exit ` + strconv.Itoa(relaunchHandBackExit)
	if code := runRelaunch(relaunchWait, sh, []string{"sh", "-c", script, "sh", runs}); code != 5 {
		t.Fatalf("exit status = %d, want 5", code)
	}
	data, err := os.ReadFile(runs)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/old/bin:/usr/bin:/bin restarting: no answer", "/fresh/bin:/usr/bin:/bin none"}
	if got := strings.Split(strings.TrimSpace(string(data)), "\n"); !slices.Equal(got, want) || refreshed != 1 {
		t.Fatalf("runs = %q (refreshed %d), want %q", got, refreshed, want)
	}
}
