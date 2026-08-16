package agentd_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/agentd"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

var agentdTestTmuxBase string

// TestMain installs a binary-wide default for the terminal-spawning
// seam, so any test that reaches an openTerminal call site without
// swapping its own stub gets a deterministic "could not open" error —
// the same degraded path a headless CI host exercises — instead of
// popping a real terminal window onto a developer desktop (TCL-584).
//
// Tests that assert on the open path still swap their own
// recorder/stub via agentd.SetOpenTerminalForTest; their restore puts
// this default back, never the real launcher. terminal.OpenWithCommand
// additionally refuses all test binaries outright, so this default is
// the first of two layers, not the only one.
//
// One TestMain governs the whole test binary, including the internal
// `package agentd` test files compiled alongside this external
// package.
func TestMain(m *testing.M) {
	agentd.SetPresentedPRAccessValidatorForTest(func(string, string, string) error { return nil })
	agentd.SetPresentedPRRemotePolicyCheckForTest(func(string) error { return nil })
	// Keep every tmux command in this test binary away from the operator's
	// live `-L tclaude` server. This belongs here rather than in newFlow:
	// internal package agentd tests share this binary but cannot call the
	// external-package fixture. Tests that need a specific TMUX_TMPDIR override
	// it after this point; t.Setenv cleanup restores this isolated default.
	// Use the short system temp root explicitly. macOS's ambient TMPDIR lives
	// below /var/folders/...; adding tmux's own tmux-UID/socket suffix there can
	// exceed the platform's Unix-socket path limit before a server can start.
	// Sandboxed helper copies of this test binary may correctly be denied direct
	// /tmp writes; give those processes their own ambient-temp base instead.
	tmuxBase, shortRootErr := os.MkdirTemp("/tmp", "tclaude-agentd-")
	if shortRootErr != nil {
		var ambientErr error
		tmuxBase, ambientErr = os.MkdirTemp("", "tclaude-agentd-")
		if ambientErr != nil {
			panic(fmt.Sprintf("create isolated agentd test tmux base: short root: %v; ambient root: %v", shortRootErr, ambientErr))
		}
	}
	var err error
	tmuxBase, err = filepath.EvalSymlinks(tmuxBase)
	if err != nil {
		panic(fmt.Sprintf("canonicalize isolated agentd test tmux base: %v", err))
	}
	if err := os.Setenv("TMUX_TMPDIR", tmuxBase); err != nil {
		panic(fmt.Sprintf("set isolated agentd test TMUX_TMPDIR: %v", err))
	}
	agentdTestTmuxBase = tmuxBase
	tmuxSocketPath := filepath.Join(tmuxBase, fmt.Sprintf("tmux-%d", os.Getuid()), clcommon.TmuxSocketName)

	agentd.SetOpenTerminalForTest(func(string) error {
		return errors.New("agentd tests: terminal spawn suppressed by default (TCL-584); swap agentd.SetOpenTerminalForTest to observe the open path")
	})
	// Resolving a filtered Codex launch reads Codex's effective config by
	// executing the real binary. CI shards that run these flows have no Codex
	// install, so without a default every Codex filtered flow would refuse for
	// a missing executable rather than exercising the path under test. This
	// default answers with Codex's own unconfigured shape — no provider
	// override, no base URL, no remotely delivered layer — which resolves to
	// the first-party API route. Tests needing a different route swap their own.
	// The soft-exit escalation ladder's last two rungs are kill(-pgid, SIGTERM)
	// then SIGKILL against the pane process. Only the first rung — tmux
	// kill-pane — is simulated; these two were the real syscalls in any test
	// that did not swap them, and the tmux simulator used to hand its first
	// session pane pid 1. kill(2) reads a negative pid as a process group, and
	// -1 is not "an unlikely group": it is the kernel's wildcard for every
	// process the caller may signal. So a test that let the ladder past
	// kill-pane SIGTERMed the test binary, its `go test` parent, and on a
	// developer's or agent's machine the shell around them — writing no
	// failure and no stack, so the whole event read as infrastructure trouble
	// (TCL-1035).
	//
	// BINARY-WIDE rather than in newFlow, and that is the point: newFlow lives
	// in package agentd_test, so a default there protects only the tests that
	// call it. The internal `package agentd` files compiled into this same
	// binary drive stopOneConvWithIntent directly with hand-rolled tmux stubs
	// and cannot call newFlow. TestMain is the only seam that covers both, and
	// it is where the terminal-spawn suppression above already sits for the
	// same reason.
	//
	// alive=false stands the ladder down before any signal is attempted. Tests
	// that assert on the signal rungs swap their own pair; their restore puts
	// this default back, never the real syscalls.
	restoreEscalationProcess := agentd.SetSoftExitEscalationProcessForTest(
		func(int) bool { return false },
		func(int, syscall.Signal) error { return nil },
	)
	// Flow tests spawn API-driven Copilot agents against a simulated tmux with
	// no Copilot process anywhere, so the real bootstrap would spend its whole
	// minute-long budget polling for a listener that cannot appear — and would
	// still be running, against a torn-down database, after the test returned.
	// Suppressed binary-wide for the same reason as the two defaults above:
	// package agentd's internal tests share this binary and cannot reach
	// newFlow. Tests that want to observe the kick-off swap their own.
	restoreCopilotBootstrap := agentd.SetCopilotAPIBootstrapForTest(func(string, bool, bool, string) {})
	// And its consequence: with the bootstrap suppressed no handle can ever be
	// adopted, so the spawn's post-init wait for one would burn its whole
	// budget on every API-drive spawn before concluding what is already known
	// here. "It never came up" is the truthful answer under that stub, and it
	// is the arm that falls through to the pane — which is what these tests
	// assert against.
	restoreCopilotPostInitWait := agentd.SetCopilotAPIPostInitWaitForTest(
		func(string) bool { return false })
	// The daemon's startup reconcile, suppressed for the same reason as the
	// bootstrap: a flow test that starts a daemon would otherwise sweep the
	// shared database for port records and spend a port wait per row against a
	// tmux that has no Copilot process behind it. Tests that want to observe the
	// reconcile drive it directly rather than through a daemon start.
	restoreCopilotReconnect := agentd.SetCopilotAPIReconnectForTest(func(<-chan struct{}) {})
	restoreCodexProbe := session.SetCodexEffectiveConfigProbeForTest(
		func(string, []sandboxpolicy.EnvironmentEntry, string) (json.RawMessage, error) {
			return json.RawMessage(`{"config":{},"origins":{}}`), nil
		})
	restoreCodexNativeRegistry := agentd.SetCodexNativeRegistryReadinessForTest(func() error { return nil })
	code := m.Run()
	restoreCodexNativeRegistry()
	restoreCodexProbe()
	restoreCopilotReconnect()
	restoreCopilotPostInitWait()
	restoreCopilotBootstrap()
	restoreEscalationProcess()
	// A real detached session would keep its server and panes alive after its
	// socket is unlinked. Target the computed private socket explicitly rather
	// than trusting teardown-time environment; no server is the normal case.
	_ = exec.Command("tmux", "-S", tmuxSocketPath, "kill-server").Run()
	_ = os.RemoveAll(tmuxBase)
	os.Exit(code)
}

func TestMainIsolatesTmuxServer(t *testing.T) {
	if agentdTestTmuxBase == "" {
		t.Fatal("TestMain did not install a throwaway tmux base; agentd tests can reach the operator's live server")
	}
	if got := os.Getenv("TMUX_TMPDIR"); got != agentdTestTmuxBase {
		t.Fatalf("TMUX_TMPDIR = %q, want TestMain's throwaway base %q; agentd tests can reach another tmux server", got, agentdTestTmuxBase)
	}
	got, err := clcommon.TclaudeTmuxSocketDir()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(agentdTestTmuxBase, fmt.Sprintf("tmux-%d", os.Getuid()))
	if got != want {
		t.Fatalf("tclaude tmux socket directory = %q, want isolated directory %q", got, want)
	}
}

func TestMainReplacesAmbientTmuxBase(t *testing.T) {
	const childEnv = "TCLAUDE_AGENTD_TEST_AMBIENT_BASE_CHILD"
	if ambient := os.Getenv(childEnv); ambient != "" {
		if agentdTestTmuxBase == ambient || os.Getenv("TMUX_TMPDIR") == ambient {
			t.Fatalf("TestMain retained ambient tmux base %q; agentd tests can reach that server", ambient)
		}
		return
	}

	for _, ambient := range []string{"/tmp", t.TempDir()} {
		ambient := ambient
		t.Run(filepath.Base(ambient), func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestMainReplacesAmbientTmuxBase$", "-test.count=1")
			cmd.Env = append(os.Environ(),
				childEnv+"="+ambient,
				"TMUX_TMPDIR="+ambient,
				// Regression for the discarded inheritance design: this private-looking
				// marker must not make an arbitrary live base trustworthy.
				"TCLAUDE_AGENTD_TEST_TMUX_BASE="+ambient,
			)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("child with ambient tmux base %q failed: %v\n%s", ambient, err, out)
			}
		})
	}
}
