package agentd

import (
	"bufio"
	"context"
	"errors"
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
	Socket               string `long:"socket" short:"s" optional:"true" help:"Unix socket path (default ~/.tclaude/agentd.sock)"`
	NoTray               bool   `long:"no-tray" help:"Don't show a system tray icon. Use on headless / CI hosts. Also settable via agent.disable_tray in config.json."`
	AutoLaunchDashboard  bool   `long:"auto-launch-dashboard" help:"Open the agentd dashboard in your browser on startup (also settable via agent.auto_launch_dashboard in config.json)."`
	Slop                 bool   `long:"slop" help:"Open the auto-launched dashboard in 🎰 slop machine theme — a purely cosmetic re-skin, same data."`
	Terminal             string `long:"terminal" optional:"true" help:"Terminal emulator for agent shell windows (ghostty, kitty, wezterm, alacritty, foot, iterm2, konsole, gnome-terminal, …). Default: auto-detect. Also settable via the 'terminal' field in config.json."`
	AgentCloneCooldown   string `long:"agent-clone-cooldown" optional:"true" help:"Minimum cooldown between two clones of the same agent (Go duration, e.g. 1m, 30s; 0 disables). Overrides agent.clone_cooldown in config.json. Default 1m."`
	DashboardPort        int    `long:"dashboard-port" optional:"true" help:"Fixed loopback port for the dashboard + approval popup. 0 (default) picks a random free port each start. Overrides agent.dashboard_port in config.json. A configured port already in use (or out of range) fails startup rather than falling back to random."`
	PersistOperatorToken bool   `long:"persist-operator-token" help:"Persist the operator token across restarts (OS keychain when available, else a 0600 ~/.tclaude/operator_token file) instead of minting a fresh in-memory one each start. ORs with agent.persist_operator_token in config.json. Default: off (fresh token every boot)."`
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

	// Config drives the dashboard-port resolution below, the auto-launch
	// decision, the clone cooldown, and the spawn guardrails. Load it once
	// up front so every startup knob reads the same snapshot.
	cfg, _ := config.Load()

	// Loopback HTTP listener for the human-approval popup (Phase B of the
	// permission story) — the dashboard rides on the same listener. The
	// port is resolved flag > config > random (0): a fixed port gives a
	// stable, bookmarkable URL, while the default :0 never collides with
	// another daemon or app. The URL is derived from ln.Addr() and fed to
	// xdg-open / `tclaude agent dashboard`, never persisted.
	//
	// Bind failure is FATAL: the dashboard + approval popup are essential,
	// and a configured fixed port that is already in use must surface at
	// startup, not silently degrade to a random port (which would break the
	// bookmark / reverse-proxy / firewall rule the fixed port was set up
	// for). An out-of-range port likewise fails the bind and aborts here.
	dashPort, dashPortSrc := resolveDashboardPort(p.DashboardPort, cfg)
	popupSrv, popupURL, err := startPopupServer(dashPort)
	if err != nil {
		return fmt.Errorf("start dashboard/approval-popup server (port source: %s): %w", dashPortSrc, err)
	}
	popupBaseURL = popupURL
	slog.Info("dashboard loopback port", "resolved", dashPort, "source", dashPortSrc, "url", popupBaseURL)

	// Operator token — positively authenticates the human operator on the
	// CLI / Unix-socket path so the daemon can fail closed instead of
	// assuming "no Claude Code ancestor => human". By default minted fresh
	// each daemon lifetime and held only in memory; with persistence opted
	// in (flag ORs config) it is generated once and stored (OS keychain or
	// a 0600 file) so it survives restarts. Never written through slog
	// (slog → output.log); the banner below prints it only to a TTY.
	operatorTok, tokenSrc := resolveOperatorToken(shouldPersistOperatorToken(p.PersistOperatorToken, cfg))
	slog.Info("operator token", "source", tokenSrc.kind, "persisted", tokenSrc.kind != tokenSourceEphemeral)

	// Auto-launch the dashboard in the browser when the human opted in
	// — either via --auto-launch-dashboard or the persistent
	// agent.auto_launch_dashboard config field. Saves a separate
	// `tclaude agent dashboard` after every daemon start. Best-effort:
	// a failed launch is logged, never fatal.
	if shouldAutoLaunchDashboard(p.AutoLaunchDashboard, cfg) {
		autoLaunchDashboard(p.Slop)
	}

	// Optional network-exposed dashboard listener (LAN / mesh / tunnel),
	// off unless remote_access.enabled + bind are set. A SEPARATE HTTPS
	// server with mTLS + passphrase auth; the loopback dashboard above is
	// untouched. Best-effort: a missing material / failed bind is logged and
	// surfaced on the banner, never fatal — the rest of the daemon runs.
	var remoteSrv *http.Server
	if cfg.RemoteAccessEnabled() {
		if rs, err := startRemoteServer(cfg.RemoteAccessBind()); err != nil {
			slog.Error("remote-access: failed to start listener", "bind", cfg.RemoteAccessBind(), "err", err)
			fmt.Fprintf(os.Stderr, "  remote access: FAILED to start on %s: %v\n", cfg.RemoteAccessBind(), err)
		} else {
			remoteSrv = rs
			slog.Info("remote-access listener started", "bind", cfg.RemoteAccessBind())
			fmt.Printf("  remote access (mTLS+passphrase): https://%s\n", cfg.RemoteAccessBind())
		}
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

	// Branch-history PR enrichment — off by default. When on, the
	// dashboard's branch-link resolver also stamps resolved PRs onto
	// the conv_branch_history table. The branch re-scan and hook append
	// run regardless. See branchlinks.go. Reset before the conditional
	// assignment so a config without an `agent` section can't leave a
	// stale value from an earlier resolve.
	branchHistoryPREnrichment = false
	if cfg != nil && cfg.Agent != nil {
		branchHistoryPREnrichment = cfg.Agent.BranchHistoryPREnrichment
	}
	slog.Info("branch-history PR enrichment", "enabled", branchHistoryPREnrichment)

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

	// audit_log retention sweep (JOH-268). Hard-deletes audit rows older
	// than the configured retention window (default 30 days) so the command
	// trail stays bounded. Shares the daemon-wide stop channel. See
	// audit_cleanup.go.
	startAuditLogCleanup(cronStop)

	// Session reaper. Sweeps for sessions whose tmux session + process
	// are gone and stamps status=exited — a crashed or kill -9'd agent
	// fires no SessionEnd hook, so without this its row would stay
	// frozen at its last hook status. Shares the daemon-wide stop
	// channel.
	startSessionReaper(cronStop)

	// Export-job housekeeping (JOH-265). Times out requested/running export
	// jobs whose agent never delivered, and TTL-prunes terminal jobs + their
	// on-disk artifacts under ~/.tclaude/exports. Shares the stop channel.
	startExportJobsCleanup(cronStop)

	// Pending-spawn sweeper. Finishes enrollment for non-blocking spawns
	// whose conv-id materialised only after the spawn returned PENDING —
	// the JOH-205 inc2 back-fill: a Codex held behind a startup gate
	// (untrusted dir / new-hooks-config / OpenAI auth modal) takes no first
	// turn, so its conv-id never appears synchronously; once the operator
	// clears the gate and the first turn lands, this sweep enrolls the agent
	// and drops the pending_spawns row. Restart-safe. Shares the daemon-wide
	// stop channel.
	startPendingSpawnSweeper(cronStop)

	// Size-based rotation of ~/.tclaude/output.log. agentd holds the
	// log fd for its whole life, so rotation renames the file and
	// reopens a fresh one in-process. Shares the daemon-wide stop
	// channel. See logrotate.go.
	startLogRotation(cronStop, common.ActiveLogRotator(), cfg)

	// Plugin status checker. Re-probes every plugins.json step-check
	// each minute and caches the results, so the dashboard's Plugins
	// tab + warning badge stay fresh without the 2s snapshot poll ever
	// spawning a subprocess. Shares the daemon-wide stop channel.
	startPluginChecker(cronStop)

	// Subscription-usage poller. Keeps the SQLite usage_cache row fresh
	// so the dashboard's top-bar 5h/7d readout stays current even when
	// no Claude Code statusbar is running to populate it. Side-effect
	// only and cheap — usageapi.GetCached's own TTL keeps API hits rare.
	// Shares the daemon-wide stop channel.
	startUsagePoller(cronStop)

	// Codex subscription-usage poller. Codex has no usage API wired into
	// tclaude, so this lifts the 5h/weekly rate limits off Codex's local
	// rollout files into an in-memory snapshot the dashboard reads beside
	// the Claude figures. Cheap (only recently-touched rollouts are read)
	// and a no-op when Codex isn't installed. Shares the daemon-wide stop
	// channel.
	startCodexUsagePoller(cronStop)

	// Live conv_index monitor. One fsnotify watcher over
	// ~/.claude/projects/ keeps the conv_index SQLite cache fresh as
	// conversation .jsonl files change, so the dashboard (and any other
	// reader) can trust cached rows instead of re-stat+reparsing each
	// .jsonl on every access. Shares the daemon-wide stop channel.
	startConvMonitor(cronStop)

	// Online conversations are enrolled as agents by the session reaper's
	// continuous liveness sweep (its first tick fires immediately at
	// startup) — see enrollOnlineSession. That subsumes the one-shot
	// startup reconcile this used to run and keeps enrolling convs that
	// come online later, so terminal-launched sessions (`tclaude conv
	// new`) surface on the dashboard like web-UI spawns do.

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
		printOperatorTokenBanner(operatorTok, tokenSrc)
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

	if shouldDisableTray(p.NoTray, cfg) {
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
	if remoteSrv != nil {
		_ = remoteSrv.Shutdown(ctx)
	}
	return nil
}

// shouldDisableTray reports whether `tclaude agentd serve` should skip
// the system tray icon. The --no-tray flag (flagSet) and the persistent
// agent.disable_tray config field OR together — either one suppresses
// the tray — so a service/autostart launch can stay tray-less without
// carrying the flag. Mirrors shouldAutoLaunchDashboard (inverted polarity).
func shouldDisableTray(flagSet bool, cfg *config.Config) bool {
	if flagSet {
		return true
	}
	return cfg != nil && cfg.Agent != nil && cfg.Agent.DisableTray
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

// resolveDashboardPort resolves the loopback port the dashboard +
// human-approval popup bind to. Priority, highest first:
//
//  1. the --dashboard-port serve flag (when > 0)
//  2. the agent.dashboard_port config.json field (when > 0)
//  3. 0 — let the OS pick a random free port (the default)
//
// A non-positive value at a tier means "not set here" and resolution
// falls through to the next tier; passing 0 on the flag therefore can't
// force a random port over a configured one, which is the right default
// (a fixed config port is the deliberate setting). An out-of-range
// positive value is returned as-is and surfaces as a fatal bind error in
// startPopupServer — an explicitly requested port that can't be honoured
// must crash startup, not silently fall back (the config editor's
// Validate catches the same mistake earlier, with a friendly message).
// Returns the resolved port (0 = random) and the tier it came from, for
// the startup log line + the bind-error message.
func resolveDashboardPort(flagValue int, cfg *config.Config) (int, string) {
	if flagValue > 0 {
		return flagValue, "flag"
	}
	if cfg != nil && cfg.Agent != nil && cfg.Agent.DashboardPort > 0 {
		return cfg.Agent.DashboardPort, "config"
	}
	return 0, "default (random)"
}

func buildMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/info", handleInfo)
	mux.HandleFunc("/v1/dashboard/open", handleDashboardOpen)
	mux.HandleFunc("/v1/whoami", handleWhoami)
	mux.HandleFunc("/v1/whoami/rename", handleWhoamiRename)
	mux.HandleFunc("/v1/whoami/compact", handleWhoamiCompact)
	mux.HandleFunc("/v1/whoami/remote-control", handleWhoamiRemoteControl)
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
	mux.HandleFunc("POST /v1/groups/import", handleGroupImport)
	mux.HandleFunc("POST /v1/groups/import/inspect", handleGroupImportInspect)
	mux.HandleFunc("GET /v1/groups/transfers", handleGroupTransfers)
	registerV1GroupRoutes(mux)
	// Per-agent export (JOH-265). The agent half: `tclaude agent export
	// show` reads the brief, `… submit` uploads the artifact. Self-scoped —
	// requireExportJobAccess gates each on owning the job.
	mux.HandleFunc("GET /v1/export-jobs/{id}", handleExportShow)
	mux.HandleFunc("POST /v1/export-jobs/{id}/artifact", handleExportSubmit)
	// Group templates. The bare /v1/templates and /v1/templates/{name}
	// patterns dispatch their own methods; the two POST-specific routes
	// carry a literal segment ("from-group" / "instantiate") so the mux
	// picks them over the {name} wildcard without ambiguity.
	mux.HandleFunc("/v1/templates", handleTemplates)
	mux.HandleFunc("POST /v1/templates/from-group", handleTemplateFromGroup)
	mux.HandleFunc("POST /v1/templates/{name}/instantiate", handleTemplateInstantiate)
	mux.HandleFunc("/v1/templates/{name}", handleTemplateByName)
	// Spawn profiles (JOH-210). Reads open, writes gated on profiles.manage.
	mux.HandleFunc("/v1/spawn-profiles", handleSpawnProfiles)
	mux.HandleFunc("/v1/spawn-profiles/{name}", handleSpawnProfileByName)
	mux.HandleFunc("/v1/claude-settings/default-model", handleClaudeDefaultModel)
	mux.HandleFunc("/v1/links", handleLinksAll)
	mux.HandleFunc("/v1/can-message", handleCanMessage)
	mux.HandleFunc("/v1/permissions", handlePermissions)
	mux.HandleFunc("/v1/permissions/slugs", handlePermissionsSlugs)
	mux.HandleFunc("/v1/permissions/grant", handlePermissionsGrant)
	mux.HandleFunc("/v1/permissions/deny", handlePermissionsDeny)
	mux.HandleFunc("/v1/permissions/revoke", handlePermissionsRevoke)
	mux.HandleFunc("/v1/cron", handleCron)
	mux.HandleFunc("/v1/cron/", handleCronByID)
	// Sudo: most-specific path goes first so the trailing-slash form
	// catches /v1/sudo/{id} and the bare form catches POST/GET/DELETE
	// against the collection.
	mux.HandleFunc("/v1/sudo", handleSudo)
	mux.HandleFunc("/v1/sudo/", handleSudoByID)
	mux.HandleFunc("/v1/notify-human", handleNotifyHuman)
	return logRequest(auditRequests(mux))
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

// Hijack forwards to the wrapped ResponseWriter's http.Hijacker, which Go
// does NOT promote automatically: statusRec embeds the http.ResponseWriter
// *interface*, and method promotion through an embedded interface only
// forwards methods declared on that interface (Header/Write/WriteHeader) —
// never extra methods the concrete value underneath happens to implement.
// Without this, gorilla/websocket's Upgrade (which type-asserts for
// http.Hijacker) fails on every request routed through logRequest /
// auditRequests, breaking the dashboard's in-browser terminal WebSocket.
func (r *statusRec) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("statusRec: underlying ResponseWriter does not implement http.Hijacker")
	}
	return h.Hijack()
}
