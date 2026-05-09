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
	"sync"
	"syscall"
	"time"

	"fyne.io/systray"
	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
)

type serveParams struct {
	Socket string `long:"socket" short:"s" optional:"true" help:"Unix socket path (default ~/.tclaude/agentd.sock)"`
	NoTray bool   `long:"no-tray" help:"Don't show a system tray icon. Use on headless / CI hosts."`
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

	// Both the Unix-socket server and the popup server run in
	// goroutines so the main goroutine is free for the tray loop
	// (systray needs the main thread on every supported platform).
	serveErrCh := make(chan error, 1)
	go func() {
		slog.Info("agentd listening", "socket", sockPath, "popup", popupBaseURL)
		fmt.Printf("tclaude agentd listening on %s\n", sockPath)
		if popupBaseURL != "" {
			fmt.Printf("  human-approval popup on %s/approve/<id>\n", popupBaseURL)
			fmt.Printf("  agent dashboard on        %s/\n", popupBaseURL)
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

func buildMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/info", handleInfo)
	mux.HandleFunc("/v1/whoami", handleWhoami)
	mux.HandleFunc("/v1/whoami/rename", handleWhoamiRename)
	mux.HandleFunc("/v1/whoami/compact", handleWhoamiCompact)
	mux.HandleFunc("/v1/whoami/reincarnate", handleWhoamiReincarnate)
	mux.HandleFunc("/v1/whoami/context", handleWhoamiContext)
	mux.HandleFunc("/v1/lookup", handleLookup)
	mux.HandleFunc("/v1/peers", handlePeers)
	mux.HandleFunc("/v1/messages", handleMessages)
	mux.HandleFunc("/v1/messages/", handleMessageByIDOrReply)
	mux.HandleFunc("/v1/inbox", handleInbox)
	mux.HandleFunc("/v1/inbox/prune", handleInboxPrune)
	mux.HandleFunc("/v1/groups", handleGroups)
	mux.HandleFunc("/v1/groups/", handleGroupByName)
	mux.HandleFunc("/v1/permissions", handlePermissions)
	mux.HandleFunc("/v1/permissions/slugs", handlePermissionsSlugs)
	mux.HandleFunc("/v1/permissions/grant", handlePermissionsGrant)
	mux.HandleFunc("/v1/permissions/revoke", handlePermissionsRevoke)
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
