// Package launcher detects how the running agentd process was started
// (a systemd unit, a launchd job, a tmux pane, an interactive shell, or
// something else) and knows how to restart it through that supervisor, so a
// restart keeps the supervisor's own arguments and environment.
//
// Every external command goes through an injected Runner; tests never reach
// the real systemctl or launchctl.
package launcher

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Kinds of launcher.
const (
	KindSystemdUser   = "systemd-user"
	KindSystemdSystem = "systemd-system"
	KindLaunchd       = "launchd"
	KindTmux          = "tmux"
	KindShell         = "shell"
	KindUnknown       = "unknown"
)

// Restart methods.
const (
	// RestartSupervisor asks the supervisor (systemctl / launchctl) to
	// restart the service, so the unit's own argv and environment apply.
	RestartSupervisor = "supervisor"
	// RestartExec stops gracefully and re-executes in place with the same
	// argv and environment (same PID).
	RestartExec = "exec"
	// RestartExit stops gracefully and exits with RestartExitCode, which the
	// unit's Restart= policy answers by starting it again through its own
	// ExecStart (a wrapper re-resolves its arguments). Used where tclaude may
	// not restart the unit itself (a system unit, not root).
	RestartExit = "exit"
)

// ManagedMarker is written into every unit file or plist tclaude writes. A
// unit carrying it is one tclaude may rewrite or remove.
const ManagedMarker = "Managed by tclaude"

// EnvVar is one observed environment variable. Value is "<redacted>" for
// secret-looking keys.
type EnvVar struct {
	Key   string `json:"key"`
	Value string `json:"value"`
	// From is the EnvironmentFile the variable came from; empty for the
	// unit's own Environment= / EnvironmentVariables.
	From string `json:"from,omitempty"`
}

// Info is the read-only launcher status.
type Info struct {
	Kind string `json:"kind"`
	// Unit is the systemd unit name (systemd kinds only).
	Unit string `json:"unit,omitempty"`
	// Label is the launchd job label; Domain its launchctl domain target
	// (gui/<uid> or system).
	Label  string `json:"label,omitempty"`
	Domain string `json:"domain,omitempty"`
	// UnitPath is the unit file or plist the supervisor loaded, when known.
	UnitPath string `json:"unit_path,omitempty"`
	// Argv is the supervisor's configured command line (ExecStart /
	// ProgramArguments); ProcessArgv is this process's own os.Args.
	Argv        []string `json:"argv,omitempty"`
	ProcessArgv []string `json:"process_argv"`
	// Env is the environment the supervisor sets (Environment= / the
	// plist's EnvironmentVariables), secrets redacted.
	Env              []EnvVar `json:"env,omitempty"`
	EnvironmentFiles []string `json:"environment_files,omitempty"`
	// RestartPolicy is systemd's Restart= or launchd's KeepAlive.
	RestartPolicy string `json:"restart_policy,omitempty"`
	KillMode      string `json:"kill_mode,omitempty"`
	// ManagedByTclaude reports whether the unit file carries ManagedMarker.
	ManagedByTclaude bool `json:"managed_by_tclaude"`
	// Wrapped: the unit's command is not `tclaude agentd serve` itself (a
	// wrapper script that execs it). Argv is the wrapper, ProcessArgv what
	// agentd actually runs with.
	Wrapped bool `json:"wrapped"`
	// Rewritable is false when a supervisor unit tclaude did not write owns
	// agentd: tclaude never offers to rewrite it.
	Rewritable bool `json:"rewritable"`
	// UnitCgroup is the unit's cgroup (systemd).
	UnitCgroup string `json:"unit_cgroup,omitempty"`
	// Tmux is where the tclaude tmux server runs, when it could be probed.
	Tmux *TmuxPlacement `json:"tmux,omitempty"`
	// RestartMethod is how a daemon restart will be carried out;
	// RestartExitCode is the exit status of RestartExit.
	RestartMethod   string    `json:"restart_method"`
	RestartExitCode int       `json:"restart_exit_code"`
	Warnings        []string  `json:"warnings"`
	DetectedAt      time.Time `json:"detected_at"`

	// rawArgv and rawProcessArgv are the unredacted command lines, for
	// rewriting a unit; they never leave the process.
	rawArgv, rawProcessArgv  []string
	successExit, preventExit map[int]bool
}

// TmuxPlacement is the tclaude tmux server's cgroup relative to agentd's
// unit: whether stopping the unit can take the server and the agents down.
type TmuxPlacement struct {
	Running bool   `json:"running"`
	PID     int    `json:"pid,omitempty"`
	Cgroup  string `json:"cgroup,omitempty"`
	// InUnit: the server is inside agentd's unit cgroup.
	InUnit bool `json:"in_unit"`
}

// RawArgv returns the unredacted command line agentd is configured with:
// the supervisor's when it runs `agentd serve` directly, otherwise (no
// supervisor, or a wrapper) the process's own.
func (i Info) RawArgv() []string {
	if len(i.rawArgv) > 0 && !i.Wrapped {
		return append([]string(nil), i.rawArgv...)
	}
	return append([]string(nil), i.rawProcessArgv...)
}

// IsServeArgv reports whether argv runs agentd directly:
// `<tclaude> agentd serve ...` or `<tclaude-agentd> serve ...`.
func IsServeArgv(argv []string) bool {
	return len(argv) >= 3 && argv[1] == "agentd" && argv[2] == "serve" ||
		len(argv) >= 2 && filepath.Base(argv[0]) == "tclaude-agentd" && argv[1] == "serve"
}

// Supervised reports whether a service manager owns the process.
func (i Info) Supervised() bool {
	return i.Kind == KindSystemdUser || i.Kind == KindSystemdSystem || i.Kind == KindLaunchd
}

// Runner runs an external command and returns its stdout.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// ExecRunner is the production Runner.
func ExecRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}

// Env is the process context detection reads. Production uses FromProcess.
type Env struct {
	GOOS     string
	PID      int
	UID      int
	EUID     int
	Args     []string
	Getenv   func(string) string
	ReadFile func(string) ([]byte, error)
	// StdinTTY reports whether stdin is a terminal.
	StdinTTY bool
	// TmuxServerPID probes the tclaude tmux server: its pid, or 0 when none
	// runs. nil, or an error, means it cannot tell.
	TmuxServerPID func() (int, error)
}

// FromProcess returns the Env of the running process.
func FromProcess(goos string) Env {
	tty := false
	if fi, err := os.Stdin.Stat(); err == nil {
		tty = fi.Mode()&os.ModeCharDevice != 0
	}
	return Env{
		GOOS: goos, PID: os.Getpid(), UID: os.Getuid(), EUID: os.Geteuid(),
		Args: append([]string(nil), os.Args...), Getenv: os.Getenv, ReadFile: os.ReadFile,
		StdinTTY: tty,
	}
}

var (
	unitNameRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9:_.\\@-]{0,199}\.service$`)
	labelRe    = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,199}$`)
	secretRe   = regexp.MustCompile(`(?i)(TOKEN|SECRET|PASSWORD|PASSWD|PASS|KEY|CREDENTIAL|AUTH|COOKIE|SESSION|PRIVATE)`)
	urlCredRe  = regexp.MustCompile(`://[^/\s:@]+:[^/\s@]+@`)
)

// Redacted is the value shown for a secret-looking environment variable.
const Redacted = "<redacted>"

// redactEnv returns key/value with the value hidden when the key looks
// secret, or the value carries credentials in a URL (user:pass@host).
func redactEnv(key, value string) EnvVar {
	if secretRe.MatchString(key) || urlCredRe.MatchString(value) {
		return EnvVar{Key: key, Value: Redacted}
	}
	return EnvVar{Key: key, Value: value}
}

// secretFlagRe matches a flag whose value is secret-looking.
var secretFlagRe = regexp.MustCompile(`(?i)^--?[a-z0-9-]*(token|secret|password|passwd|key|credential|auth|cookie|passphrase)[a-z0-9-]*$`)

// RedactArgv hides secret-looking values in a command line: KEY=VALUE
// tokens (as `env KEY=VALUE cmd` or `sh -c "..."` would carry), secret
// flags (--token=x, --token x) and credentials in URLs.
func RedactArgv(argv []string) []string {
	out := make([]string, len(argv))
	for i, a := range argv {
		out[i] = a
		if i > 0 && secretFlagRe.MatchString(argv[i-1]) && !strings.HasPrefix(a, "-") {
			out[i] = Redacted
			continue
		}
		if flag, _, ok := strings.Cut(a, "="); ok && strings.HasPrefix(flag, "-") && secretFlagRe.MatchString(flag) {
			out[i] = flag + "=" + Redacted
			continue
		}
		out[i] = redactAssignments(a)
	}
	return out
}

var assignRe = regexp.MustCompile(`(^|[\s;'"])([A-Za-z_][A-Za-z0-9_]*)=([^\s;'"]*)`)

// redactAssignments redacts KEY=VALUE pairs with a secret-looking key, also
// inside a shell string, and URL credentials anywhere.
func redactAssignments(s string) string {
	s = assignRe.ReplaceAllStringFunc(s, func(m string) string {
		sub := assignRe.FindStringSubmatch(m)
		if secretRe.MatchString(sub[2]) {
			return sub[1] + sub[2] + "=" + Redacted
		}
		return m
	})
	return urlCredRe.ReplaceAllString(s, "://"+Redacted+"@")
}

// Detect reports how the process described by env was launched.
func Detect(ctx context.Context, env Env, run Runner) Info {
	info := Info{Kind: KindUnknown, ProcessArgv: env.Args, Warnings: []string{}, DetectedAt: time.Now().UTC()}
	switch env.GOOS {
	case "linux":
		if detectSystemd(ctx, env, run, &info) {
			break
		}
		detectInteractive(env, &info)
	case "darwin":
		if detectLaunchd(ctx, env, run, &info) {
			break
		}
		detectInteractive(env, &info)
	default:
		detectInteractive(env, &info)
	}
	if info.Supervised() {
		info.Wrapped = len(info.Argv) > 0 && !IsServeArgv(info.Argv)
		if info.Wrapped {
			info.Warnings = append(info.Warnings, "The unit runs a wrapper, not `tclaude agentd serve` itself; restarts go through the unit so the wrapper resolves its arguments again. The arguments agentd runs with now are process_argv.")
		}
		if !info.ManagedByTclaude {
			info.Warnings = append(info.Warnings, "tclaude did not write this unit, so it never rewrites it; edit it yourself.")
		}
	}
	info.Rewritable = !info.Supervised() || info.ManagedByTclaude
	if info.UnitCgroup != "" {
		info.Tmux = probeTmux(env, info.UnitCgroup)
	}
	info.RestartMethod, info.RestartExitCode = restartMethod(env, &info)
	info.rawArgv, info.rawProcessArgv = info.Argv, info.ProcessArgv
	info.Argv = RedactArgv(info.Argv)
	info.ProcessArgv = RedactArgv(info.ProcessArgv)
	return info
}

func detectInteractive(env Env, info *Info) {
	switch {
	case env.Getenv("TMUX") != "":
		info.Kind = KindTmux
	case env.StdinTTY || env.Getenv("TERM") != "" && env.Getenv("SHLVL") != "":
		info.Kind = KindShell
	default:
		info.Kind = KindUnknown
	}
}

// probeTmux places the tclaude tmux server relative to unitCgroup, or
// returns nil when it cannot tell.
func probeTmux(env Env, unitCgroup string) *TmuxPlacement {
	if env.TmuxServerPID == nil {
		return nil
	}
	pid, err := env.TmuxServerPID()
	if err != nil {
		return nil
	}
	if pid <= 0 {
		return &TmuxPlacement{}
	}
	raw, err := env.ReadFile("/proc/" + strconv.Itoa(pid) + "/cgroup")
	if err != nil {
		return nil
	}
	cg := cgroupPath(string(raw))
	if cg == "" {
		return nil
	}
	return &TmuxPlacement{Running: true, PID: pid, Cgroup: cg,
		InUnit: cg == unitCgroup || strings.HasPrefix(cg, unitCgroup+"/")}
}

// restartKillsTmux reports whether stopping the unit would also stop the
// tmux server (and so every agent): it runs in the unit's cgroup and the
// KillMode reaches it. With no server running nothing can be lost; when the
// server cannot be placed, KillMode alone decides, conservatively.
func restartKillsTmux(info *Info) bool {
	modeKills := info.KillMode == "" || info.KillMode == "control-group" || info.KillMode == "mixed"
	if info.Tmux == nil {
		return modeKills
	}
	return info.Tmux.Running && info.Tmux.InUnit && modeKills
}

// exitRestartCode returns an exit status the unit's Restart= policy answers
// by starting it again, or ok=false.
func exitRestartCode(info *Info) (int, bool) {
	switch info.RestartPolicy {
	case "always", "on-success":
		if !info.preventExit[0] {
			return 0, true
		}
	case "on-failure":
		for _, code := range []int{1, 2, 3} {
			if !info.successExit[code] && !info.preventExit[code] {
				return code, true
			}
		}
	}
	return 0, false
}

func restartMethod(env Env, info *Info) (string, int) {
	switch info.Kind {
	case KindSystemdUser, KindSystemdSystem:
		if restartKillsTmux(info) {
			why := "The unit uses KillMode=" + info.KillMode + " and"
			if info.Tmux != nil {
				why += " the tclaude tmux server runs inside the unit (" + info.Tmux.Cgroup + "):"
			} else {
				why += " the tclaude tmux server could not be placed:"
			}
			info.Warnings = append(info.Warnings, why+" a unit restart would also stop it and every agent, so tclaude restarts in place instead. Set KillMode=process, or run tmux in its own unit.")
			return RestartExec, 0
		}
		if info.Kind == KindSystemdUser || env.EUID == 0 {
			return RestartSupervisor, 0
		}
		if code, ok := exitRestartCode(info); ok {
			info.Warnings = append(info.Warnings, fmt.Sprintf("System unit and agentd is not root: tclaude restarts by exiting (status %d) and lets Restart=%s start the unit again.", code, info.RestartPolicy))
			return RestartExit, code
		}
		info.Warnings = append(info.Warnings, "System unit and agentd is not root, and Restart="+info.RestartPolicy+" would not start it again after an exit: tclaude restarts in place with the same arguments.")
		return RestartExec, 0
	case KindLaunchd:
		return RestartSupervisor, 0
	}
	return RestartExec, 0
}

// RestartCommand returns the supervisor command that restarts info's
// service, or ok=false when the restart method is not RestartSupervisor.
// The names in it are validated, never free text.
func RestartCommand(info Info) (name string, args []string, ok bool) {
	if info.RestartMethod != RestartSupervisor {
		return "", nil, false
	}
	switch info.Kind {
	case KindSystemdUser:
		if unitNameRe.MatchString(info.Unit) {
			return "systemctl", []string{"--user", "--no-block", "restart", info.Unit}, true
		}
	case KindSystemdSystem:
		if unitNameRe.MatchString(info.Unit) {
			return "systemctl", []string{"--no-block", "restart", info.Unit}, true
		}
	case KindLaunchd:
		if labelRe.MatchString(info.Label) && validDomain(info.Domain) {
			return "launchctl", []string{"kickstart", "-k", info.Domain + "/" + info.Label}, true
		}
	}
	return "", nil, false
}

func validDomain(d string) bool {
	if d == "system" {
		return true
	}
	rest, ok := strings.CutPrefix(d, "gui/")
	if !ok {
		rest, ok = strings.CutPrefix(d, "user/")
	}
	return ok && rest != "" && strings.Trim(rest, "0123456789") == ""
}
