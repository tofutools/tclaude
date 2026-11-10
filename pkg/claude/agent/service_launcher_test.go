package agent

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

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

func TestLauncherOpenStartsDetachedWithTheLaunchProfile(t *testing.T) {
	// A hand-made unit is not tclaude's to start.
	f := newOpenFixture("linux", []byte("[Service]\nExecStart=/mine\n"))
	f.deps.Profile = func() (*config.LaunchProfile, error) {
		return &config.LaunchProfile{Flags: []string{"--no-tray"}, Env: []config.LaunchEnv{{Key: "LANG", Value: "C.UTF-8"}}}, nil
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
