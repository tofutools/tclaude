package agentd

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"time"

	"fyne.io/systray"
	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/terminal"
	"github.com/tofutools/tclaude/pkg/common"
)

type serveParams struct {
	Socket              string `long:"socket" short:"s" optional:"true" help:"Unix socket path (default ~/.tclaude/agentd.sock)"`
	NoTray              bool   `long:"no-tray" help:"Don't show a system tray icon. Use on headless / CI hosts."`
	AutoLaunchDashboard bool   `long:"auto-launch-dashboard" help:"Open the agentd dashboard in your browser on startup (also settable via agent.auto_launch_dashboard in config.json)."`
	Terminal            string `long:"terminal" optional:"true" help:"Terminal emulator for agent shell windows (ghostty, kitty, wezterm, alacritty, iterm2, gnome-terminal, …). Default: auto-detect. Also settable via the 'terminal' field in config.json."`
	AgentCloneCooldown  string `long:"agent-clone-cooldown" optional:"true" help:"Minimum cooldown between two clones of the same agent (Go duration, e.g. 1m, 30s; 0 disables). Overrides agent.clone_cooldown in config.json. Default 1m."`
}

func serveCmd() *cobra.Command {
	return boa.CmdT[serveParams]{
		Use:         "serve",
		Short:       "Run the agent daemon in the foreground",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *serveParams, _ *cobra.Command, _ []string) {
			if err := runServe(p); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
		},
	}.ToCobra()
}

// popupBaseURL is set during runServe once the loopback listener has
// chosen a port. requestHumanApproval reads it to build the URL it
// hands to xdg-open. Empty when no popup listener is up — in that
// case ask-human flows return immediately denied.
var popupBaseURL string

func runServe(p *serveParams) error {
	sockPath := p.Socket
	if sockPath == "" {
		sockPath = SocketPath()
		if sockPath == "" {
			return fmt.Errorf("could not determine socket path; pass --socket")
		}
	}
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o755); err != nil {
		return fmt.Errorf("mkdir socket dir: %w", err)
	}
	// Clean up any stale socket file from a crashed previous run. We don't
	// want to clobber a live one — error out if something is already
	// listening — and we explicitly refuse to delete anything that isn't
	// a Unix socket, in case a user pointed --socket at a regular file.
	if fi, err := os.Lstat(sockPath); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("refusing to remove non-socket path %s", sockPath)
		}
		if c, derr := net.Dial("unix", sockPath); derr == nil {
			_ = c.Close()
			return fmt.Errorf("agentd is already listening on %s", sockPath)
		}
		if err := os.Remove(sockPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove stale socket: %w", err)
		}
	}

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		return fmt.Errorf("listen %s: %w", sockPath, err)
	}
	if err := os.Chmod(sockPath, 0o600); err != nil {
		_ = ln.Close()
		return fmt.Errorf("chmod socket: %w", err)
	}
	defer func() { _ = os.Remove(sockPath) }()

	srv := &http.Server{
		Handler:           withIdentity(buildMux()),
		ReadHeaderTimeout: 5 * time.Second,
		// Stash the underlying *net.UnixConn so the identity middleware
		// can read peer credentials from it.
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			if uc, ok := c.(*net.UnixConn); ok {
				return context.WithValue(ctx, unixConnKey{}, uc)
			}
			return ctx
		},
	}

	// Loopback HTTP listener for the human-approval popup (Phase B of
	// the permission story). Bind :0 so we never collide with another
	// daemon or app; the URL is derived from ln.Addr() and only fed to
	// xdg-open, never persisted. Failure to bind is logged but does
	// not abort startup — the rest of the daemon still works, just
	// without --ask-human.
	popupSrv, popupURL := startPopupServer()
	popupBaseURL = popupURL

	// Auto-launch the dashboard in the browser when the human opted in
	// — either via --auto-launch-dashboard or the persistent
	// agent.auto_launch_dashboard config field. Saves a separate
	// `tclaude agent dashboard` after every daemon start. Best-effort:
	// a failed launch is logged, never fatal.
	cfg, _ := config.Load()
	if shouldAutoLaunchDashboard(p.AutoLaunchDashboard, cfg) {
		autoLaunchDashboard()
	}

	// Clone cooldown (see CloneCooldown). Resolved once here at startup,
	// tier order flag > config > default; the clone handler reads
	// CloneCooldown per request.
	cooldown, source := resolveCloneCooldown(p.AgentCloneCooldown, cfg)
	CloneCooldown = cooldown
	slog.Info("clone cooldown", "cooldown", cooldown, "source", source)

	// Spawn guardrails — resolve the runaway-prevention knobs from the
	// config.json `agent` section into the agentd package vars once,
	// here at startup; handleGroupSpawn reads them per request. Absent
	// fields keep the built-in defaults. See spawn_guardrails.go.
	slog.Info("agent-spawn guardrails", "config", resolveSpawnGuardrailConfig(cfg))

	// Terminal preference. claude.go's PersistentPreRun already applied
	// the config file's `terminal` field (tier 2); the --terminal flag
	// (tier 1) overrides it here. Resolve then runs the one-time
	// terminal detection now, at startup, so every later agent spawn
	// opens a window with no fresh PATH / bundle / osascript lookups.
	resolveTerminalPreference(p.Terminal)

	// Recurring agent_cron_jobs scheduler. Runs in its own goroutine
	// and stops when the daemon-wide quit channel closes.
	cronStop := make(chan struct{})
	defer close(cronStop)
	startCronScheduler(cronStop)

	// agent_sudo_grants housekeeping. Hard-deletes expired rows
	// older than sudoGrantsRetention so the table stays bounded.
	// Shares the same stop channel — both sweeps shut down together
	// when the daemon quits.
	startSudoGrantsCleanup(cronStop)

	// Session reaper. Sweeps for sessions whose tmux session + process
	// are gone and stamps status=exited — a crashed or kill -9'd agent
	// fires no SessionEnd hook, so without this its row would stay
	// frozen at its last hook status. Shares the daemon-wide stop
	// channel.
	startSessionReaper(cronStop)

	// One-shot: enroll any conv that is online right now but not yet
	// in agent_enrollment. The v29→v30 migration backfills agents from
	// the durable agentic tables, but a still-running agent that was
	// otherwise unrecorded can't be tmux-probed from a SQL migration —
	// this sweep closes that gap so no live agent drops off the roster
	// across the upgrade.
	go reconcileOnlineEnrollment()

	// Both the Unix-socket server and the popup server run in
	// goroutines so the main goroutine is free for the tray loop
	// (systray needs the main thread on every supported platform).
	serveErrCh := make(chan error, 1)
	go func() {
		slog.Info("agentd listening", "socket", sockPath, "popup", popupBaseURL)
		fmt.Printf("tclaude agentd listening on %s\n", sockPath)
		if popupBaseURL != "" {
			fmt.Printf("  human-approval popup on %s/approve/<id>\n", popupBaseURL)
			fmt.Printf("  agent dashboard:        run `tclaude agent dashboard` (loopback %s)\n", popupBaseURL)
		}
		serveErrCh <- srv.Serve(ln)
	}()

	quit := newQuitter()

	// Translate any of: SIGINT/SIGTERM, the socket server dying,
	// "Quit" from the tray menu, into a single shutdown signal.
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		select {
		case <-sig:
			slog.Info("agentd received shutdown signal")
		case err := <-serveErrCh:
			if err != nil && err != http.ErrServerClosed {
				slog.Error("agentd socket server failed", "error", err)
			}
		}
		quit.signal()
	}()

	if p.NoTray {
		// Legacy / headless path: just block on shutdown.
		<-quit.ch
	} else {
		// Tray path: when shutdown is requested for any reason
		// (signal, server error, tray Quit), call systray.Quit() to
		// unblock systray.Run on the main goroutine.
		go func() {
			<-quit.ch
			systray.Quit()
		}()
		runTrayBlocking(trayConfig{
			SocketPath:   sockPath,
			PopupBaseURL: popupBaseURL,
		}, quit.signal)
	}

	// Graceful shutdown.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	if popupSrv != nil {
		_ = popupSrv.Shutdown(ctx)
	}
	return nil
}

// quitter funnels multiple shutdown sources (signal, tray, server
// error) into one idempotent close on the quit channel.
type quitter struct {
	once sync.Once
	ch   chan struct{}
}

func newQuitter() *quitter {
	return &quitter{ch: make(chan struct{})}
}

func (q *quitter) signal() {
	q.once.Do(func() { close(q.ch) })
}

// resolveTerminalPreference applies the --terminal flag (the highest
// priority tier) over whatever the config file already set, then runs
// the one-time terminal detection and prints the picked terminal as
//
//	selected terminal: <os>/<terminal>
//
// An unknown --terminal value is warned about, not fatal — auto-detect
// still applies. A detection failure is logged but never aborts
// startup; the daemon runs fine, agent shell windows just won't open.
func resolveTerminalPreference(flagValue string) {
	if flagValue != "" {
		if id := terminal.CanonicalTerminalID(flagValue); id != "" {
			terminal.SetPreferred(id)
		} else {
			slog.Warn("unknown --terminal value; falling back to auto-detect",
				"value", flagValue, "known", terminal.KnownTerminalIDs())
		}
	}
	if err := terminal.Resolve(); err != nil {
		slog.Warn("terminal detection failed; agent shell windows will be unavailable",
			"error", err)
		return
	}
	fmt.Printf("selected terminal: %s/%s\n", runtime.GOOS, terminal.ResolvedTerminal())
}

// resolveCloneCooldown resolves the clone cooldown — the minimum
// cooldown between two clones of the same source conv (see
// CloneCooldown). Priority, highest first:
//
//  1. the --agent-clone-cooldown serve flag
//  2. the agent.clone_cooldown config.json field
//  3. the built-in defaultCloneCooldown (1m)
//
// The value is a Go duration string; "0" disables the cooldown. A
// missing value at a tier is skipped silently; a present-but-invalid
// (unparseable or negative) value is warned about and skipped. Either
// way the next tier is consulted. Returns the resolved duration and
// the tier it came from, for the startup log line.
func resolveCloneCooldown(flagValue string, cfg *config.Config) (time.Duration, string) {
	if d, ok := parseCloneCooldown(flagValue, "--agent-clone-cooldown"); ok {
		return d, "flag"
	}
	if cfg != nil && cfg.Agent != nil {
		if d, ok := parseCloneCooldown(cfg.Agent.CloneCooldown, "agent.clone_cooldown"); ok {
			return d, "config"
		}
	}
	return defaultCloneCooldown, "default"
}

// parseCloneCooldown parses one tier's raw value. An empty string
// means "tier not set" — returns ok=false with no warning. A non-empty
// but unparseable or negative value is warned about (source names the
// tier) and also returns ok=false so the caller falls through.
func parseCloneCooldown(value, source string) (time.Duration, bool) {
	if value == "" {
		return 0, false
	}
	d, err := time.ParseDuration(value)
	if err != nil {
		slog.Warn("invalid "+source+"; ignoring", "value", value, "error", err)
		return 0, false
	}
	if d < 0 {
		slog.Warn("negative "+source+"; ignoring", "value", value)
		return 0, false
	}
	return d, true
}

func buildMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/info", handleInfo)
	mux.HandleFunc("/v1/dashboard/open", handleDashboardOpen)
	mux.HandleFunc("/v1/whoami", handleWhoami)
	mux.HandleFunc("/v1/whoami/rename", handleWhoamiRename)
	mux.HandleFunc("/v1/whoami/compact", handleWhoamiCompact)
	mux.HandleFunc("/v1/whoami/reincarnate", handleWhoamiReincarnate)
	mux.HandleFunc("/v1/whoami/clone", handleWhoamiClone)
	mux.HandleFunc("/v1/whoami/context", handleWhoamiContext)
	mux.HandleFunc("/v1/whoami/dir", handleWhoamiDir)
	mux.HandleFunc("/v1/lookup", handleLookup)
	mux.HandleFunc("/v1/peers", handlePeers)
	mux.HandleFunc("/v1/messages", handleMessages)
	mux.HandleFunc("/v1/messages/", handleMessageByIDOrReply)
	mux.HandleFunc("/v1/inbox", handleInbox)
	mux.HandleFunc("/v1/inbox/prune", handleInboxPrune)
	// Head aliases: most-specific path goes first so HandleFunc's
	// pattern table picks `/v1/agent/aliases` over `/v1/agent/`.
	mux.HandleFunc("/v1/agent/aliases", handleHeadAliases)
	mux.HandleFunc("/v1/agent/aliases/", handleHeadAliasByHandle)
	mux.HandleFunc("/v1/agent/", handleAgentByConv)
	mux.HandleFunc("/v1/groups", handleGroups)
	mux.HandleFunc("/v1/groups/", handleGroupByName)
	mux.HandleFunc("/v1/links", handleLinksAll)
	mux.HandleFunc("/v1/can-message", handleCanMessage)
	mux.HandleFunc("/v1/permissions", handlePermissions)
	mux.HandleFunc("/v1/permissions/slugs", handlePermissionsSlugs)
	mux.HandleFunc("/v1/permissions/grant", handlePermissionsGrant)
	mux.HandleFunc("/v1/permissions/revoke", handlePermissionsRevoke)
	mux.HandleFunc("/v1/cron", handleCron)
	mux.HandleFunc("/v1/cron/", handleCronByID)
	// Sudo: most-specific path goes first so the trailing-slash form
	// catches /v1/sudo/{id} and the bare form catches POST/GET/DELETE
	// against the collection.
	mux.HandleFunc("/v1/sudo", handleSudo)
	mux.HandleFunc("/v1/sudo/", handleSudoByID)
	return logRequest(mux)
}

func logRequest(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRec{ResponseWriter: w, code: 200}
		h.ServeHTTP(rec, r)
		p := peerFromContext(r.Context())
		slog.Info("http",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.code,
			"peer_pid", p.PID,
			"peer_conv", p.ConvID,
			"dur_ms", time.Since(start).Milliseconds())
	})
}

type statusRec struct {
	http.ResponseWriter
	code int
}

func (r *statusRec) WriteHeader(c int) {
	r.code = c
	r.ResponseWriter.WriteHeader(c)
}
