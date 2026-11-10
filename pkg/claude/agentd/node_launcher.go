package agentd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/common/buildversion"
	"github.com/tofutools/tclaude/pkg/launcher"
)

// App launcher: `tclaude service launcher install|uninstall` and the Config
// tab's Installation page write a desktop entry (Linux) or an app bundle in
// ~/Applications (macOS) that opens the dashboard, starting agentd first
// when it does not answer. The launcher runs the tclaude binary this daemon
// runs; a managed launcher naming another binary or version is rewritten
// when agentd starts (refreshStaleAppLauncher), which covers a self-update
// (it restarts agentd) and a relocated binary (started from its new path).

// appLauncherFactory builds the production AppLauncher; tests swap it so
// they never write into the real HOME or run lsregister.
var appLauncherFactory = func() launcher.AppLauncher {
	home, _ := os.UserHomeDir()
	spec, _ := currentAppLauncherSpec()
	return launcher.AppLauncher{
		GOOS:    runtime.GOOS,
		Paths:   launcher.AppLauncherPathsFor(runtime.GOOS, home, os.Getenv("XDG_DATA_HOME")),
		Run:     launcherRunner,
		FS:      launcher.OSFS{},
		Current: spec,
	}
}

// currentAppLauncherSpec is what an install now writes: this daemon's
// tclaude binary, version and PATH, with warnings for PATH entries the
// launcher script cannot carry.
func currentAppLauncherSpec() (launcher.AppLauncherSpec, []string) {
	exe, _ := tclaudeExecutable(os.Args[0], exec.LookPath, fileExists)
	var keep, warnings []string
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" || strings.ContainsAny(dir, "\"'`$\\%") || strings.ContainsFunc(dir, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
			if dir != "" {
				warnings = append(warnings, "Left "+dir+" out of the launcher's PATH: it holds a quote, $, % or backslash.")
			}
			continue
		}
		keep = append(keep, dir)
	}
	keep = withLauncherFallbackPATH(runtime.GOOS, keep, dirExists)
	if lookPathIn("tmux", keep, fileExecutable) == "" {
		warnings = append(warnings, "tmux is not on the PATH the launcher gives agentd: an agentd it starts cannot reach your agents. "+
			"Reinstall the launcher from an agentd started in a terminal where tmux works.")
	}
	spec := launcher.AppLauncherSpec{
		Exe:     exe,
		Version: buildversion.AppVersion(),
		PATH:    strings.Join(keep, string(os.PathListSeparator)),
	}
	return spec, warnings
}

// launcherFallbackDirs are where macOS package managers put tmux and the
// harnesses. A launcher installed from an agentd that itself had the bare
// app PATH would otherwise bake in a PATH without them.
var launcherFallbackDirs = map[string][]string{
	"darwin": {"/opt/homebrew/bin", "/usr/local/bin"},
}

// withLauncherFallbackPATH appends the existing fallback dirs PATH lacks.
func withLauncherFallbackPATH(goos string, dirs []string, isDir func(string) bool) []string {
	out := append([]string(nil), dirs...)
	for _, d := range launcherFallbackDirs[goos] {
		if !slices.Contains(out, d) && isDir(d) {
			out = append(out, d)
		}
	}
	return out
}

// lookPathIn is exec.LookPath over dirs instead of $PATH.
func lookPathIn(name string, dirs []string, isExec func(string) bool) string {
	for _, d := range dirs {
		if p := filepath.Join(d, name); filepath.IsAbs(p) && isExec(p) {
			return p
		}
	}
	return ""
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func fileExecutable(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular() && st.Mode()&0o111 != 0
}

// appLauncher is the AppLauncher with the serve flags an install now bakes
// in, read from config.json at each call.
func appLauncher() launcher.AppLauncher {
	l := appLauncherFactory()
	if cfg, err := config.Load(); err == nil {
		l.Current.ServeArgs, _ = launcherServeArgs(cfg)
	}
	return l
}

// Where the launcher's serve flags come from (NodeAppLauncher.ServeArgsSource).
const (
	serveArgsFromProfile  = "profile"
	serveArgsFromLauncher = "launcher"
	serveArgsFromDefault  = "default"
)

// defaultLauncherDashboardPort is the fixed port the default launcher flags
// name when the running daemon has none.
const defaultLauncherDashboardPort = 7878

// operatorTokenStore is where the running daemon keeps its operator token:
// file, keychain or ephemeral (set by serve; empty when unknown).
var operatorTokenStore atomic.Value

// launcherInstallMu keeps a launcher write and the setting it bakes in
// together against a concurrent install or rewrite.
var launcherInstallMu sync.Mutex

// dashboardFixedPort is the running daemon's fixed dashboard port, 0 when
// it picked a random one (set by serve).
var dashboardFixedPort atomic.Int64

// launcherServeArgs are the serve flags the launcher starts agentd with:
// the launch profile's when one is set, else the launcher's own setting,
// else tclaude's defaults.
func launcherServeArgs(cfg *config.Config) ([]string, string) {
	if p := cfg.ServiceLaunch(); p != nil {
		return append([]string{}, p.Flags...), serveArgsFromProfile
	}
	if a := cfg.AppLauncherServeArgs(); a != nil {
		return append([]string{}, a...), serveArgsFromLauncher
	}
	return defaultLauncherServeArgs(), serveArgsFromDefault
}

// defaultLauncherServeArgs: the dashboard sign-in survives restarts, a fixed
// port keeps bookmarks and open tabs working, and the token is not printed
// (a launcher has no terminal, so it would only land in a log). The tray
// stays on and the dashboard on loopback.
func defaultLauncherServeArgs() []string {
	port := dashboardFixedPort.Load()
	if port <= 0 {
		port = defaultLauncherDashboardPort
	}
	return []string{"--persist-operator-token", "--dashboard-port=" + strconv.FormatInt(port, 10), "--no-print-human-token"}
}

// peerLauncherServeFlags are the serve flags a peer without full access may
// choose for the launcher.
var peerLauncherServeFlags = map[string]bool{
	"--persist-operator-token": true, "--dashboard-port": true, "--no-print-human-token": true, "--no-tray": true,
}

// validateLauncherServeArgs checks flags as a launch profile's are checked
// (known serve flags only, none secret or terminal-only), and for a caller
// without full access against peerLauncherServeFlags.
func validateLauncherServeArgs(args []string, access string) error {
	if errs := validateLaunchProfile(&config.LaunchProfile{Flags: args, Env: []config.LaunchEnv{}, InheritEnv: []string{}}); len(errs) > 0 {
		return errors.New(errs[0].Message)
	}
	if err := launcher.ValidateAppLauncherSpec(launcher.AppLauncherSpec{Exe: "/x", ServeArgs: args}); err != nil {
		return err
	}
	if access != serviceAccessFull {
		for _, a := range args {
			name, _, _ := strings.Cut(a, "=")
			if !peerLauncherServeFlags[name] {
				return errLauncherFlagNotAllowed{name}
			}
		}
	}
	return nil
}

type errLauncherFlagNotAllowed struct{ name string }

func (e errLauncherFlagNotAllowed) Error() string {
	return e.name + " can only be chosen on the node itself or by an unrestricted peer; peers may set --persist-operator-token, --dashboard-port, --no-print-human-token and --no-tray"
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// tclaudeExecutable is the absolute tclaude binary for argv0: the binary
// itself, or for the standalone tclaude-agentd the tclaude next to it (else
// on PATH). It keeps a symlinked install path (a package manager's bin
// link) rather than resolving it, so an upgrade behind the link does not
// leave the launcher pointing at the old version's file.
func tclaudeExecutable(argv0 string, lookPath func(string) (string, error), exists func(string) bool) (string, error) {
	exe := argv0
	if !filepath.IsAbs(exe) {
		resolved, err := lookPath(exe)
		if err != nil {
			return "", fmt.Errorf("cannot resolve the tclaude binary %q: %w", argv0, err)
		}
		if exe, err = filepath.Abs(resolved); err != nil {
			return "", err
		}
	}
	if filepath.Base(exe) != "tclaude-agentd" {
		return filepath.Clean(exe), nil
	}
	if sibling := filepath.Join(filepath.Dir(exe), "tclaude"); exists(sibling) {
		return sibling, nil
	}
	found, err := lookPath("tclaude")
	if err != nil {
		return "", fmt.Errorf("agentd runs as tclaude-agentd and no tclaude binary is next to it or on PATH")
	}
	return filepath.Abs(found)
}

// AppLauncherRequest is the POST .../launcher/install body.
type AppLauncherRequest struct {
	// StartDaemon (default true): the launcher starts agentd when it does
	// not answer.
	StartDaemon *bool `json:"start_daemon,omitempty"`
	// ServeArgs sets the launcher's own serve flags (service.launcher.
	// serve_args): omitted keeps the setting, null returns to the
	// defaults, an array (one token per flag, --name or --name=value)
	// replaces it.
	ServeArgs json.RawMessage `json:"serve_args,omitempty"`
}

// NodeAppLauncher is the GET /api/node/launcher and /v1/node/launcher body:
// the launcher status, and what the caller may do (full, manage or read, as
// for the service routes).
type NodeAppLauncher struct {
	launcher.AppLauncherStatus
	// ServeArgs are the serve flags an install now bakes in, and
	// ServeArgsSource where they come from: profile (the launch profile,
	// which overrides the rest), launcher (service.launcher.serve_args) or
	// default.
	ServeArgs       []string `json:"serve_args"`
	ServeArgsSource string   `json:"serve_args_source"`
	// DefaultServeArgs are tclaude's defaults, for a form to offer.
	DefaultServeArgs []string `json:"default_serve_args"`
	// OperatorTokenStore is where the running daemon keeps its operator
	// token (file, keychain, ephemeral). The launcher signs in to the
	// dashboard with it, so an ephemeral one gets a warning.
	OperatorTokenStore string `json:"operator_token_store,omitempty"`
	Access             string `json:"access"`
}

func registerNodeLauncherRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/node/launcher", handleDashboardNodeService)
	mux.HandleFunc("POST /api/node/launcher/install", handleDashboardNodeService)
	mux.HandleFunc("POST /api/node/launcher/uninstall", handleDashboardNodeService)
}

func nodeLauncherStatus(access string) NodeAppLauncher {
	st := NodeAppLauncher{AppLauncherStatus: appLauncher().Status(), Access: access, DefaultServeArgs: defaultLauncherServeArgs()}
	cfg, err := config.Load()
	if err != nil {
		st.Warnings = append(st.Warnings, "config.json could not be read, so the launcher's serve flags are unknown: "+err.Error())
		st.ServeArgs = []string{}
		return st
	}
	st.ServeArgs, st.ServeArgsSource = launcherServeArgs(cfg)
	st.OperatorTokenStore, _ = operatorTokenStore.Load().(string)
	st.Warnings = append(st.Warnings, launcherTokenWarnings(st.OperatorTokenStore, st.ServeArgs, cfg)...)
	return st
}

// launcherTokenWarnings: the launcher opens the dashboard as the operator,
// which needs the operator token, and a desktop launcher has no
// TCLAUDE_HUMAN_TOKEN. It reads the persisted token (the 0600 file, or the
// keychain), so a daemon with an in-memory token refuses it.
func launcherTokenWarnings(store string, serveArgs []string, cfg *config.Config) []string {
	var out []string
	if store == string(tokenSourceEphemeral) {
		out = append(out, "agentd keeps its operator token in memory only, so the launcher cannot sign in to the dashboard (it gets \"unconfirmed caller\"). Restart agentd with --persist-operator-token, or set agent.persist_operator_token, or add the flag to the launch profile.")
	}
	persists := cfg != nil && cfg.Agent != nil && (cfg.Agent.PersistOperatorToken || cfg.Agent.PersistOperatorTokenKeychain)
	for _, a := range serveArgs {
		name, value, _ := strings.Cut(a, "=")
		if (name == "--persist-operator-token" || name == "--persist-operator-token-keychain" || name == "-p") && value != "false" {
			persists = true
		}
	}
	if !persists {
		out = append(out, "The serve flags the launcher starts agentd with do not persist the operator token, so a daemon it starts cannot be signed in to from the launcher. Add --persist-operator-token.")
	}
	return out
}

// serveNodeLauncher serves the launcher routes; serveNodeService routes
// them here after the caller's access is settled.
func serveNodeLauncher(w http.ResponseWriter, r *http.Request, actor, access string) {
	w.Header().Set("Cache-Control", "private, no-store")
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		writeJSON(w, http.StatusOK, nodeLauncherStatus(access))
		return
	case http.MethodPost:
	default:
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use GET to read and POST to act")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4*1024)
	var req AppLauncherRequest
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "invalid launcher request")
		return
	}
	if s := strings.TrimSpace(string(body)); s != "" {
		dec := json.NewDecoder(strings.NewReader(s))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil || dec.More() {
			writeError(w, http.StatusBadRequest, "invalid_arg", "invalid launcher request")
			return
		}
	}
	l := appLauncher()
	var warnings []string
	switch {
	case strings.HasSuffix(r.URL.Path, "/install"):
		spec := l.Current
		_, specWarnings := currentAppLauncherSpec()
		if spec.Exe == "" {
			writeError(w, http.StatusBadRequest, "invalid_arg", "cannot tell which tclaude binary this agentd runs")
			return
		}
		spec.Options.StartDaemon = req.StartDaemon == nil || *req.StartDaemon
		change, args, argWarnings, status, code, err := planLauncherServeArgs(req.ServeArgs, access)
		if err != nil {
			writeError(w, status, code, err.Error())
			return
		}
		spec.ServeArgs = args
		specWarnings = append(specWarnings, argWarnings...)
		launcherInstallMu.Lock()
		err = l.Install(r.Context(), spec)
		if err == nil {
			err = storeLauncherServeArgs(change)
			if err != nil {
				err = fmt.Errorf("the launcher was written, but its serve flags could not be saved to config.json: %w", err)
			}
		}
		launcherInstallMu.Unlock()
		if err != nil {
			writeLauncherError(w, err)
			return
		}
		warnings = specWarnings
		recordFederationAudit("node.launcher", actor, "", "", fmt.Sprintf("install %s: %s (start_daemon=%t, serve %s)", l.Status().Path, spec.Exe, spec.Options.StartDaemon, serveFlagNames(spec.ServeArgs)), http.StatusOK)
	case strings.HasSuffix(r.URL.Path, "/uninstall"):
		if req.StartDaemon != nil || len(req.ServeArgs) > 0 {
			writeError(w, http.StatusBadRequest, "invalid_arg", "uninstall takes no options")
			return
		}
		if err := l.Uninstall(r.Context()); err != nil {
			writeLauncherError(w, err)
			return
		}
		recordFederationAudit("node.launcher", actor, "", "", "uninstall "+l.Status().Path, http.StatusOK)
	default:
		writeError(w, http.StatusNotFound, "not_found", "unknown launcher action")
		return
	}
	st := nodeLauncherStatus(access)
	st.Warnings = append(st.Warnings, warnings...)
	writeJSON(w, http.StatusOK, st)
}

func writeLauncherError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, launcher.ErrLauncherNotManaged):
		writeError(w, http.StatusConflict, "launcher_not_managed", err.Error())
	case errors.Is(err, launcher.ErrLauncherUnsupported):
		writeError(w, http.StatusBadRequest, "unsupported", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "launcher_failed", err.Error())
	}
}

// refreshStaleAppLauncher rewrites a managed launcher that runs another
// tclaude binary or names another version than this daemon, keeping its
// options. It never installs one that is not there, and leaves a launcher
// alone when this daemon runs from a throwaway build (go run, a test).
func refreshStaleAppLauncher(ctx context.Context) {
	launcherInstallMu.Lock()
	defer launcherInstallMu.Unlock()
	l := appLauncher()
	st := l.Status()
	if !st.Managed || !st.Stale || l.Current.Exe == "" || throwawayBinary(l.Current.Exe) {
		return
	}
	spec := l.Current
	spec.Options = st.Options
	if err := l.Install(ctx, spec); err != nil {
		slog.Warn("app launcher: could not rewrite the stale launcher", "path", st.Path, "reason", st.StaleReason, "error", err)
		return
	}
	slog.Info("app launcher: rewritten for this tclaude", "path", st.Path, "reason", st.StaleReason)
}

func throwawayBinary(exe string) bool {
	tmp := filepath.Clean(os.TempDir()) + string(filepath.Separator)
	return strings.HasPrefix(exe, tmp) || strings.Contains(exe, string(filepath.Separator)+"go-build")
}

// launcherServeArgsChange is a requested serve-flag setting (see
// AppLauncherRequest.ServeArgs), validated but not yet stored.
type launcherServeArgsChange struct {
	given bool
	clear bool
	set   []string
}

func (c launcherServeArgsChange) apply(cfg *config.Config) {
	if !c.given {
		return
	}
	if c.clear {
		if cfg.Service != nil {
			cfg.Service.Launcher = nil
			if *cfg.Service == (config.ServiceConfig{}) {
				cfg.Service = nil
			}
		}
		return
	}
	if cfg.Service == nil {
		cfg.Service = &config.ServiceConfig{}
	}
	cfg.Service.Launcher = &config.AppLauncherConfig{ServeArgs: c.set}
}

// planLauncherServeArgs validates a requested setting and returns the serve
// flags in effect with it applied. Nothing is stored: the caller stores it
// once the launcher is written, so a refused install changes nothing.
func planLauncherServeArgs(raw json.RawMessage, access string) (change launcherServeArgsChange, args []string, warnings []string, status int, code string, err error) {
	if len(raw) > 0 {
		change.given = true
		change.clear = strings.TrimSpace(string(raw)) == "null"
		if !change.clear {
			if err := json.Unmarshal(raw, &change.set); err != nil {
				return change, nil, nil, http.StatusBadRequest, "invalid_arg", errors.New("serve_args must be an array of flags or null")
			}
			if change.set == nil {
				change.set = []string{}
			}
			if err := validateLauncherServeArgs(change.set, access); err != nil {
				var denied errLauncherFlagNotAllowed
				if errors.As(err, &denied) {
					return change, nil, nil, http.StatusForbidden, "local_only_setting", err
				}
				return change, nil, nil, http.StatusBadRequest, "invalid_flag", err
			}
		}
	}
	cfg, err := config.Load()
	if err != nil {
		return change, nil, nil, http.StatusInternalServerError, "config_read_failed", fmt.Errorf("config.json could not be read, so the launcher's serve flags are unknown: %w", err)
	}
	change.apply(cfg)
	args, source := launcherServeArgs(cfg)
	if change.given && source == serveArgsFromProfile {
		warnings = append(warnings, "The launch profile is set, so the launcher starts agentd with the profile's flags; the launcher's own flags apply once the profile is cleared.")
	}
	return change, args, warnings, 0, "", nil
}

// storeLauncherServeArgs stores a planned setting.
func storeLauncherServeArgs(change launcherServeArgsChange) error {
	if !change.given {
		return nil
	}
	_, err := config.Update(func(cfg *config.Config, loadErr error) error {
		if loadErr != nil {
			return loadErr
		}
		change.apply(cfg)
		return nil
	})
	return err
}

// serveFlagNames lists flag names only, as the launch profile's audit does.
func serveFlagNames(args []string) string {
	names := make([]string, len(args))
	for i, a := range args {
		names[i], _, _ = strings.Cut(a, "=")
	}
	return strings.Join(names, ",")
}
