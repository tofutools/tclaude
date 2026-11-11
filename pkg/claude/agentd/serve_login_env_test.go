package agentd

import (
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/launcher"
)

type loginEnvFixture struct {
	deps   loginEnvDeps
	setenv []string
	asked  int
	execed []string
	failed []error
}

func newLoginEnvFixture(plist string) *loginEnvFixture {
	f := &loginEnvFixture{}
	unit := launcher.AutostartPaths("darwin", "/Users/u", "").Unit
	f.deps = loginEnvDeps{
		GOOS:    "darwin",
		Environ: []string{"PATH=/usr/bin:/opt/homebrew/bin", "XPC_SERVICE_NAME=" + launcher.LaunchdLabel, "ANTHROPIC_BASE_URL=http://from-plist"},
		Home:    "/Users/u",
		ReadFile: func(p string) ([]byte, error) {
			if p == unit && plist != "" {
				return []byte(plist), nil
			}
			return nil, os.ErrNotExist
		},
		LoginEnv: func([]string) ([]string, error) {
			f.asked++
			return []string{"PATH=/Users/u/go/bin:/usr/bin", "TMUX_TMPDIR=/Users/u/.tmux", "ANTHROPIC_BASE_URL=http://from-profile"}, nil
		},
		Exec: func(_ string, _, env []string) error {
			f.execed = env
			return nil
		},
		Failed: func(err error) { f.failed = append(f.failed, err) },
		Setenv: func(k, v string) error { f.setenv = append(f.setenv, k+"="+v); return nil },
		Locale: launcher.LocaleSources{
			GOOS:        "darwin",
			AppleLocale: func() (string, error) { return "en_GB@rg=sezzzz", nil },
			Installed:   func() ([]string, error) { return []string{"en_GB.UTF-8", "en_US.UTF-8"}, nil },
		},
		Exe:  "/Users/u/go/bin/tclaude",
		Args: []string{"/Users/u/go/bin/tclaude", "agentd", "serve"},
	}
	return f
}

// launchd starts tclaude's LaunchAgent with the plist's variables only, so
// serve re-executes itself with the login shell's environment: the
// profile's TMUX_TMPDIR and a locale reach agentd and every agent, the
// plist's own variables still win, and its PATH stays as a fallback.
func TestAdoptLaunchdLoginEnv(t *testing.T) {
	plist := "<!-- " + launcher.ManagedMarker + " -->\n<plist><dict><key>EnvironmentVariables</key><dict>" +
		"<key>PATH</key><string>/usr/bin:/opt/homebrew/bin</string>" +
		"<key>ANTHROPIC_BASE_URL</key><string>http://from-plist</string>" +
		"</dict></dict></plist>"
	f := newLoginEnvFixture(plist)
	adoptLaunchdLoginEnv(f.deps)
	if f.execed == nil {
		t.Fatal("did not re-exec")
	}
	env := strings.Join(f.execed, "\n")
	for _, want := range []string{
		"PATH=/Users/u/go/bin:/usr/bin:/opt/homebrew/bin",
		"TMUX_TMPDIR=/Users/u/.tmux",
		"ANTHROPIC_BASE_URL=http://from-plist",
		"LANG=en_GB.UTF-8",
		loginEnvAppliedVar + "=1",
	} {
		if !slices.Contains(f.execed, want) {
			t.Errorf("env lacks %s:\n%s", want, env)
		}
	}
}

func TestAdoptLaunchdLoginEnvOnlyForTheLaunchAgentOnce(t *testing.T) {
	for name, mutate := range map[string]func(*loginEnvDeps){
		"linux":       func(d *loginEnvDeps) { d.GOOS = "linux" },
		"not launchd": func(d *loginEnvDeps) { d.Environ = []string{"PATH=/usr/bin"} },
		"other label": func(d *loginEnvDeps) { d.Environ = []string{"XPC_SERVICE_NAME=com.example.other"} },
		"already applied": func(d *loginEnvDeps) {
			d.Environ = append(d.Environ, loginEnvAppliedVar+"=1")
		},
	} {
		f := newLoginEnvFixture("")
		mutate(&f.deps)
		adoptLaunchdLoginEnv(f.deps)
		if f.asked != 0 || f.execed != nil || f.failed != nil || f.setenv != nil {
			t.Errorf("%s: asked %d, execed %v", name, f.asked, f.execed)
		}
	}

	// The shell gives nothing: start as is.
	f := newLoginEnvFixture("")
	f.deps.LoginEnv = func([]string) ([]string, error) { return nil, errors.New("timed out") }
	adoptLaunchdLoginEnv(f.deps)
	if f.execed != nil || len(f.failed) != 1 {
		t.Errorf("execed %v, failed %v", f.execed, f.failed)
	}
	// Still a UTF-8 locale: without one the tmux client escapes the tab in
	// its list-sessions output and every agent reads as offline.
	if !slices.Equal(f.setenv, []string{"LANG=en_GB.UTF-8"}) {
		t.Errorf("setenv %v", f.setenv)
	}
	// A locale launchd did give is left alone.
	f = newLoginEnvFixture("")
	f.deps.Environ = append(f.deps.Environ, "LC_CTYPE=UTF-8")
	f.deps.LoginEnv = func([]string) ([]string, error) { return nil, errors.New("timed out") }
	adoptLaunchdLoginEnv(f.deps)
	if f.setenv != nil {
		t.Errorf("setenv %v", f.setenv)
	}

	// The exec fails: start as is, and say so.
	f = newLoginEnvFixture("")
	f.deps.Exec = func(string, []string, []string) error { return errors.New("exec format error") }
	adoptLaunchdLoginEnv(f.deps)
	if len(f.failed) != 1 || !strings.Contains(f.failed[0].Error(), "exec format error") || len(f.setenv) != 1 {
		t.Errorf("failed %v, setenv %v", f.failed, f.setenv)
	}
}

// A failed login-shell step reaches the operator as a Messages notice
// once agentd is up, and agents do not inherit the variable carrying it.
func TestReportLoginEnvFailure(t *testing.T) {
	setupTestDB(t)
	t.Setenv(launcher.LoginEnvErrorVar, "/bin/zsh did not finish within 30s")
	reportLoginEnvFailure()
	if v, set := os.LookupEnv(launcher.LoginEnvErrorVar); set {
		t.Errorf("variable left set: %q", v)
	}
	msgs, err := db.ListUnreadAndRecentHumanMessages(10, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || !strings.Contains(msgs[0].Subject, "login shell") ||
		!strings.Contains(msgs[0].Body, "/bin/zsh did not finish within 30s") ||
		!strings.Contains(msgs[0].Body, "time $SHELL -l -i -c true") {
		t.Fatalf("messages %+v", msgs)
	}
	// Nothing to report: no notice.
	reportLoginEnvFailure()
	if msgs, _ := db.ListUnreadAndRecentHumanMessages(10, 10); len(msgs) != 1 {
		t.Errorf("%d messages", len(msgs))
	}
}

// TestLaunchdLoginEnvReExecsBeforeServeStarts pins where the re-exec runs:
// in serve's RunFunc before runServe, which starts the tray (Cocoa), takes
// the singleton lock and opens the sockets and the DB; an exec after any of
// them would carry it broken into the new image. It reads serve.go, as
// the other serve wiring tests do.
func TestLaunchdLoginEnvReExecsBeforeServeStarts(t *testing.T) {
	source, err := os.ReadFile("serve.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	start := strings.Index(text, "\nfunc serveCmd(")
	if start < 0 {
		t.Fatal("serveCmd has moved")
	}
	body := text[start+1:]
	if end := strings.Index(body, "\nfunc "); end >= 0 {
		body = body[:end]
	}
	adopt := strings.Index(body, "adoptLaunchdLoginEnv(")
	serve := strings.Index(body, "runServe(p)")
	if adopt < 0 || serve < 0 || adopt > serve {
		t.Fatalf("adoptLaunchdLoginEnv must be called before runServe in serveCmd (at %d, runServe at %d)", adopt, serve)
	}
	if n := strings.Count(text, "adoptLaunchdLoginEnv("); n != 1 {
		t.Errorf("serve.go calls adoptLaunchdLoginEnv %d times; only serveCmd may, before runServe", n)
	}
}

// The update's fresh env keeps what agentd was deliberately configured
// with, on Linux as on macOS: the unit's own variables and the launch
// profile's. Nothing else of agentd's env comes back, and PATH stays the
// shell's.
func TestSelfUpdateToolEnvKeepsTheOwnLaunchEnv(t *testing.T) {
	fresh := []string{"PATH=/fresh/bin", "HOME=/h"}
	environ := []string{"PATH=/stale/bin", "GOROOT=/stale/go", "GOPRIVATE=corp.example", "GIT_SSH_COMMAND=ssh -i k", "ANTHROPIC_BASE_URL=http://p"}
	info := launcher.Info{Kind: "systemd-user", Env: []launcher.EnvVar{{Key: "GOPRIVATE", Value: launcher.Redacted, From: "/home/u/.config/tclaude/agentd.env"}, {Key: "PATH", Value: "/stale/bin"}}}
	profile := &config.LaunchProfile{Env: []config.LaunchEnv{{Key: "ANTHROPIC_BASE_URL", Value: "http://p"}}, InheritEnv: []string{"GIT_SSH_COMMAND"}}
	got := withOwnLaunchEnv(fresh, environ, info, profile)
	want := []string{"PATH=/fresh/bin", "HOME=/h", "GOPRIVATE=corp.example", "GIT_SSH_COMMAND=ssh -i k", "ANTHROPIC_BASE_URL=http://p"}
	if !slices.Equal(got, want) {
		t.Errorf("got  %q\nwant %q", got, want)
	}
	if got := withOwnLaunchEnv(fresh, environ, launcher.Info{}, nil); !slices.Equal(got, fresh) {
		t.Errorf("nothing configured: got %q", got)
	}
}
