package launcher

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

var (
	envKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
	userRe   = regexp.MustCompile(`^[a-z_][a-z0-9_.-]{0,63}$`)
)

// Autostart starts agentd at boot (systemd user unit with lingering) or at
// login (macOS LaunchAgent). tclaude only writes, rewrites or removes files
// that carry ManagedMarker.

// Names of what autostart writes.
const (
	SystemdUnitName = "tclaude-agentd.service"
	LaunchdLabel    = "com.tofutools.tclaude.agentd"
)

// AutostartStatus is the autostart part of the service status.
type AutostartStatus struct {
	Supported bool `json:"supported"`
	// Kind is systemd-user or launchd, the kind of unit autostart writes.
	Kind string `json:"kind,omitempty"`
	Path string `json:"path,omitempty"`
	// Installed: the unit file exists; Managed: it carries ManagedMarker.
	Installed bool `json:"installed"`
	Managed   bool `json:"managed"`
	// Enabled is systemctl is-enabled / not launchctl-disabled.
	Enabled bool `json:"enabled"`
	// Argv and Env are what the managed unit runs with (env redacted).
	Argv    []string `json:"argv,omitempty"`
	Env     []EnvVar `json:"env,omitempty"`
	EnvFile string   `json:"env_file,omitempty"`
	// Linger reports systemd lingering, which a user unit needs to start at
	// boot rather than at login. Enabling it needs root, so tclaude only
	// prints the command.
	Linger   *Linger  `json:"linger,omitempty"`
	Warnings []string `json:"warnings"`
}

// Linger is systemd's per-user lingering state.
type Linger struct {
	Enabled bool   `json:"enabled"`
	Command string `json:"command"`
}

// AutostartSpec is what to install.
type AutostartSpec struct {
	// Argv is the full command: an absolute executable, then its arguments.
	Argv []string
	// Env is written into the unit (systemd: a 0600 EnvironmentFile; launchd:
	// EnvironmentVariables). Values are real, not redacted.
	Env []EnvVar
	// ExtraEnvFiles are existing EnvironmentFiles to reference (systemd).
	ExtraEnvFiles []string
}

// Paths is where autostart writes. FromHome derives the production paths.
type Paths struct {
	Unit    string
	EnvFile string // systemd only
	LogFile string // launchd only
}

// AutostartPaths returns the unit paths for goos under home, honouring
// XDG_CONFIG_HOME on Linux.
func AutostartPaths(goos, home, xdgConfig string) Paths {
	if goos == "darwin" {
		return Paths{
			Unit:    filepath.Join(home, "Library", "LaunchAgents", LaunchdLabel+".plist"),
			LogFile: filepath.Join(home, ".tclaude", "data", "launchd.log"),
		}
	}
	if xdgConfig == "" || !filepath.IsAbs(xdgConfig) {
		xdgConfig = filepath.Join(home, ".config")
	}
	return Paths{
		Unit:    filepath.Join(xdgConfig, "systemd", "user", SystemdUnitName),
		EnvFile: filepath.Join(xdgConfig, "tclaude", "agentd.env"),
	}
}

// Autostart installs, removes and reports the autostart unit. FS and Run are
// injected; tests never touch the real supervisor.
type Autostart struct {
	GOOS  string
	UID   int
	User  string
	Paths Paths
	Run   Runner
	FS    FS
}

// FS is the file access autostart needs.
type FS interface {
	ReadFile(string) ([]byte, error)
	WriteFile(string, []byte, os.FileMode) error
	MkdirAll(string, os.FileMode) error
	Remove(string) error
}

// OSFS is the production FS.
type OSFS struct{}

func (OSFS) ReadFile(p string) ([]byte, error) { return os.ReadFile(p) }
func (OSFS) MkdirAll(p string, m os.FileMode) error {
	return os.MkdirAll(p, m)
}
func (OSFS) Remove(p string) error { return os.Remove(p) }

// WriteFile writes atomically through a temp file in the same directory.
func (OSFS) WriteFile(p string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(p), "."+filepath.Base(p)+".tmp-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), p)
}

// ErrNotManaged refuses to overwrite or remove a unit tclaude did not write.
var ErrNotManaged = errors.New("the existing unit was not written by tclaude; edit or remove it yourself")

func (a Autostart) supported() bool { return a.GOOS == "linux" || a.GOOS == "darwin" }

func (a Autostart) kind() string {
	if a.GOOS == "darwin" {
		return KindLaunchd
	}
	return KindSystemdUser
}

// Status reads the installed unit.
func (a Autostart) Status(ctx context.Context) AutostartStatus {
	st := AutostartStatus{Supported: a.supported(), Warnings: []string{}}
	if !st.Supported {
		return st
	}
	st.Kind, st.Path = a.kind(), a.Paths.Unit
	data, err := a.FS.ReadFile(a.Paths.Unit)
	if err == nil {
		st.Installed = true
		st.Managed = bytes.Contains(data, []byte(ManagedMarker))
	}
	if st.Managed {
		if a.GOOS == "darwin" {
			st.Argv, st.Env = parseOwnPlist(data)
		} else {
			st.Argv = parseOwnExecStart(data)
			st.EnvFile = a.Paths.EnvFile
			if env, err := a.FS.ReadFile(a.Paths.EnvFile); err == nil {
				st.Env = parseEnvFile(env)
			}
		}
		for i, e := range st.Env {
			st.Env[i] = redactEnv(e.Key, e.Value)
		}
	}
	if a.GOOS == "darwin" {
		st.Enabled = st.Installed && !a.launchdDisabled(ctx)
		return st
	}
	if st.Installed {
		cctx, cancel := context.WithTimeout(ctx, probeTimeout)
		out, _ := a.Run(cctx, "systemctl", "--user", "is-enabled", SystemdUnitName)
		cancel()
		st.Enabled = strings.TrimSpace(string(out)) == "enabled"
	}
	st.Linger = a.linger(ctx)
	if st.Enabled && st.Linger != nil && !st.Linger.Enabled {
		st.Warnings = append(st.Warnings, "Lingering is off: the unit starts when you log in, not at boot. Run: "+st.Linger.Command)
	}
	return st
}

func (a Autostart) linger(ctx context.Context) *Linger {
	if a.User == "" || !validUser(a.User) {
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	out, err := a.Run(cctx, "loginctl", "show-user", a.User, "--property=Linger", "--value")
	l := &Linger{Command: "sudo loginctl enable-linger " + a.User}
	if err == nil {
		l.Enabled = strings.TrimSpace(string(out)) == "yes"
	}
	return l
}

// launchdDisabled reads `launchctl print-disabled gui/<uid>`.
func (a Autostart) launchdDisabled(ctx context.Context) bool {
	cctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	out, err := a.Run(cctx, "launchctl", "print-disabled", "gui/"+strconv.Itoa(a.UID))
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, `"`+LaunchdLabel+`"`) {
			return strings.HasSuffix(line, "disabled") || strings.HasSuffix(line, "true")
		}
	}
	return false
}

// Install writes (or rewrites) the managed unit and enables it. It does not
// start it: a second agentd next to the running one would fail its
// singleton lock and, under Restart=on-failure, loop. The unit starts at the
// next boot or login, or now through a supervisor restart once agentd runs
// under it.
func (a Autostart) Install(ctx context.Context, spec AutostartSpec) error {
	if !a.supported() {
		return fmt.Errorf("autostart is supported on Linux (systemd) and macOS (launchd)")
	}
	if err := ValidateSpec(spec); err != nil {
		return err
	}
	if data, err := a.FS.ReadFile(a.Paths.Unit); err == nil && !bytes.Contains(data, []byte(ManagedMarker)) {
		return ErrNotManaged
	}
	if a.GOOS == "darwin" {
		return a.installLaunchd(ctx, spec)
	}
	return a.installSystemd(ctx, spec)
}

func (a Autostart) installSystemd(ctx context.Context, spec AutostartSpec) error {
	if data, err := a.FS.ReadFile(a.Paths.EnvFile); err == nil && !bytes.Contains(data, []byte(ManagedMarker)) {
		return fmt.Errorf("%s exists and was not written by tclaude; move it aside or add its variables yourself", a.Paths.EnvFile)
	}
	if err := a.FS.MkdirAll(filepath.Dir(a.Paths.EnvFile), 0o700); err != nil {
		return err
	}
	if err := a.FS.WriteFile(a.Paths.EnvFile, RenderEnvFile(spec.Env), 0o600); err != nil {
		return err
	}
	if err := a.FS.MkdirAll(filepath.Dir(a.Paths.Unit), 0o755); err != nil {
		return err
	}
	if err := a.FS.WriteFile(a.Paths.Unit, RenderSystemdUnit(spec, a.Paths.EnvFile), 0o644); err != nil {
		return err
	}
	if err := a.systemctl(ctx, "daemon-reload"); err != nil {
		return err
	}
	return a.systemctl(ctx, "enable", SystemdUnitName)
}

func (a Autostart) installLaunchd(ctx context.Context, spec AutostartSpec) error {
	if err := a.FS.MkdirAll(filepath.Dir(a.Paths.Unit), 0o755); err != nil {
		return err
	}
	if a.Paths.LogFile != "" {
		_ = a.FS.MkdirAll(filepath.Dir(a.Paths.LogFile), 0o700)
	}
	if err := a.FS.WriteFile(a.Paths.Unit, RenderLaunchdPlist(spec, a.Paths.LogFile), 0o644); err != nil {
		return err
	}
	// launchd loads ~/Library/LaunchAgents at the next login. Clear an
	// earlier disable so it does; do not bootstrap now (see Install).
	cctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	if _, err := a.Run(cctx, "launchctl", "enable", "gui/"+strconv.Itoa(a.UID)+"/"+LaunchdLabel); err != nil {
		return fmt.Errorf("launchctl enable: %w", err)
	}
	return nil
}

// Uninstall disables and removes the managed unit (and, on Linux, its
// managed env file). A running agentd under it keeps running until it exits.
func (a Autostart) Uninstall(ctx context.Context) error {
	if !a.supported() {
		return fmt.Errorf("autostart is supported on Linux (systemd) and macOS (launchd)")
	}
	data, err := a.FS.ReadFile(a.Paths.Unit)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !bytes.Contains(data, []byte(ManagedMarker)) {
		return ErrNotManaged
	}
	if a.GOOS == "darwin" {
		cctx, cancel := context.WithTimeout(ctx, probeTimeout)
		_, _ = a.Run(cctx, "launchctl", "disable", "gui/"+strconv.Itoa(a.UID)+"/"+LaunchdLabel)
		cancel()
		return a.FS.Remove(a.Paths.Unit)
	}
	if err := a.systemctl(ctx, "disable", SystemdUnitName); err != nil {
		return err
	}
	if err := a.FS.Remove(a.Paths.Unit); err != nil {
		return err
	}
	if env, err := a.FS.ReadFile(a.Paths.EnvFile); err == nil && bytes.Contains(env, []byte(ManagedMarker)) {
		if err := a.FS.Remove(a.Paths.EnvFile); err != nil {
			return err
		}
	}
	return a.systemctl(ctx, "daemon-reload")
}

func (a Autostart) systemctl(ctx context.Context, args ...string) error {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, err := a.Run(cctx, "systemctl", append([]string{"--user"}, args...)...); err != nil {
		return fmt.Errorf("systemctl --user %s: %w", strings.Join(args, " "), err)
	}
	return nil
}

// ValidateSpec rejects text that would break out of a unit file line or a
// plist string: control characters anywhere, a relative executable, and
// malformed env keys.
func ValidateSpec(spec AutostartSpec) error {
	if len(spec.Argv) == 0 || !filepath.IsAbs(spec.Argv[0]) {
		return fmt.Errorf("the executable must be an absolute path")
	}
	if len(spec.Argv) > 64 {
		return fmt.Errorf("too many arguments")
	}
	for _, arg := range spec.Argv {
		if arg == "" {
			return fmt.Errorf("empty argument")
		}
		if err := plainText(arg, 4096); err != nil {
			return fmt.Errorf("argument %q: %w", arg, err)
		}
	}
	if len(spec.Env) > 128 {
		return fmt.Errorf("too many environment variables")
	}
	for _, e := range spec.Env {
		if !envKeyRe.MatchString(e.Key) {
			return fmt.Errorf("invalid environment variable name %q", e.Key)
		}
		if err := plainText(e.Value, 32*1024); err != nil {
			return fmt.Errorf("environment variable %s: %w", e.Key, err)
		}
	}
	for _, f := range spec.ExtraEnvFiles {
		if !filepath.IsAbs(f) || plainText(f, 4096) != nil || strings.ContainsAny(f, " \t\"'\\") {
			return fmt.Errorf("invalid environment file %q", f)
		}
	}
	return nil
}

func plainText(s string, max int) error {
	if len(s) > max {
		return fmt.Errorf("too long")
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("contains a control character")
		}
	}
	return nil
}

func validUser(u string) bool {
	return userRe.MatchString(u)
}

// RenderEnvFile renders a systemd EnvironmentFile. Values are double-quoted
// with \ and " escaped, which systemd unquotes.
func RenderEnvFile(env []EnvVar) []byte {
	var b strings.Builder
	b.WriteString("# " + ManagedMarker + ": written by `tclaude service install`; rewritten on reinstall.\n")
	for _, e := range sortedEnv(env) {
		fmt.Fprintf(&b, "%s=%s\n", e.Key, quoteSystemd(e.Value, false))
	}
	return []byte(b.String())
}

// RenderSystemdUnit renders the managed user unit, the shape
// docs/server/systemd.md describes (KillMode=process keeps the tmux server
// and the agents up across a unit restart).
func RenderSystemdUnit(spec AutostartSpec, envFile string) []byte {
	var b strings.Builder
	b.WriteString("# " + ManagedMarker + ": written by `tclaude service install`.\n")
	b.WriteString("# tclaude rewrites or removes this file; to take it over, delete this line.\n")
	b.WriteString("[Unit]\nDescription=tclaude agentd\n\n[Service]\nType=simple\n")
	fmt.Fprintf(&b, "EnvironmentFile=%s\n", strings.ReplaceAll(envFile, "%", "%%"))
	for _, f := range spec.ExtraEnvFiles {
		fmt.Fprintf(&b, "EnvironmentFile=-%s\n", strings.ReplaceAll(f, "%", "%%"))
	}
	quoted := make([]string, len(spec.Argv))
	for i, a := range spec.Argv {
		quoted[i] = quoteSystemd(a, true)
	}
	fmt.Fprintf(&b, "ExecStart=%s\n", strings.Join(quoted, " "))
	b.WriteString("# Stop only agentd, not the tmux server it started.\nKillMode=process\nRestart=on-failure\nRestartSec=5\n\n")
	b.WriteString("[Install]\nWantedBy=default.target\n")
	return []byte(b.String())
}

// quoteSystemd double-quotes s for a unit file. In ExecStart, % and $ are
// specifiers and variable expansion, so they are doubled.
func quoteSystemd(s string, exec bool) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	s = r.Replace(s)
	if exec {
		s = strings.NewReplacer("%", "%%", "$", "$$").Replace(s)
	}
	return `"` + s + `"`
}

// parseOwnExecStart reads back the ExecStart that RenderSystemdUnit wrote.
func parseOwnExecStart(unit []byte) []string {
	for _, line := range strings.Split(string(unit), "\n") {
		rest, ok := strings.CutPrefix(line, "ExecStart=")
		if !ok {
			continue
		}
		var out []string
		for _, w := range splitQuoted(rest) {
			out = append(out, strings.NewReplacer("%%", "%", "$$", "$").Replace(w))
		}
		return out
	}
	return nil
}

// RenderLaunchdPlist renders the managed LaunchAgent. AbandonProcessGroup
// keeps launchd from killing the tmux server (in agentd's process group)
// when agentd exits; KeepAlive restarts agentd after a crash, not after a
// clean exit.
func RenderLaunchdPlist(spec AutostartSpec, logFile string) []byte {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString("<!-- " + ManagedMarker + ": written by `tclaude service install`. tclaude rewrites or removes this file; to take it over, delete this comment. -->\n")
	b.WriteString("<plist version=\"1.0\">\n<dict>\n")
	key := func(k string) { fmt.Fprintf(&b, "\t<key>%s</key>\n", xmlText(k)) }
	str := func(indent, v string) { fmt.Fprintf(&b, "%s<string>%s</string>\n", indent, xmlText(v)) }
	key("Label")
	str("\t", LaunchdLabel)
	key("ProgramArguments")
	b.WriteString("\t<array>\n")
	for _, a := range spec.Argv {
		str("\t\t", a)
	}
	b.WriteString("\t</array>\n")
	if len(spec.Env) > 0 {
		key("EnvironmentVariables")
		b.WriteString("\t<dict>\n")
		for _, e := range sortedEnv(spec.Env) {
			fmt.Fprintf(&b, "\t\t<key>%s</key>\n", xmlText(e.Key))
			str("\t\t", e.Value)
		}
		b.WriteString("\t</dict>\n")
	}
	key("RunAtLoad")
	b.WriteString("\t<true/>\n")
	key("KeepAlive")
	b.WriteString("\t<dict>\n\t\t<key>SuccessfulExit</key>\n\t\t<false/>\n\t</dict>\n")
	key("ThrottleInterval")
	b.WriteString("\t<integer>5</integer>\n")
	key("AbandonProcessGroup")
	b.WriteString("\t<true/>\n")
	key("ProcessType")
	str("\t", "Interactive")
	if logFile != "" {
		key("StandardOutPath")
		str("\t", logFile)
		key("StandardErrorPath")
		str("\t", logFile)
	}
	b.WriteString("</dict>\n</plist>\n")
	return []byte(b.String())
}

func xmlText(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// parseOwnPlist reads back ProgramArguments and EnvironmentVariables from a
// plist RenderLaunchdPlist wrote.
func parseOwnPlist(data []byte) ([]string, []EnvVar) {
	dec := xml.NewDecoder(bytes.NewReader(data))
	dec.Strict = false
	var argv []string
	var env []EnvVar
	var path []string
	lastKey, section, envKey := "", "", ""
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			path = append(path, t.Name.Local)
			if (t.Name.Local == "array" || t.Name.Local == "dict") && len(path) == 3 {
				section = lastKey
			}
		case xml.EndElement:
			if len(path) == 3 && (t.Name.Local == "array" || t.Name.Local == "dict") {
				section = ""
			}
			if len(path) > 0 {
				path = path[:len(path)-1]
			}
		case xml.CharData:
			if len(path) == 0 {
				continue
			}
			text := string(t)
			switch {
			case len(path) == 3 && path[2] == "key":
				lastKey = text
			case len(path) == 4 && section == "ProgramArguments" && path[3] == "string":
				argv = append(argv, text)
			case len(path) == 4 && section == "EnvironmentVariables" && path[3] == "key":
				envKey = text
			case len(path) == 4 && section == "EnvironmentVariables" && path[3] == "string":
				env = append(env, EnvVar{Key: envKey, Value: text})
			}
		}
	}
	return argv, env
}

func sortedEnv(env []EnvVar) []EnvVar {
	out := append([]EnvVar(nil), env...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}
