package agent

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/launcher"
)

type launcherMemFS map[string][]byte

func (m launcherMemFS) ReadFile(p string) ([]byte, error) {
	if b, ok := m[p]; ok {
		return b, nil
	}
	return nil, os.ErrNotExist
}
func (m launcherMemFS) WriteFile(p string, b []byte, _ os.FileMode) error { m[p] = b; return nil }
func (m launcherMemFS) MkdirAll(string, os.FileMode) error                { return nil }
func (m launcherMemFS) Remove(p string) error                             { delete(m, p); return nil }

type openFixture struct {
	deps      openDeps
	up        bool
	upAfter   int // DaemonUp turns true after this many polls once started
	started   bool
	polls     int
	calls     []string
	served    [][]string
	servedEnv []string
	notified  []string
	opened    int
}

func newOpenFixture(goos string, unit []byte) *openFixture {
	f := &openFixture{}
	fs := launcherMemFS{}
	paths := launcher.AutostartPaths(goos, "/home/u", "")
	if unit != nil {
		fs[paths.Unit] = unit
	}
	run := func(_ context.Context, name string, args ...string) ([]byte, error) {
		cmd := name + " " + strings.Join(args, " ")
		f.calls = append(f.calls, cmd)
		if strings.Contains(cmd, " start ") || strings.Contains(cmd, "kickstart") || strings.Contains(cmd, "bootstrap") {
			f.started = true
		}
		return nil, nil
	}
	f.deps = openDeps{
		GOOS: goos, UID: 501, Exe: "/home/u/.local/bin/tclaude",
		DaemonUp: func() bool {
			if f.up {
				return true
			}
			if f.started {
				f.polls++
				return f.polls > f.upAfter
			}
			return false
		},
		Dashboard: func(io.Writer) int { f.opened++; return rcOK },
		Autostart: launcher.Autostart{GOOS: goos, UID: 501, Paths: paths, Run: run, FS: fs},
		Run:       run,
		StartServe: func(argv, env []string, _ string) error {
			f.started = true
			f.served = append(f.served, argv)
			f.servedEnv = env
			return nil
		},
		Profile: func() (*config.LaunchProfile, error) { return nil, nil },
		Environ: []string{"PATH=/home/u/.local/bin:/usr/bin"},
		LogFile: "/home/u/.tclaude/data/launcher-agentd.log",
		Notify:  func(msg string) { f.notified = append(f.notified, msg) },
		Sleep:   func(time.Duration) {},
		WaitFor: 2 * time.Second,
	}
	return f
}

func TestLauncherOpenWithDaemonRunning(t *testing.T) {
	f := newOpenFixture("linux", nil)
	f.up = true
	if rc := runLauncherOpen(true, f.deps, io.Discard, io.Discard); rc != rcOK {
		t.Fatal(rc)
	}
	if f.opened != 1 || f.started || len(f.calls) != 0 {
		t.Fatalf("%+v", f)
	}
}

func TestLauncherOpenStartsThroughTheManagedUnit(t *testing.T) {
	f := newOpenFixture("linux", []byte("# "+launcher.ManagedMarker+"\n"))
	f.upAfter = 3
	if rc := runLauncherOpen(true, f.deps, io.Discard, io.Discard); rc != rcOK {
		t.Fatal(rc)
	}
	if !strings.Contains(strings.Join(f.calls, "\n"), "systemctl --user start tclaude-agentd.service") || len(f.served) != 0 || f.opened != 1 {
		t.Fatalf("calls %v served %v", f.calls, f.served)
	}

	f = newOpenFixture("darwin", []byte("<!-- "+launcher.ManagedMarker+" -->"))
	if rc := runLauncherOpen(true, f.deps, io.Discard, io.Discard); rc != rcOK {
		t.Fatal(rc)
	}
	if !strings.Contains(strings.Join(f.calls, "\n"), "launchctl kickstart gui/501/"+launcher.LaunchdLabel) {
		t.Fatalf("calls %v", f.calls)
	}
}

func TestLauncherOpenStartsDetachedWithTheBakedFlags(t *testing.T) {
	// A hand-made unit is not tclaude's to start.
	f := newOpenFixture("linux", []byte("[Service]\nExecStart=/mine\n"))
	f.deps.ServeArgs = []string{"--no-tray"}
	f.deps.Profile = func() (*config.LaunchProfile, error) {
		return &config.LaunchProfile{Flags: []string{"--ignored-here"}, Env: []config.LaunchEnv{{Key: "LANG", Value: "C.UTF-8"}}}, nil
	}
	if rc := runLauncherOpen(true, f.deps, io.Discard, io.Discard); rc != rcOK {
		t.Fatal(rc)
	}
	if len(f.served) != 1 || strings.Join(f.served[0], " ") != "/home/u/.local/bin/tclaude agentd serve --no-tray" {
		t.Fatalf("served %v calls %v", f.served, f.calls)
	}
	if !strings.Contains(strings.Join(f.servedEnv, "\n"), "LANG=C.UTF-8") || !strings.Contains(strings.Join(f.servedEnv, "\n"), "PATH=/home/u/.local/bin") {
		t.Fatalf("env %v", f.servedEnv)
	}
}

func TestLauncherOpenFailuresNotify(t *testing.T) {
	// Without --start-daemon nothing is started.
	f := newOpenFixture("linux", nil)
	if rc := runLauncherOpen(false, f.deps, io.Discard, io.Discard); rc == rcOK || f.started || len(f.notified) != 1 {
		t.Fatalf("rc %d %+v", rc, f)
	}
	// Started, but never answers within the bound.
	f = newOpenFixture("linux", nil)
	f.upAfter = 1 << 30
	var stderr bytes.Buffer
	if rc := runLauncherOpen(true, f.deps, io.Discard, &stderr); rc == rcOK || f.opened != 0 {
		t.Fatal("opened a dashboard with no daemon")
	}
	if len(f.notified) != 1 || !strings.Contains(f.notified[0], "did not answer") || !strings.Contains(f.notified[0], "launcher-agentd.log") {
		t.Fatalf("notified %v", f.notified)
	}
	// The start itself fails.
	f = newOpenFixture("linux", nil)
	f.deps.StartServe = func([]string, []string, string) error { return errors.New("boom") }
	if rc := runLauncherOpen(true, f.deps, io.Discard, io.Discard); rc == rcOK || len(f.notified) != 1 || !strings.Contains(f.notified[0], "boom") {
		t.Fatalf("notified %v", f.notified)
	}
	// The dashboard fails to open.
	f = newOpenFixture("linux", nil)
	f.up = true
	f.deps.Dashboard = func(w io.Writer) int { _, _ = io.WriteString(w, "Error: no browser\n"); return rcIOFailure }
	if rc := runLauncherOpen(true, f.deps, io.Discard, io.Discard); rc == rcOK || len(f.notified) != 1 || !strings.Contains(f.notified[0], "no browser") {
		t.Fatalf("notified %v", f.notified)
	}
}

// A managed unit whose supervisor does not run: start agentd directly.
func TestLauncherOpenFallsBackWhenTheUnitFails(t *testing.T) {
	f := newOpenFixture("linux", []byte("# "+launcher.ManagedMarker+"\n"))
	run := f.deps.Run
	failStart := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if strings.Contains(strings.Join(args, " "), "start "+launcher.SystemdUnitName) {
			f.calls = append(f.calls, "failed start")
			return nil, errors.New("Failed to connect to bus")
		}
		return run(ctx, name, args...)
	}
	f.deps.Run, f.deps.Autostart.Run = failStart, failStart
	f.deps.ServeArgs = []string{"--persist-operator-token"}
	var stdout bytes.Buffer
	if rc := runLauncherOpen(true, f.deps, &stdout, io.Discard); rc != rcOK {
		t.Fatal(rc)
	}
	if len(f.served) != 1 || f.opened != 1 || !strings.Contains(stdout.String(), "Failed to connect to bus") {
		t.Fatalf("served %v out %q calls %v", f.served, stdout.String(), f.calls)
	}
}

// The launcher script always passes the serve flags after --.
func TestLauncherOpenTakesServeFlagsAfterDash(t *testing.T) {
	open, _, err := serviceLauncherCmd().Find([]string{"open"})
	if err != nil {
		t.Fatal(err)
	}
	if err := open.ParseFlags([]string{"--start-daemon", "--", "--persist-operator-token", "--dashboard-port=7878"}); err != nil {
		t.Fatal(err)
	}
	if err := open.ValidateArgs(open.Flags().Args()); err != nil {
		t.Fatalf("serve flags after -- refused: %v", err)
	}
	open, _, _ = serviceLauncherCmd().Find([]string{"open"})
	if err := open.ParseFlags([]string{"stray"}); err != nil {
		t.Fatal(err)
	}
	if err := open.ValidateArgs(open.Flags().Args()); err == nil {
		t.Fatal("a positional argument before -- was accepted")
	}
}

// The keychain token goes only to the default socket, and never to an agent.
func TestKeychainTokenOnlyForTheOperatorOnTheDefaultSocket(t *testing.T) {
	read := 0
	prev := keychainOperatorToken
	keychainOperatorToken = func(string, string) (string, error) { read++; return "tok-from-keychain", nil }
	t.Cleanup(func() { keychainOperatorToken = prev })
	t.Setenv("HOME", t.TempDir())
	t.Setenv(HumanTokenEnvVar, "")

	t.Setenv(agentipc.AgentHintEnvVar, "1")
	useKeychainOperatorToken()
	t.Setenv(agentipc.AgentHintEnvVar, "")
	t.Setenv(agentipc.SocketEnv, "/tmp/someone-elses.sock")
	useKeychainOperatorToken()
	if read != 0 || os.Getenv(HumanTokenEnvVar) != "" {
		t.Fatalf("keychain read %d times; token %q", read, os.Getenv(HumanTokenEnvVar))
	}
	t.Setenv(agentipc.SocketEnv, "")
	useKeychainOperatorToken()
	if read != 1 || os.Getenv(HumanTokenEnvVar) != "tok-from-keychain" {
		t.Fatalf("keychain read %d times; token %q", read, os.Getenv(HumanTokenEnvVar))
	}
}

// A detached agentd starts with the login shell's environment over the
// launcher's own, the launcher's PATH kept as a fallback; when the shell
// cannot be asked it starts anyway, and says so.
func TestLauncherOpenStartsDetachedWithTheLoginShellEnv(t *testing.T) {
	f := newOpenFixture("darwin", nil)
	var asked []string
	f.deps.LoginEnv = func(environ []string) ([]string, error) {
		asked = environ
		return []string{"PATH=/Users/u/.local/bin:/usr/bin", "ANTHROPIC_API_KEY=k", "LANG=en_US.UTF-8"}, nil
	}
	f.deps.Profile = func() (*config.LaunchProfile, error) {
		return &config.LaunchProfile{Env: []config.LaunchEnv{{Key: "LANG", Value: "C.UTF-8"}}}, nil
	}
	var out bytes.Buffer
	if rc := runLauncherOpen(true, f.deps, &out, io.Discard); rc != rcOK {
		t.Fatal(rc)
	}
	if strings.Join(asked, "\n") != "PATH=/home/u/.local/bin:/usr/bin" {
		t.Errorf("login shell asked with %v", asked)
	}
	env := strings.Join(f.servedEnv, "\n")
	for _, want := range []string{"PATH=/Users/u/.local/bin:/usr/bin:/home/u/.local/bin", "ANTHROPIC_API_KEY=k"} {
		if !strings.Contains(env, want) {
			t.Errorf("env lacks %s:\n%s", want, env)
		}
	}
	// The launch profile's explicit variables still win.
	if i, j := strings.LastIndex(env, "LANG=C.UTF-8"), strings.LastIndex(env, "LANG=en_US.UTF-8"); i < j {
		t.Errorf("profile LANG lost:\n%s", env)
	}
	if strings.Contains(out.String(), "without") {
		t.Errorf("out %q", out.String())
	}

	f = newOpenFixture("darwin", nil)
	f.deps.LoginEnv = func([]string) ([]string, error) { return nil, errors.New("/bin/zsh did not finish within 10s") }
	var logged []string
	f.deps.LogNote = func(msg string) { logged = append(logged, msg) }
	out.Reset()
	if rc := runLauncherOpen(true, f.deps, &out, io.Discard); rc != rcOK {
		t.Fatal(rc)
	}
	// macOS: no locale from anywhere, so a UTF-8 one is supplied.
	if strings.Join(f.servedEnv, "\n") != "PATH=/home/u/.local/bin:/usr/bin\n"+launcher.LoginEnvErrorVar+"=/bin/zsh did not finish within 10s\nLANG=en_US.UTF-8" {
		t.Errorf("env %v", f.servedEnv)
	}
	if !strings.Contains(out.String(), "without the login shell's environment: /bin/zsh did not finish") {
		t.Errorf("out %q", out.String())
	}
	if len(logged) != 1 || !strings.Contains(logged[0], "/bin/zsh did not finish") {
		t.Errorf("log %q", logged)
	}
	// Nobody reads a launcher's output: a desktop notification, and agentd
	// is handed the failure to show in Messages.
	if len(f.notified) != 1 || !strings.Contains(f.notified[0], "login shell") {
		t.Errorf("notified %q", f.notified)
	}
	if !slices.Contains(f.servedEnv, launcher.LoginEnvErrorVar+"=/bin/zsh did not finish within 10s") {
		t.Errorf("env %v", f.servedEnv)
	}
}
