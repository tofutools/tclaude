package agentd

import (
	"context"
	"log/slog"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/launcher"
)

// loginEnvAppliedVar marks a serve process that already re-executed itself
// with the login shell's environment, so it does not loop.
const loginEnvAppliedVar = "TCLAUDE_AGENTD_LOGIN_ENV"

// loginEnvDeps is what adoptLaunchdLoginEnv reaches outside the process;
// tests swap it.
type loginEnvDeps struct {
	GOOS     string
	Environ  []string
	Home     string
	Username string
	ReadFile func(string) ([]byte, error)
	// LoginEnv asks the login shell for its environment.
	LoginEnv func(environ []string) ([]string, error)
	Exec     func(argv0 string, argv, env []string) error
	Exe      string
	Args     []string
}

// adoptLaunchdLoginEnv re-executes agentd, in place, with the user's login
// shell environment when launchd started it as tclaude's LaunchAgent.
//
// launchd gives the unit only what its plist names (PATH and a few launchd
// variables): no locale, no TMUX_TMPDIR, none of the shell profile. Every
// agent inherits that, and the tmux client agentd runs to see its agents
// may not reach the server a terminal started. The plist could carry the
// whole login environment, but it would then hold the profile's secrets in
// a file and go stale when the profile changes; asking the shell at each
// start avoids both. The plist's own variables still win over the shell's
// (its PATH is kept after the shell's, as a fallback). Exec keeps the PID,
// so launchd still tracks the process. Any failure starts agentd as before.
func adoptLaunchdLoginEnv(d loginEnvDeps) {
	if d.GOOS != "darwin" || envLookup(d.Environ, "XPC_SERVICE_NAME") != launcher.LaunchdLabel ||
		envLookup(d.Environ, loginEnvAppliedVar) != "" {
		return
	}
	login, err := d.LoginEnv(d.Environ)
	if err != nil {
		slog.Warn("agentd: starting without the login shell's environment", "error", err)
		return
	}
	env := launcher.MergeLoginEnv(d.Environ, login)
	if data, err := d.ReadFile(launcher.AutostartPaths("darwin", d.Home, "").Unit); err == nil {
		var own []string
		for _, e := range launcher.LaunchdUnitEnv(data) {
			if e.Key != "PATH" {
				own = append(own, e.Key+"="+e.Value)
			}
		}
		env = launcher.MergeLoginEnv(env, own)
	}
	env = launcher.WithDefaultLocale(env)
	env = append(env, loginEnvAppliedVar+"=1")
	if err := d.Exec(d.Exe, d.Args, env); err != nil {
		slog.Warn("agentd: could not restart with the login shell's environment", "error", err)
	}
}

func productionLoginEnvDeps() loginEnvDeps {
	home, _ := os.UserHomeDir()
	name := ""
	if u, err := user.Current(); err == nil {
		name = u.Username
	}
	exe, _ := os.Executable()
	return loginEnvDeps{
		GOOS: runtime.GOOS, Environ: os.Environ(), Home: home, Username: name,
		ReadFile: os.ReadFile,
		LoginEnv: func(environ []string) ([]string, error) {
			ctx, cancel := context.WithTimeout(context.Background(), launcher.ShellEnvTimeout+5*time.Second)
			defer cancel()
			shell := launcher.LoginShell(ctx, "darwin", environ, name, launcher.ExecRunner)
			// The dump is a tclaude subcommand; the standalone
			// tclaude-agentd binary has none, so name tclaude itself.
			argv, err := launcher.LoginShellCommand(shell, []string{clcommon.SelfTclaudePath(), "service", "launcher", "shell-env"})
			if err != nil {
				return nil, err
			}
			return launcher.LoginShellEnv(ctx, argv, environ)
		},
		Exec: syscall.Exec,
		Exe:  filepath.Clean(exe),
		Args: os.Args,
	}
}

func envLookup(environ []string, key string) string {
	for i := len(environ) - 1; i >= 0; i-- {
		if k, v, ok := strings.Cut(environ[i], "="); ok && k == key {
			return v
		}
	}
	return ""
}
