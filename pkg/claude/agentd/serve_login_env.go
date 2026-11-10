package agentd

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
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
	// Failed is told when agentd starts without the login environment.
	Failed func(error)
	Exe    string
	Args   []string
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
		d.Failed(err)
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
		d.Failed(fmt.Errorf("restarting with it: %w", err))
	}
}

// loginEnvFailed hands a failed login-shell step to reportLoginEnvFailure,
// which tells the operator once the daemon is up.
func loginEnvFailed(err error) {
	slog.Error("agentd: starting without the login shell's environment", "error", err)
	_ = os.Setenv(launcher.LoginEnvErrorVar, err.Error())
}

// reportLoginEnvFailure tells the operator, in the dashboard's Messages,
// that agentd started without the login shell's environment, whether the
// LaunchAgent re-exec or the app launcher found it so. agentd still starts,
// so the dashboard is there to say it. The variable is dropped so agents
// do not inherit it.
func reportLoginEnvFailure() {
	msg := os.Getenv(launcher.LoginEnvErrorVar)
	if msg == "" {
		return
	}
	_ = os.Unsetenv(launcher.LoginEnvErrorVar)
	slog.Error("agentd: running without the login shell's environment", "error", msg)
	if _, err := db.InsertHumanMessage(loginEnvFailureMessage(msg)); err != nil {
		slog.Warn("agentd: could not post the login shell notice", "error", err)
	}
}

func loginEnvFailureMessage(msg string) *db.HumanMessage {
	return &db.HumanMessage{
		Subject: "agentd started without your login shell's environment",
		Body: fmt.Sprintf("agentd asked your login shell for its environment (PATH, locale, TMUX_TMPDIR, your profile's variables), "+
			"and it did not answer within %s: %s.\n\n"+
			"agentd started anyway, with the bare environment it was given. Agents may not start, or may show offline.\n\n"+
			"To check, time your shell's startup in a terminal: `time $SHELL -l -i -c true`. It should finish in a few seconds, "+
			"without asking anything. Then see ~/.tclaude/data/output.log, and ~/.tclaude/data/launcher-agentd.log when the app launcher started agentd. "+
			"Restart agentd once the shell answers quickly.", launcher.ShellEnvTimeout, msg),
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
		Exec:   syscall.Exec,
		Failed: loginEnvFailed,
		Exe:    filepath.Clean(exe),
		Args:   os.Args,
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
