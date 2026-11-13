package agentd

import (
	"context"
	"log/slog"
	"os/user"
	"runtime"
	"slices"
	"strings"
	"time"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/launcher"
)

// A restart tclaude starts itself (the dashboard's or the CLI's Restart, a
// relaunch, the re-exec after a self-update) behaves like a fresh start:
// the restarted daemon gets the environment a new terminal would have, not
// the one this process started with, which may predate a toolchain upgrade
// or a profile change ("compile: version go1.27.3 does not match go tool
// version go1.27.4"). Agents and their panes take their environment from
// agentd (spawnEnvWithoutOperatorToken, BuildEnvExports), so new ones see
// it too.
//
// A supervisor restart already starts fresh: launchd's re-runs
// adoptLaunchdLoginEnv (the plist has no loginEnvAppliedVar), and a systemd
// unit starts with the unit's environment. Under systemd that is also what
// an in-place restart keeps: a fresh start of the unit never asks the login
// shell, so an in-place restart must not either, or it would run with an
// environment the unit's own restarts do not give it.

// terminalLoginEnv asks the user's login shell, seeded from environ the way
// a terminal seeds a new shell, for the environment a new terminal would
// have (launcher.TerminalEnv, the self-update tools' probe). Tests swap it.
var terminalLoginEnv = func(ctx context.Context, environ []string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, launcher.ShellEnvTimeout+5*time.Second)
	defer cancel()
	name := ""
	if u, err := user.Current(); err == nil {
		name = u.Username
	}
	// The dump is a tclaude subcommand; the standalone tclaude-agentd
	// binary has none, so name tclaude itself.
	return launcher.TerminalEnv(ctx, runtime.GOOS, name, environ,
		[]string{clcommon.SelfTclaudePath(), "service", "launcher", "shell-env"}, launcher.ExecRunner)
}

// restartKeepsEnv are the variables of how and where this daemon was
// launched rather than of the user's profile: the launchd job's, and the
// terminal, tmux pane or SSH session it runs in. A restart is the same
// launch, so they stay; launcher detection reads some of them (TMUX,
// SHLVL, XPC_SERVICE_NAME).
var restartKeepsEnv = map[string]bool{
	"XPC_SERVICE_NAME": true, "XPC_FLAGS": true, "LaunchInstanceID": true, "__CFBundleIdentifier": true,
	"TMUX": true, "TMUX_PANE": true, "STY": true, "WINDOW": true, "WINDOWID": true,
	"TERM_PROGRAM": true, "TERM_PROGRAM_VERSION": true, "TERM_SESSION_ID": true, "ITERM_SESSION_ID": true,
	"COLORTERM": true, "SHLVL": true, "XAUTHORITY": true,
	"SSH_CONNECTION": true, "SSH_CLIENT": true, "SSH_TTY": true, "SSH_AGENT_PID": true,
}

// restartKeepsVar reports a variable of environ the restart keeps over the
// login shell's: restartKeepsEnv, and tclaude's own (TCLAUDE_*: markers,
// configuration, the operator token the launcher handed over, which agents
// never inherit: spawnEnvWithoutOperatorToken and BuildEnvExports strip it).
func restartKeepsVar(key string) bool {
	return restartKeepsEnv[key] || strings.HasPrefix(key, "TCLAUDE_")
}

// restartRefreshesEnv reports whether a restart of a daemon launched as
// info asks the login shell for its environment: every launcher but a
// systemd unit.
func restartRefreshesEnv(info launcher.Info) bool {
	return info.Kind != launcher.KindSystemdUser && info.Kind != launcher.KindSystemdSystem
}

// restartEnviron returns the environment to restart a daemon launched as
// info, with environ (launchEnviron), in: the login shell's (a new
// terminal's), with the launch's own variables over it (withOwnLaunchEnv's
// unit and profile ones, restartKeepsVar's) and environ's PATH kept last as
// a fallback. Under systemd, or when the shell does not answer, it is
// environ; the failure is logged and handed to the restarted daemon, which
// tells the operator (reportLoginEnvFailure).
func restartEnviron(ctx context.Context, environ []string, info launcher.Info, profile *config.LaunchProfile) []string {
	environ = slices.Clip(environ)
	if !restartRefreshesEnv(info) {
		return environ
	}
	fresh, err := terminalLoginEnv(ctx, environ)
	if err == nil && len(fresh) == 0 {
		err = errEmptyLoginEnv
	}
	if err != nil {
		slog.Warn("agentd restart: could not get a fresh environment from the login shell; restarting with the environment agentd started with", "error", err)
		return append(environ, launcher.LoginEnvErrorVar+"=restarting: "+err.Error())
	}
	env := withOwnLaunchEnv(fresh, environ, info, profile)
	var keep []string
	for _, kv := range environ {
		if k, _, ok := strings.Cut(kv, "="); ok && restartKeepsVar(k) {
			keep = append(keep, kv)
		}
	}
	if envLookup(environ, "XPC_SERVICE_NAME") == launcher.LaunchdLabel {
		// The LaunchAgent need not re-exec for the login environment
		// again (adoptLaunchdLoginEnv): it has it.
		keep = append(keep, loginEnvAppliedVar+"=1")
	}
	env = overlayEnv(env, keep)
	if before, after := envLookup(environ, "PATH"), envLookup(env, "PATH"); before != after {
		slog.Info("agentd restart: refreshed the environment from the login shell", "path_before", before, "path_after", after)
	} else {
		slog.Info("agentd restart: refreshed the environment from the login shell; PATH unchanged")
	}
	return env
}

// overlayEnv sets each KEY=VALUE of over in env, in place of env's own.
func overlayEnv(env, over []string) []string {
	if len(over) == 0 {
		return env
	}
	keys := map[string]bool{}
	for _, kv := range over {
		k, _, _ := strings.Cut(kv, "=")
		keys[k] = true
	}
	out := make([]string, 0, len(env)+len(over))
	for _, kv := range env {
		if k, _, _ := strings.Cut(kv, "="); !keys[k] {
			out = append(out, kv)
		}
	}
	return append(out, over...)
}

type loginEnvError string

func (e loginEnvError) Error() string { return string(e) }

const errEmptyLoginEnv = loginEnvError("the login shell printed an empty environment")

// daemonRestartEnviron is restartEnviron for this process: its launcher
// and launch profile.
func daemonRestartEnviron(environ []string) []string {
	cfg, _ := config.Load()
	return restartEnviron(context.Background(), environ, currentLauncher(false), cfg.ServiceLaunch())
}
