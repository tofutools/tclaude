package agentd

import (
	"bytes"
	"io"
	"os/exec"
	"slices"
	"strings"
	"testing"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// How a scripted probe answers, mirroring what tmux actually does on the wire:
// a pid on stdout, tmux's "no server running on …" on stderr with a non-zero
// exit, or a failure that says nothing either way.
const (
	probeNoServer = "no-server"
	probeBroken   = "broken"
)

// tuiTmuxRec records the tmux argv the --tui server lifecycle issues and
// scripts the answers it depends on, without a real tmux server. `pids` is the
// queue of probe results — a decimal pid, probeNoServer, or probeBroken —
// consumed one per `display-message`; failStart makes the start invocation
// fail. `panes` scripts the teardown's emptiness probe the way `list-panes -a`
// answers it: probeNoServer, probeBroken, or the literal stdout tmux would
// print (see tuiPanes). The zero value is a server with no panes at all.
type tuiTmuxRec struct {
	calls     [][]string
	pids      []string
	failStart bool
	panes     string
}

// tuiPanes builds the `list-panes -a -F "#{session_name}\t#{pane_dead}"` stdout
// for the given panes, so a test can describe what is on the server in the same
// terms tmux reports it: one line per PANE, not per session.
func tuiPanes(panes ...[2]string) string {
	var b strings.Builder
	for _, p := range panes {
		b.WriteString(p[0] + "\t" + p[1] + "\n")
	}
	return b.String()
}

// tuiLivePane / tuiDeadPane name the two pane_dead values at the call site,
// because "worker", "0" reads as a pane index rather than "not dead".
func tuiLivePane(session string) [2]string { return [2]string{session, "0"} }
func tuiDeadPane(session string) [2]string { return [2]string{session, "1"} }

// noServerScript is tmux's own shape for "no server running": the message on
// stderr, exit 1. The shell is here only because stderr redirection needs one;
// the script is a compile-time constant with nothing interpolated into it.
const noServerScript = "echo 'no server running on /tmp/tmux-1000/tclaude' >&2; exit 1"

func (r *tuiTmuxRec) Command(args ...string) *exec.Cmd {
	r.calls = append(r.calls, append([]string(nil), args...))
	if slices.Contains(args, "display-message") {
		answer := probeBroken
		if len(r.pids) > 0 {
			answer, r.pids = r.pids[0], r.pids[1:]
		}
		switch answer {
		case probeNoServer:
			return exec.Command("sh", "-c", noServerScript)
		case probeBroken:
			return exec.Command("false")
		default:
			return exec.Command("echo", answer)
		}
	}
	if slices.Contains(args, "list-panes") {
		switch r.panes {
		case probeNoServer:
			return exec.Command("sh", "-c", noServerScript)
		case probeBroken:
			return exec.Command("false")
		default:
			// printf, not echo: the empty script must produce empty stdout, which
			// is exactly what tmux prints for a server holding no panes.
			return exec.Command("printf", "%s", r.panes)
		}
	}
	if r.failStart && slices.Contains(args, "start-server") {
		return exec.Command("false")
	}
	return exec.Command("true")
}

// ListSessions satisfies clcommon.Tmux. The --tui lifecycle deliberately does
// not use it — see tuiTmuxLiveSessions for why — so a call here is a test
// failure waiting to happen rather than a value worth scripting.
func (r *tuiTmuxRec) ListSessions() (map[string]struct{}, error) {
	r.calls = append(r.calls, []string{"ListSessions"})
	return map[string]struct{}{}, nil
}

func (r *tuiTmuxRec) issued(subcommand string) bool {
	return slices.ContainsFunc(r.calls, func(call []string) bool {
		return slices.Contains(call, subcommand)
	})
}

// releasedExitEmpty reports the exact argv that hands a surviving server back to
// tmux's own lifetime rule. Matched whole rather than by substring: the STARTUP
// call also mentions exit-empty (setting it off), so `issued("exit-empty")`
// would pass on every run and pin nothing.
func (r *tuiTmuxRec) releasedExitEmpty() bool {
	return slices.ContainsFunc(r.calls, func(call []string) bool {
		return slices.Equal(call, []string{"set-option", "-gu", "exit-empty"})
	})
}

// installTUITmuxRec swaps the tmux boundary for the recorder and pins the two
// environment inputs the lifecycle reads: TMUX (so the delegation lookup answers
// from the environment instead of probing a server through the recorder) and the
// delegation directory itself.
func installTUITmuxRec(t *testing.T, delegationDir string, pids ...string) *tuiTmuxRec {
	t.Helper()
	t.Setenv("TMUX", "")
	t.Setenv(session.ResourceDelegationDirEnv, delegationDir)
	rec := &tuiTmuxRec{pids: pids}
	prev := clcommon.Default
	clcommon.Default = rec
	t.Cleanup(func() { clcommon.Default = prev })
	return rec
}

// TestTmuxServerOwnershipRequested pins the opt-in: taking over the tmux
// server's lifetime happens only when the operator asked for it AND there is a
// console to tie that lifetime to. Default-off is the point of the flag — a
// daemon that quietly started killing agent panes on exit would be a surprising
// upgrade — and --own-tmux-server without --tui has nothing to attach to.
func TestTmuxServerOwnershipRequested(t *testing.T) {
	cases := map[string]struct {
		params serveParams
		want   bool
	}{
		"neither flag":              {serveParams{}, false},
		"tui alone stays hands-off": {serveParams{TUI: true}, false},
		"flag without a console":    {serveParams{OwnTmuxServer: true}, false},
		"both":                      {serveParams{TUI: true, OwnTmuxServer: true}, true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := tmuxServerOwnershipRequested(&tc.params); got != tc.want {
				t.Errorf("tmuxServerOwnershipRequested(%+v) = %v, want %v", tc.params, got, tc.want)
			}
		})
	}
}

// TestStartTUITmuxServer_StartsEmptyThenKills pins the whole `serve --tui`
// tmux-server lifetime on a host with no server yet: the probe gets tmux's "no
// server running", one invocation brings the server up and turns exit-empty off
// in the same client connection (so the empty server does not exit the moment
// that client leaves), and the teardown — finding the server still empty —
// kills it outright rather than leaving it to linger with exit-empty off.
func TestStartTUITmuxServer_StartsEmptyThenKills(t *testing.T) {
	rec := installTUITmuxRec(t, "", probeNoServer, "4242", "4242")

	var notice bytes.Buffer
	stop, owned := startTUITmuxServer(&notice)
	if !owned {
		t.Fatal("starting a server on a host with none must claim ownership")
	}

	start := []string{"start-server", ";", "set-option", "-g", "exit-empty", "off"}
	if len(rec.calls) != 3 || !slices.Equal(rec.calls[1], start) {
		t.Fatalf("startup tmux calls = %v, want a probe, %v, and a pid read", rec.calls, start)
	}

	stop()

	if last := rec.calls[len(rec.calls)-1]; !slices.Equal(last, []string{"kill-server"}) {
		t.Fatalf("teardown tmux calls = %v, want a trailing [kill-server]", rec.calls)
	}
	// The operator quit the console and the server went with it. Saying so is
	// the whole point of the notice: the alternative outcome — a server left
	// running — is silent otherwise, and the two are indistinguishable from the
	// shell prompt they land back on.
	if got := notice.String(); !strings.Contains(got, "shut down") {
		t.Fatalf("teardown notice = %q, want it to report the server was shut down", got)
	}
}

// TestStartTUITmuxServer_LeavesAServerWithSessionsOnIt is the emptiness
// condition on the kill, on top of every ownership check: this daemon started
// the server, but by the time the console quits the operator has agents running
// on it. kill-server would take them out with no console left to ask, so the
// server stays and the operator is told why.
func TestStartTUITmuxServer_LeavesAServerWithSessionsOnIt(t *testing.T) {
	rec := installTUITmuxRec(t, "", probeNoServer, "4242", "4242")
	rec.panes = tuiPanes(tuiLivePane("worker-a"), tuiLivePane("worker-b"))

	var notice bytes.Buffer
	stop, owned := startTUITmuxServer(&notice)
	stop()

	if !owned {
		t.Error("a server this run started is still owned; only the kill is conditional")
	}
	if rec.issued("kill-server") {
		t.Fatalf("a server with sessions on it must survive teardown, got tmux calls %v", rec.calls)
	}
	got := notice.String()
	if !strings.Contains(got, "left running") || !strings.Contains(got, "2 sessions") {
		t.Fatalf("teardown notice = %q, want it to report 2 sessions keeping the server alive", got)
	}
	// exit-empty off outlives this process. A server we decline to kill must be
	// handed back to tmux's own rule, or it never exits once those sessions end
	// and the next --tui run — finding it already up — will not adopt it either.
	if !rec.releasedExitEmpty() {
		t.Fatalf("a surviving server must get exit-empty released, got tmux calls %v", rec.calls)
	}
}

// TestStartTUITmuxServer_CountsOneSessionInSingular keeps the one line the
// operator actually reads from reading like a bug in the thing that just
// declined to kill their agent.
func TestStartTUITmuxServer_CountsOneSessionInSingular(t *testing.T) {
	rec := installTUITmuxRec(t, "", probeNoServer, "4242", "4242")
	rec.panes = tuiPanes(tuiLivePane("worker-a"))

	var notice bytes.Buffer
	stop, _ := startTUITmuxServer(&notice)
	stop()

	if got := notice.String(); !strings.Contains(got, "1 session ") {
		t.Fatalf("teardown notice = %q, want a singular session count", got)
	}
}

// TestStartTUITmuxServer_CountsASessionOnceForAllItsPanes is why the probe reads
// panes and folds them into sessions rather than counting lines: a session with
// several panes is one session, and the operator is told how many sessions are
// keeping their server alive, not how many panes those sessions happen to hold.
func TestStartTUITmuxServer_CountsASessionOnceForAllItsPanes(t *testing.T) {
	rec := installTUITmuxRec(t, "", probeNoServer, "4242", "4242")
	rec.panes = tuiPanes(tuiLivePane("worker-a"), tuiLivePane("worker-a"), tuiLivePane("worker-a"))

	var notice bytes.Buffer
	stop, _ := startTUITmuxServer(&notice)
	stop()

	if got := notice.String(); !strings.Contains(got, "1 session ") {
		t.Fatalf("teardown notice = %q, want three panes of one session counted once", got)
	}
}

// TestStartTUITmuxServer_KillsAServerHoldingOnlyDeadPanes keeps the ordinary
// path working. An agent that has run and exited leaves its session behind as a
// retained-dead pane holding scrollback — that is what remain-on-exit is for.
// Counting those as work in progress would mean the server survived almost
// every real session, which is the feature failing to fire rather than failing
// safe.
func TestStartTUITmuxServer_KillsAServerHoldingOnlyDeadPanes(t *testing.T) {
	rec := installTUITmuxRec(t, "", probeNoServer, "4242", "4242")
	rec.panes = tuiPanes(tuiDeadPane("worker-a"), tuiDeadPane("worker-b"))

	var notice bytes.Buffer
	stop, _ := startTUITmuxServer(&notice)
	stop()

	if !rec.issued("kill-server") {
		t.Fatalf("a server holding only corpses must still be shut down, got tmux calls %v", rec.calls)
	}
	if got := notice.String(); !strings.Contains(got, "shut down") {
		t.Fatalf("teardown notice = %q, want it to report the server was shut down", got)
	}
}

// TestStartTUITmuxServer_KeepsASessionAliveForOneLivePane is the multi-pane
// direction of the same read, and the reason the probe cannot use a
// session-scoped #{pane_dead}: tmux resolves a pane variable on `list-sessions`
// against the session's CURRENT window's active pane only. A session whose
// harness pane has exited while a second window still runs something must count
// as live, or the check kills the very work it exists to protect.
func TestStartTUITmuxServer_KeepsASessionAliveForOneLivePane(t *testing.T) {
	rec := installTUITmuxRec(t, "", probeNoServer, "4242", "4242")
	rec.panes = tuiPanes(tuiDeadPane("worker-a"), tuiLivePane("worker-a"))

	var notice bytes.Buffer
	stop, _ := startTUITmuxServer(&notice)
	stop()

	if rec.issued("kill-server") {
		t.Fatalf("one live pane must keep its session alive, got tmux calls %v", rec.calls)
	}
	if got := notice.String(); !strings.Contains(got, "1 session ") {
		t.Fatalf("teardown notice = %q, want the part-dead session counted as live", got)
	}
}

// TestStartTUITmuxServer_KeepsAServerWhoseSessionsItCannotList applies the
// probe's fail-safe rule to the new condition: a listing that errors says
// nothing about whether the server is empty, and kill-server does not ask
// twice. Leaving it costs an idle server; killing it could cost the sessions
// the listing failed to name. This is also why the check does not go through
// clcommon.ListSessions, which turns any non-zero tmux exit into the empty set
// — a fine answer for a dashboard poll, a kill order here.
func TestStartTUITmuxServer_KeepsAServerWhoseSessionsItCannotList(t *testing.T) {
	rec := installTUITmuxRec(t, "", probeNoServer, "4242", "4242")
	rec.panes = probeBroken

	var notice bytes.Buffer
	stop, _ := startTUITmuxServer(&notice)
	stop()

	if rec.issued("kill-server") {
		t.Fatalf("an unlistable server must survive teardown, got tmux calls %v", rec.calls)
	}
	if got := notice.String(); !strings.Contains(got, "left running") {
		t.Fatalf("teardown notice = %q, want it to report the server was left running", got)
	}
}

// TestStartTUITmuxServer_DoesNotUseTheDashboardSessionRead pins the boundary
// choice itself. clcommon.ListSessions is the snapshot read the dashboard and
// peer listings use; its documented semantics collapse a failed listing into
// "nothing is alive", which is the one answer this teardown must never act on.
func TestStartTUITmuxServer_DoesNotUseTheDashboardSessionRead(t *testing.T) {
	rec := installTUITmuxRec(t, "", probeNoServer, "4242", "4242")
	rec.panes = tuiPanes(tuiLivePane("worker-a"))

	stop, _ := startTUITmuxServer(io.Discard)
	stop()

	if rec.issued("ListSessions") {
		t.Fatalf("the emptiness check must not route through ListSessions, got tmux calls %v", rec.calls)
	}
}

// TestStartTUITmuxServer_ChecksEmptinessOnlyAfterOwnership pins the order the
// conditions are evaluated in. A server that is not ours must not even be
// inspected: the emptiness check exists to protect sessions on a server we would
// otherwise kill, and a server we will never kill has nothing to protect. Its
// exit-empty is not ours to restore either.
func TestStartTUITmuxServer_ChecksEmptinessOnlyAfterOwnership(t *testing.T) {
	rec := installTUITmuxRec(t, "", probeNoServer, "4242", "9999")

	var notice bytes.Buffer
	stop, _ := startTUITmuxServer(&notice)
	stop()

	if rec.issued("list-panes") || rec.releasedExitEmpty() {
		t.Fatalf("a replacement server must not be inspected or reconfigured, got tmux calls %v", rec.calls)
	}
	// Silence would be indistinguishable from "this daemon never owned a
	// server", so even the outcomes that predate the emptiness check say what
	// they did.
	if got := notice.String(); !strings.Contains(got, "left running") {
		t.Fatalf("teardown notice = %q, want it to report the server was left running", got)
	}
}

// TestStartTUITmuxServer_LeavesAServerThatWasAlreadyRunning is the ownership
// rule: a server that predates the console came from an earlier daemon, a
// `tclaude session new`, or the operator's own tmux, and its sessions are not
// this console's to end. It is neither reconfigured on the way in nor killed on
// the way out — only the probe may touch it.
func TestStartTUITmuxServer_LeavesAServerThatWasAlreadyRunning(t *testing.T) {
	rec := installTUITmuxRec(t, "", "1111")

	stop, owned := startTUITmuxServer(io.Discard)
	stop()

	if owned {
		t.Error("a server this run did not start must not be reported as owned")
	}
	if len(rec.calls) != 1 || !slices.Contains(rec.calls[0], "display-message") {
		t.Fatalf("tmux calls = %v, want the pid probe and nothing else", rec.calls)
	}
}

// TestStartTUITmuxServer_DeclinesOwnershipWhenTheProbeCannotTell is the
// fail-safe direction. A probe that errors for any reason other than "no server
// running" — tmux missing, too old for -N, a permission error — says nothing
// about what is out there, and the console must not claim, reconfigure, or
// later kill a server on that basis.
func TestStartTUITmuxServer_DeclinesOwnershipWhenTheProbeCannotTell(t *testing.T) {
	rec := installTUITmuxRec(t, "", probeBroken)

	stop, owned := startTUITmuxServer(io.Discard)
	stop()

	if owned {
		t.Error("an unreadable probe must not be read as ownership")
	}
	if rec.issued("start-server") || rec.issued("kill-server") {
		t.Fatalf("an unreadable probe must start and kill nothing, got tmux calls %v", rec.calls)
	}
}

// TestStartTUITmuxServer_DoesNotTrustANonPIDProbe pins the charset gate as part
// of that same rule: tmux writing anything but a bare decimal pid (a warning
// line that reached stdout, a changed format) is "cannot tell", not "no
// server" — which would otherwise adopt, and then kill, a live server.
func TestStartTUITmuxServer_DoesNotTrustANonPIDProbe(t *testing.T) {
	rec := installTUITmuxRec(t, "", "tmux: warning 4242")

	stop, owned := startTUITmuxServer(io.Discard)
	stop()

	if owned || rec.issued("start-server") || rec.issued("kill-server") {
		t.Fatalf("a non-pid probe answer must be treated as unknown, got owned=%v calls=%v",
			owned, rec.calls)
	}
}

// TestStartTUITmuxServer_KeepsAServerItCanNoLongerVerify covers the teardown
// half of the same fail-safe: the server was ours at startup, but by exit the
// probe cannot answer. kill-server does not ask whose server it is, so an
// unverifiable one is left running.
func TestStartTUITmuxServer_KeepsAServerItCanNoLongerVerify(t *testing.T) {
	rec := installTUITmuxRec(t, "", probeNoServer, "4242", probeBroken)

	stop, _ := startTUITmuxServer(io.Discard)
	stop()

	if rec.issued("kill-server") {
		t.Fatalf("an unverifiable server must survive teardown, got tmux calls %v", rec.calls)
	}
}

// TestStartTUITmuxServer_KillsEvenWhenItsOwnPIDWasUnreadable is the other side
// of that line, and the reason the two probes are not treated alike: the
// startup probe said definitively that no server was running, so this run is
// what put one there. Refusing to kill on a missing pid would leak the empty
// server — with exit-empty off it would never exit on its own.
func TestStartTUITmuxServer_KillsEvenWhenItsOwnPIDWasUnreadable(t *testing.T) {
	rec := installTUITmuxRec(t, "", probeNoServer, probeBroken, "4242")

	stop, owned := startTUITmuxServer(io.Discard)
	stop()

	if !owned || !rec.issued("kill-server") {
		t.Fatalf("a server this run started must still be killed, got owned=%v calls=%v",
			owned, rec.calls)
	}
}

// TestStartTUITmuxServer_LeavesAReplacementServerAlone covers the gap between
// the probe and the start: if the server this run created dies and something
// else claims the socket, the recorded pid stops matching and the teardown must
// keep its hands off the newcomer.
func TestStartTUITmuxServer_LeavesAReplacementServerAlone(t *testing.T) {
	rec := installTUITmuxRec(t, "", probeNoServer, "4242", "9999")

	stop, _ := startTUITmuxServer(io.Discard)
	stop()

	if rec.issued("kill-server") {
		t.Fatalf("a replacement server must survive teardown, got tmux calls %v", rec.calls)
	}
}

// TestStartTUITmuxServer_LeavesAnExternalRuntimeAlone guards the resource
// delegation contract: that tmux server belongs to a separate, longer-lived
// systemd unit, so the console must neither start one (which would land it in
// agentd's own cgroup) nor kill it on the way out (which would take down panes
// meant to outlive agentd).
func TestStartTUITmuxServer_LeavesAnExternalRuntimeAlone(t *testing.T) {
	rec := installTUITmuxRec(t, "/sys/fs/cgroup/system.slice/tclaude-tmux.service")

	stop, owned := startTUITmuxServer(io.Discard)
	stop()

	if owned {
		t.Error("an external tmux runtime must never be reported as owned")
	}
	if len(rec.calls) != 0 {
		t.Fatalf("external tmux runtime must be untouched, got tmux calls %v", rec.calls)
	}
}

// TestStartTUITmuxServer_DoesNotKillAServerItCouldNotStart keeps a failed
// startup from turning into a kill-server aimed at whatever is out there: if
// this daemon never created a server, teardown has nothing to tear down.
func TestStartTUITmuxServer_DoesNotKillAServerItCouldNotStart(t *testing.T) {
	rec := installTUITmuxRec(t, "", probeNoServer, "4242", "4242")
	rec.failStart = true

	stop, owned := startTUITmuxServer(io.Discard)
	stop()

	if owned {
		t.Error("a failed start must not claim ownership")
	}
	if rec.issued("kill-server") {
		t.Fatalf("failed startup must not kill a server, got tmux calls %v", rec.calls)
	}
}
