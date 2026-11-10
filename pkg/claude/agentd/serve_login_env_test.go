package agentd

import (
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/tofutools/tclaude/pkg/launcher"
)

type loginEnvFixture struct {
	deps   loginEnvDeps
	asked  int
	execed []string
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
		"LANG=en_US.UTF-8",
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
		if f.asked != 0 || f.execed != nil {
			t.Errorf("%s: asked %d, execed %v", name, f.asked, f.execed)
		}
	}

	// The shell gives nothing: start as is.
	f := newLoginEnvFixture("")
	f.deps.LoginEnv = func([]string) ([]string, error) { return nil, errors.New("timed out") }
	adoptLaunchdLoginEnv(f.deps)
	if f.execed != nil {
		t.Errorf("execed %v", f.execed)
	}
}
