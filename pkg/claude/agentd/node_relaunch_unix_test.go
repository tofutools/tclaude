//go:build linux || darwin

package agentd

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

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
	code, sig := relaunchWaitChild(cmd, sigs)
	if code != 7 || sig != 0 {
		t.Fatalf("code=%d sig=%v, want 7 from the forwarded SIGTERM", code, sig)
	}
}

func TestRelaunchWaitReportsAChildKilledBySignal(t *testing.T) {
	sh := shellPath(t)
	cmd := exec.Command(sh, "-c", "kill -KILL $$")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if code, sig := relaunchWaitChild(cmd, make(chan os.Signal)); sig != syscall.SIGKILL {
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
