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
	"os"
	"os/exec"
	"regexp"
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
)

// ManagedMarker is written into every unit file or plist tclaude writes. A
// unit carrying it is one tclaude may rewrite or remove.
const ManagedMarker = "Managed by tclaude"

// EnvVar is one observed environment variable. Value is "<redacted>" for
// secret-looking keys.
type EnvVar struct {
	Key   string `json:"key"`
	Value string `json:"value"`
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
	// RestartMethod is how a daemon restart will be carried out.
	RestartMethod string    `json:"restart_method"`
	Warnings      []string  `json:"warnings"`
	DetectedAt    time.Time `json:"detected_at"`

	// rawArgv and rawProcessArgv are the unredacted command lines, for
	// rewriting a unit; they never leave the process.
	rawArgv, rawProcessArgv []string
}

// RawArgv returns the unredacted supervisor command line, or the process's
// own when no supervisor reported one.
func (i Info) RawArgv() []string {
	if len(i.rawArgv) > 0 {
		return append([]string(nil), i.rawArgv...)
	}
	return append([]string(nil), i.rawProcessArgv...)
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
	info.RestartMethod = restartMethod(env, &info)
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

func restartMethod(env Env, info *Info) string {
	switch info.Kind {
	case KindSystemdUser:
		if killsTmux(info.KillMode) {
			info.Warnings = append(info.Warnings, "The unit uses KillMode="+info.KillMode+": a unit restart would also stop the tmux server and every agent, so tclaude restarts in place instead. Set KillMode=process.")
			return RestartExec
		}
		return RestartSupervisor
	case KindSystemdSystem:
		if killsTmux(info.KillMode) {
			info.Warnings = append(info.Warnings, "The unit uses KillMode="+info.KillMode+": a unit restart would also stop the tmux server and every agent, so tclaude restarts in place instead. Set KillMode=process.")
			return RestartExec
		}
		if env.EUID != 0 {
			info.Warnings = append(info.Warnings, "System unit and agentd is not root: tclaude cannot restart the unit, so it restarts in place with the same arguments.")
			return RestartExec
		}
		return RestartSupervisor
	case KindLaunchd:
		return RestartSupervisor
	}
	return RestartExec
}

func killsTmux(mode string) bool {
	return mode == "" || mode == "control-group" || mode == "mixed"
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
