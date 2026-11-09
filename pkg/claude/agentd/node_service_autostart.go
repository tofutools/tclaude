package agentd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/tofutools/tclaude/pkg/launcher"
)

// Autostart: `tclaude service install|uninstall` and the Config tab's
// Service panel write a systemd user unit (Linux) or a LaunchAgent (macOS)
// that starts agentd at boot or login, keeping the observed launch
// arguments and environment by default.

// autostartFactory builds the production Autostart; tests swap it so they
// never write a real unit or reach systemctl/launchctl.
var autostartFactory = func() launcher.Autostart {
	home, _ := os.UserHomeDir()
	name := ""
	if u, err := user.Current(); err == nil {
		name = u.Username
	}
	return launcher.Autostart{
		GOOS: runtime.GOOS, UID: os.Getuid(), User: name,
		Paths: launcher.AutostartPaths(runtime.GOOS, home, os.Getenv("XDG_CONFIG_HOME")),
		Run:   launcherRunner, FS: launcher.OSFS{},
	}
}

// autostartLookupEnv reads this process's environment for the values of the
// observed launcher variables (the status only carries redacted copies).
var autostartLookupEnv = os.LookupEnv

// AutostartRequest is the POST .../service/autostart body.
type AutostartRequest struct {
	Action string `json:"action"`
	// ServeArgs replaces the `serve` flags. Omitted (null) keeps the
	// observed flags; [] means none.
	ServeArgs *[]string `json:"serve_args,omitempty"`
	// KeepEnv (default true) carries the launcher's observed environment
	// and EnvironmentFiles into the unit. PATH is always captured.
	KeepEnv *bool `json:"keep_env,omitempty"`
}

// supervisorEnvKeys are set by the supervisor itself, not by the unit.
var supervisorEnvKeys = map[string]bool{
	"INVOCATION_ID": true, "JOURNAL_STREAM": true, "MANAGERPID": true, "SYSTEMD_EXEC_PID": true,
	"NOTIFY_SOCKET": true, "LISTEN_PID": true, "LISTEN_FDS": true, "LISTEN_FDNAMES": true,
	"WATCHDOG_PID": true, "WATCHDOG_USEC": true, "XPC_SERVICE_NAME": true, "XPC_FLAGS": true,
	"HOME": true, "USER": true, "LOGNAME": true, "SHELL": true, "PATH": true,
}

// serveFlagsDenied cannot work in a unit: they need a terminal, or would
// write the operator token to the journal.
var serveFlagsDenied = map[string]string{
	"tui":             "--tui needs a terminal",
	"own-tmux-server": "--own-tmux-server only applies with --tui",
}

// planAutostart resolves the unit's argv and env from the observed launcher
// and the request, with warnings for what it could not keep.
func planAutostart(info launcher.Info, req AutostartRequest, ownEnvFile string) (launcher.AutostartSpec, []string, error) {
	var warnings []string
	exe, observedFlags, err := splitServeArgv(info.RawArgv())
	if err != nil {
		return launcher.AutostartSpec{}, nil, err
	}
	var flags []string
	if req.ServeArgs != nil {
		flags = *req.ServeArgs
	} else {
		// The observed flags may carry terminal-only ones (a `--tui`
		// launch); drop those rather than refuse the default install.
		for _, f := range observedFlags {
			name, _, _ := strings.Cut(strings.TrimLeft(f, "-"), "=")
			if why, denied := serveFlagsDenied[name]; denied && strings.HasPrefix(f, "--") {
				warnings = append(warnings, "Left out "+f+": "+why+".")
				continue
			}
			flags = append(flags, f)
		}
	}
	if err := validateServeFlags(flags); err != nil {
		return launcher.AutostartSpec{}, nil, err
	}
	spec := launcher.AutostartSpec{Argv: append(exe, flags...)}
	if path, _ := autostartLookupEnv("PATH"); path != "" {
		spec.Env = append(spec.Env, launcher.EnvVar{Key: "PATH", Value: path})
	}
	keep := req.KeepEnv == nil || *req.KeepEnv
	if keep {
		seen := map[string]bool{"PATH": true}
		for _, e := range info.Env {
			// Variables from our own env file are rewritten into it; ones
			// from other EnvironmentFiles are referenced, not copied.
			if (e.From != "" && e.From != ownEnvFile) || supervisorEnvKeys[e.Key] || seen[e.Key] {
				continue
			}
			if v, ok := autostartLookupEnv(e.Key); ok {
				seen[e.Key] = true
				spec.Env = append(spec.Env, launcher.EnvVar{Key: e.Key, Value: v})
			}
		}
		for _, f := range info.EnvironmentFiles {
			if f != ownEnvFile {
				spec.ExtraEnvFiles = append(spec.ExtraEnvFiles, f)
			}
		}
		if !info.Supervised() {
			warnings = append(warnings, "agentd was started from a "+info.Kind+", not a service, so only PATH was captured; variables exported in that shell are not in the unit. Add the ones the harnesses need (API keys, proxy settings, TCLAUDE_*) to the env file.")
		}
	}
	if info.Supervised() && info.Unit != launcher.SystemdUnitName && info.Label != launcher.LaunchdLabel {
		warnings = append(warnings, "agentd runs under "+info.Unit+info.Label+", which tclaude did not write. Disable it, or both start at boot and the second one stops at once.")
	}
	return spec, warnings, launcher.ValidateSpec(spec)
}

// splitServeArgv splits `<tclaude> agentd serve <flags>` or
// `<tclaude-agentd> serve <flags>` into the command prefix (with an
// absolute executable) and the flags.
func splitServeArgv(argv []string) ([]string, []string, error) {
	n := -1
	for i, a := range argv {
		if a == "serve" {
			n = i
			break
		}
	}
	ok := n == 1 && filepath.Base(argv[0]) == "tclaude-agentd" ||
		n == 2 && argv[1] == "agentd"
	if !ok {
		return nil, nil, fmt.Errorf("the observed command is not `tclaude agentd serve`: %q", strings.Join(argv, " "))
	}
	exe := argv[0]
	if !filepath.IsAbs(exe) {
		resolved, err := exec.LookPath(exe)
		if err != nil {
			return nil, nil, fmt.Errorf("cannot resolve the executable %q: %w", exe, err)
		}
		if exe, err = filepath.Abs(resolved); err != nil {
			return nil, nil, err
		}
	}
	prefix := append([]string{exe}, argv[1:n+1]...)
	return prefix, append([]string(nil), argv[n+1:]...), nil
}

// validateServeFlags parses flags with the real `agentd serve` flag set:
// only its flags, no positional arguments, nothing unit-hostile.
func validateServeFlags(flags []string) error {
	if len(flags) > 48 {
		return fmt.Errorf("too many serve flags")
	}
	for _, f := range flags {
		if f == "--" {
			return fmt.Errorf("`--` is not a serve flag")
		}
	}
	fs := serveCmd().Flags()
	if err := fs.Parse(flags); err != nil {
		return fmt.Errorf("invalid serve flags: %w", err)
	}
	if extra := fs.Args(); len(extra) > 0 {
		return fmt.Errorf("serve takes flags only; unexpected %q", strings.Join(extra, " "))
	}
	for name, why := range serveFlagsDenied {
		if f := fs.Lookup(name); f != nil && f.Changed {
			return fmt.Errorf("%s in an autostart unit: %s", "--"+name, why)
		}
	}
	if f := fs.Lookup("print-human-token"); f != nil && f.Changed && f.Value.String() == "always" {
		return fmt.Errorf("--print-human-token=always would write the operator token to the service log; use --persist-operator-token")
	}
	return nil
}

func autostartStatus() launcher.AutostartStatus {
	return autostartFactory().Status(context.Background())
}

func serveNodeServiceAutostart(w http.ResponseWriter, r *http.Request, actor string) {
	w.Header().Set("Cache-Control", "private, no-store")
	r.Body = http.MaxBytesReader(w, r.Body, 16*1024)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var req AutostartRequest
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "invalid autostart request")
		return
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid_arg", "only one autostart JSON object is allowed")
		return
	}
	a := autostartFactory()
	switch req.Action {
	case "install":
		spec, warnings, err := planAutostart(currentLauncher(false), req, a.Paths.EnvFile)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
			return
		}
		if err := a.Install(r.Context(), spec); err != nil {
			writeAutostartError(w, err)
			return
		}
		recordFederationAudit("node.service.autostart", actor, "", "", "install "+a.Paths.Unit+": "+strings.Join(launcher.RedactArgv(spec.Argv), " "), http.StatusOK)
		st := nodeServiceStatus(false)
		st.Autostart.Warnings = append(st.Autostart.Warnings, warnings...)
		writeJSON(w, http.StatusOK, st)
		return
	case "uninstall":
		if req.ServeArgs != nil || req.KeepEnv != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", "uninstall takes no options")
			return
		}
		if err := a.Uninstall(r.Context()); err != nil {
			writeAutostartError(w, err)
			return
		}
		recordFederationAudit("node.service.autostart", actor, "", "", "uninstall "+a.Paths.Unit, http.StatusOK)
	default:
		writeError(w, http.StatusBadRequest, "invalid_arg", "action must be install or uninstall")
		return
	}
	writeJSON(w, http.StatusOK, nodeServiceStatus(false))
}

func writeAutostartError(w http.ResponseWriter, err error) {
	if err == launcher.ErrNotManaged {
		writeError(w, http.StatusConflict, "unit_not_managed", err.Error())
		return
	}
	writeError(w, http.StatusInternalServerError, "autostart_failed", err.Error())
}
