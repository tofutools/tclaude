package launcher

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// A desktop launcher starts with the bare environment the desktop gives
// apps: on macOS a PATH of /usr/bin:/bin:/usr/sbin:/sbin and no LANG, no
// variables from the user's shell profile (API keys, proxies, a harness's
// base URL), none of what a terminal has. A daemon started from there
// launches every agent with that environment. So before `service launcher
// open` starts agentd it asks the user's login shell for the environment a
// terminal would have, the way editors resolve a shell environment when
// opened from the Dock: run the shell as an interactive login shell that
// execs `tclaude service launcher shell-env`, which prints its environment
// after ShellEnvMarker.

// ShellEnvMarker starts the line `tclaude service launcher shell-env`
// prints: the marker, then the environment as a JSON array of KEY=VALUE.
// Anything a profile prints before it is skipped.
const ShellEnvMarker = "tclaude-shell-env:"

// ShellEnvTimeout bounds the login shell: a profile that waits for input
// or hangs must not keep agentd from starting. The shell is waited for only
// until it answers; this is the most it gets. Both the launcher and the
// LaunchAgent re-exec use it.
const ShellEnvTimeout = 30 * time.Second

// LoginEnvErrorVar carries a failed login-shell step to the agentd it
// starts, which reports it to the operator and drops the variable.
const LoginEnvErrorVar = "TCLAUDE_AGENTD_LOGIN_ENV_ERROR"

// LoginShell returns the user's login shell: $SHELL when it is an absolute
// path, else the account's shell (dscl on macOS, getent elsewhere), else
// /bin/zsh on macOS and /bin/sh elsewhere.
func LoginShell(ctx context.Context, goos string, environ []string, username string, run Runner) string {
	if sh := envValue(environ, "SHELL"); filepath.IsAbs(sh) {
		return sh
	}
	if username != "" && run != nil {
		if goos == "darwin" {
			// "UserShell: /bin/zsh"
			if out, err := run(ctx, "dscl", ".", "-read", "/Users/"+username, "UserShell"); err == nil {
				if _, sh, ok := strings.Cut(strings.TrimSpace(string(out)), ":"); ok && filepath.IsAbs(strings.TrimSpace(sh)) {
					return strings.TrimSpace(sh)
				}
			}
		} else if out, err := run(ctx, "getent", "passwd", username); err == nil {
			// name:x:uid:gid:gecos:home:shell
			if f := strings.Split(strings.TrimSpace(string(out)), ":"); len(f) >= 7 && filepath.IsAbs(f[6]) {
				return f[6]
			}
		}
	}
	if goos == "darwin" {
		return "/bin/zsh"
	}
	return "/bin/sh"
}

// LoginShellCommand is the argv that asks shell for its environment by
// running dump (normally `tclaude service launcher shell-env`) inside an
// interactive login shell. Interactive, so zsh and bash read ~/.zshrc and
// ~/.bashrc, where most people export their variables. dump is quoted for
// POSIX shells and fish alike, so it must hold no single quote or
// backslash.
func LoginShellCommand(shell string, dump []string) ([]string, error) {
	quoted := make([]string, len(dump))
	for i, a := range dump {
		if a == "" || strings.ContainsAny(a, "'\\") {
			return nil, fmt.Errorf("cannot quote %q for the login shell", a)
		}
		quoted[i] = "'" + a + "'"
	}
	return []string{shell, "-l", "-i", "-c", "exec " + strings.Join(quoted, " ")}, nil
}

// LoginShellEnv runs argv (from LoginShellCommand) with environ, its stdin
// empty and no terminal, and returns the environment the dump printed.
func LoginShellEnv(ctx context.Context, argv, environ []string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, ShellEnvTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = environ
	cmd.Stdin = nil // /dev/null
	var out bytes.Buffer
	cmd.Stdout = &out
	// A profile that starts a background job keeps the output pipe open;
	// do not wait for it past the shell's own exit.
	cmd.WaitDelay = time.Second
	setNewSession(cmd)
	err := cmd.Run()
	// Whatever the profile left running in the shell's session (a `foo &`
	// in ~/.zshrc) has no owner now; real daemons start their own session.
	killSession(cmd)
	if env, perr := ParseShellEnv(out.Bytes()); perr == nil {
		return env, nil
	}
	if ctx.Err() != nil {
		return nil, fmt.Errorf("%s did not finish within %s", argv[0], ShellEnvTimeout)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", argv[0], err)
	}
	return nil, fmt.Errorf("%s printed no environment", argv[0])
}

// ParseShellEnv reads the environment after the last ShellEnvMarker.
func ParseShellEnv(out []byte) ([]string, error) {
	// Anchored at a line start, as PrintShellEnv writes it, so a value
	// holding the marker text cannot be taken for it.
	marker := []byte("\n" + ShellEnvMarker)
	i := bytes.LastIndex(out, marker)
	if i < 0 {
		return nil, errors.New("no environment marker")
	}
	var env []string
	if err := json.NewDecoder(bytes.NewReader(out[i+len(marker):])).Decode(&env); err != nil {
		return nil, err
	}
	if len(env) == 0 {
		return nil, errors.New("empty environment")
	}
	return env, nil
}

// PrintShellEnv writes this process's environment the way ParseShellEnv
// reads it.
func PrintShellEnv(w *os.File) error {
	data, err := json.Marshal(os.Environ())
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "\n%s%s\n", ShellEnvMarker, data)
	return err
}

// shellSessionVars describe the shell that printed the environment, not
// the user's environment, so they are not carried over.
var shellSessionVars = map[string]bool{"_": true, "SHLVL": true, "PWD": true, "OLDPWD": true}

// MergeLoginEnv returns base overlaid with login (the login shell's
// environment). PATH is the login shell's, followed by any directory of
// base's PATH it lacks, so the PATH the launcher was installed with stays
// a fallback.
func MergeLoginEnv(base, login []string) []string {
	out := map[string]string{}
	var order []string
	set := func(kv string) {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return
		}
		if _, seen := out[k]; !seen {
			order = append(order, k)
		}
		out[k] = v
	}
	for _, kv := range base {
		set(kv)
	}
	basePATH := out["PATH"]
	for _, kv := range login {
		if k, _, _ := strings.Cut(kv, "="); shellSessionVars[k] {
			continue
		}
		set(kv)
	}
	out["PATH"] = mergePATH(out["PATH"], basePATH)
	if out["PATH"] == "" {
		delete(out, "PATH")
	}
	env := make([]string, 0, len(order))
	for _, k := range order {
		if v, ok := out[k]; ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

func mergePATH(first, fallback string) string {
	seen := map[string]bool{}
	var dirs []string
	for _, p := range []string{first, fallback} {
		for _, d := range filepath.SplitList(p) {
			if d != "" && !seen[d] {
				seen[d] = true
				dirs = append(dirs, d)
			}
		}
	}
	return strings.Join(dirs, string(os.PathListSeparator))
}

func envValue(environ []string, key string) string {
	for i := len(environ) - 1; i >= 0; i-- {
		if k, v, ok := strings.Cut(environ[i], "="); ok && k == key {
			return v
		}
	}
	return ""
}
