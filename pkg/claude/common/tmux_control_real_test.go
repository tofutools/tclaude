package common

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/tmuxctl"
)

// realControlTmux builds a ControlTmux over a private tmux socket under an
// isolated HOME, so neither the operator's ~/.tmux.conf nor the real tclaude
// server is ever touched.
func realControlTmux(t *testing.T) (*ControlTmux, func(args ...string) *exec.Cmd) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	t.Setenv("HOME", t.TempDir())
	// A short socket path: unix socket paths are limited to ~104 bytes.
	socket := filepath.Join(os.TempDir(), fmt.Sprintf("tcc-%d-%d.sock", os.Getpid(), time.Now().UnixNano()))
	base := []string{"-u", "-f", "/dev/null", "-S", socket}
	command := func(args ...string) *exec.Cmd {
		return exec.Command("tmux", append(append([]string(nil), base...), args...)...)
	}
	ct := newControlTmux(command, tmuxctl.Config{
		BaseArgs:      base,
		ExitWhenAlone: true,
		Timeout:       5 * time.Second,
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	t.Cleanup(func() {
		ct.Close()
		_ = command("kill-server").Run()
		_ = os.Remove(socket)
	})
	return ct, command
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func serverUp(command func(args ...string) *exec.Cmd) bool {
	return command("-N", "list-sessions").Run() == nil
}

func TestControlTmuxRealServerLifecycle(t *testing.T) {
	ct, command := realControlTmux(t)

	// No server: the empty set, no connection, and no server started.
	got, err := ct.ListSessions()
	if err != nil || len(got) != 0 {
		t.Fatalf("no server: ListSessions = %v, %v", got, err)
	}
	if ct.client.Connected() || serverUp(command) {
		t.Fatal("control mode must not start a server")
	}

	// A session whose name has a space and a session with a dead pane.
	if out, err := command("new-session", "-d", "-s", "agent one", "sleep", "300").CombinedOutput(); err != nil {
		t.Fatalf("new-session: %v %s", err, out)
	}
	// remain-on-exit is set before the pane's process ends (respawn-pane),
	// so the retained-dead pane is deterministic.
	if out, err := command("new-session", "-d", "-s", "corpse", "sleep", "300", ";",
		"set-option", "-t", "=corpse:", "remain-on-exit", "on", ";",
		"respawn-pane", "-k", "-t", "=corpse:", "true").CombinedOutput(); err != nil {
		t.Fatalf("new-session corpse: %v %s", err, out)
	}
	waitFor(t, "corpse pane to die", func() bool {
		out, _ := command("display-message", "-p", "-t", "=corpse:", "#{pane_dead}").Output()
		return string(out) == "1\n"
	})

	got, err = ct.ListSessions() // exec path; triggers the connect
	if err != nil {
		t.Fatal(err)
	}
	assertAlive(t, got, "agent one")
	waitFor(t, "control connection", ct.client.Connected)

	// Over control mode: same answer, hidden session excluded.
	got, err = ct.ListSessions()
	if err != nil {
		t.Fatal(err)
	}
	assertAlive(t, got, "agent one")
	if ct.client.State().Requests == 0 {
		t.Fatal("ListSessions did not go over control mode")
	}

	// Once only the hidden session would be left, the client removes it, so
	// the server exits when empty as it does without control mode.
	if err := command("kill-session", "-t", "=corpse").Run(); err != nil {
		t.Fatal(err)
	}
	if err := command("kill-session", "-t", "=agent one").Run(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "control client to leave", func() bool { return !ct.client.Connected() })
	waitFor(t, "server to exit", func() bool { return !serverUp(command) })
	got, err = ct.ListSessions()
	if err != nil || len(got) != 0 {
		t.Fatalf("after exit: %v, %v", got, err)
	}
	if serverUp(command) {
		t.Fatal("a listing resurrected the server")
	}
}

func TestControlTmuxSurvivesKillServer(t *testing.T) {
	ct, command := realControlTmux(t)
	if err := command("new-session", "-d", "-s", "a", "sleep", "300").Run(); err != nil {
		t.Fatal(err)
	}
	if _, err := ct.ListSessions(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "control connection", ct.client.Connected)

	if out, err := command("kill-server").CombinedOutput(); err != nil {
		t.Fatalf("kill-server blocked: %v %s", err, out)
	}
	waitFor(t, "disconnect", func() bool { return !ct.client.Connected() })
	got, err := ct.ListSessions()
	if err != nil || len(got) != 0 {
		t.Fatalf("after kill-server: %v, %v", got, err)
	}
	if serverUp(command) {
		t.Fatal("control client resurrected a killed server")
	}

	// A new server and session: the client reconnects.
	if err := command("new-session", "-d", "-s", "b", "sleep", "300").Run(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "reconnect", func() bool {
		got, err := ct.ListSessions()
		_, ok := got["b"]
		return err == nil && ok && ct.client.Connected()
	})
	if st := ct.client.State(); st.Connects < 2 {
		t.Fatalf("connects = %d, want a reconnect", st.Connects)
	}
}

func assertAlive(t *testing.T, got map[string]struct{}, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("alive = %v, want %v", got, want)
	}
	for _, w := range want {
		if _, ok := got[w]; !ok {
			t.Fatalf("alive = %v, want %v", got, want)
		}
	}
}

// A hook that prints (as many tmux plugins' hooks do) fires for the hidden
// session too; its output must not drop the control connection.
func TestControlTmuxToleratesPrintingHook(t *testing.T) {
	ct, command := realControlTmux(t)
	if out, err := command("new-session", "-d", "-s", "a", "sleep", "300", ";",
		"set-hook", "-g", "session-created", "display-message -p created").CombinedOutput(); err != nil {
		t.Fatalf("setup: %v %s", err, out)
	}
	if _, err := ct.ListSessions(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "control connection", ct.client.Connected)
	if err := command("new-session", "-d", "-s", "b", "sleep", "300").Run(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "b over control mode", func() bool {
		got, err := ct.ListSessions()
		_, ok := got["b"]
		return err == nil && ok
	})
	if st := ct.client.State(); !st.Connected || st.Connects != 1 {
		t.Fatalf("state = %+v, want one steady connection", st)
	}
}

// Stage 3 routing: commands sent through TmuxOutput/TmuxRun over a live
// control connection give exactly what the exec path gives, including
// arguments the command-line quoting has to carry intact.
func TestTmuxRouteMatchesExec(t *testing.T) {
	ct, command := realControlTmux(t)
	if out, err := command("new-session", "-d", "-s", "r", "-x", "60", "-y", "5",
		"printf 'red \\033[31mx\\033[0m tab\\there é\\n'; sleep 300").CombinedOutput(); err != nil {
		t.Fatalf("new-session: %v %s", err, out)
	}
	if _, err := ct.ListSessions(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "control connection", ct.client.Connected)
	prev := Default
	Default = ct
	t.Cleanup(func() { Default = prev })

	waitFor(t, "pane output", func() bool {
		out, _ := command("capture-pane", "-p", "-t", "=r:").Output()
		return len(out) > 0 && out[0] == 'r'
	})
	for _, args := range [][]string{
		{"capture-pane", "-p", "-e", "-J", "-t", "=r:"},
		{"display-message", "-p", "-t", "=r:", "#{session_name}\t#{pane_dead}\t#{window_panes}"},
		{"list-panes", "-a", "-F", "#{session_name} #{pane_dead}"},
	} {
		want, err := command(args...).Output()
		if err != nil {
			t.Fatal(err)
		}
		got, err := TmuxOutput(args...)
		if err != nil {
			t.Fatalf("%q: %v", args, err)
		}
		// list-panes -a also lists the hidden session's pane over both paths.
		if string(got) != string(want) {
			t.Errorf("%q:\n got %q\nwant %q", args, got, want)
		}
	}

	for _, v := range []string{`plain`, `it's`, `a "q" b`, `$HOME ~ \e \\ ; # {x}`, `#{session_name}`, "tab\tx", `'`, `it's $HOME ~ \e \\ "x" ; #{y}`} {
		if err := TmuxRun("set-option", "-t", "=r:", "@probe", v); err != nil {
			t.Fatalf("set %q: %v", v, err)
		}
		out, err := command("show-options", "-v", "-t", "=r:", "@probe").Output()
		if err != nil {
			t.Fatal(err)
		}
		if string(out) != v+"\n" {
			t.Errorf("round trip %q: got %q", v, out)
		}
	}

	if err := TmuxRun("has-session", "-t", "=nope"); !IsTmuxCommandFailure(err) {
		t.Fatalf("has-session on a missing session: %v", err)
	}
	if out, err := TmuxCombinedOutput("kill-session", "-t", "=nope"); err == nil || len(out) == 0 {
		t.Fatalf("kill-session missing: %q, %v", out, err)
	}
	if st := ct.Status(); st.Routed["capture-pane"] == 0 || st.Routed["set-option"] == 0 || st.Routed["has-session"] == 0 {
		t.Fatalf("not routed over control mode: %v", st.Routed)
	}

	// An argument with a line break cannot be one control line: it runs over
	// exec instead and still works.
	if err := TmuxRun("set-option", "-t", "=r:", "@probe", "two\nlines"); err != nil {
		t.Fatal(err)
	}
}
