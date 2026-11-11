package launcher

import (
	"context"
	"strings"
)

// A long-running agentd keeps the environment it started with, and so
// does every restart (the relaunch re-execs with it). A toolchain upgraded
// since then (Homebrew moving Go from one Cellar directory to the next)
// leaves that environment pointing at the old install: a GOROOT, GOTOOLDIR
// or PATH entry the profile captured then. A build agentd runs with it
// mixes toolchains ("compile: version go1.27.3 does not match go tool
// version go1.27.4") although the same command works in a new terminal.
// TerminalEnv asks the login shell again, the way a new terminal would.
//
// The shell must not start from agentd's environment: it would inherit the
// stale variables and print them back. It starts from SessionSeed, the
// little a terminal app gives a new shell.

// sessionSeedVars name who and where the user is: what a terminal app
// passes to a new shell before any profile runs.
var sessionSeedVars = map[string]bool{
	"HOME": true, "USER": true, "LOGNAME": true, "SHELL": true, "TMPDIR": true,
	"LANG": true, "TERM": true, "__CF_USER_TEXT_ENCODING": true,
	"SSH_AUTH_SOCK": true, "DISPLAY": true, "WAYLAND_DISPLAY": true,
	"XDG_RUNTIME_DIR": true, "DBUS_SESSION_BUS_ADDRESS": true,
}

// seedPrefix keeps the locale, the XDG base directories and the proxy
// settings: on Linux a terminal gets these from the desktop or PAM
// session (/etc/environment, environment.d), which a login shell does not
// read again, and they do not go stale with a toolchain.
func seedPrefix(k string) bool {
	if strings.HasPrefix(k, "LC_") || strings.HasPrefix(k, "XDG_") {
		return true
	}
	switch strings.ToLower(k) {
	case "http_proxy", "https_proxy", "no_proxy", "all_proxy", "ftp_proxy":
		return true
	}
	return false
}

// SystemPATH is the PATH a new login session starts with before the
// profile adds to it: macOS's launchd default, and the usual Linux one.
func SystemPATH(goos string) string {
	if goos == "darwin" {
		return "/usr/bin:/bin:/usr/sbin:/sbin"
	}
	return "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
}

// SessionSeed returns the part of environ a new terminal's shell starts
// with (sessionSeedVars and seedPrefix), with the system PATH.
func SessionSeed(goos string, environ []string) []string {
	seed := []string{"PATH=" + SystemPATH(goos)}
	for _, kv := range environ {
		k, _, ok := strings.Cut(kv, "=")
		if ok && (sessionSeedVars[k] || seedPrefix(k)) {
			seed = append(seed, kv)
		}
	}
	return seed
}

// TerminalEnv returns the environment a new terminal would give a command:
// the user's login shell, started from SessionSeed(environ), asked for its
// environment by dump (normally `tclaude service launcher shell-env`).
// environ's PATH stays last, as a fallback; nothing else of environ is
// kept. It waits for the shell at most ShellEnvTimeout.
func TerminalEnv(ctx context.Context, goos, username string, environ, dump []string, run Runner) ([]string, error) {
	seed := SessionSeed(goos, environ)
	shell := LoginShell(ctx, goos, seed, username, run)
	argv, err := LoginShellCommand(shell, dump)
	if err != nil {
		return nil, err
	}
	login, err := LoginShellEnv(ctx, argv, seed)
	if err != nil {
		return nil, err
	}
	return MergeTerminalEnv(seed, login, environ), nil
}

// MergeTerminalEnv is TerminalEnv's merge: login over seed, with
// environ's PATH directories appended to the login shell's.
func MergeTerminalEnv(seed, login, environ []string) []string {
	env := MergeLoginEnv(seed, login)
	path := mergePATH(envValue(env, "PATH"), envValue(environ, "PATH"))
	out := env[:0]
	for _, kv := range env {
		if !strings.HasPrefix(kv, "PATH=") {
			out = append(out, kv)
		}
	}
	if path != "" {
		out = append(out, "PATH="+path)
	}
	return out
}
