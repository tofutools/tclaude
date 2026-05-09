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
	"syscall"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
)

type serveParams struct {
	Socket string `long:"socket" short:"s" optional:"true" help:"Unix socket path (default ~/.tclaude/agentd.sock)"`
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
	// listening.
	if _, err := os.Stat(sockPath); err == nil {
		if c, derr := net.Dial("unix", sockPath); derr == nil {
			_ = c.Close()
			return fmt.Errorf("agentd is already listening on %s", sockPath)
		}
		_ = os.Remove(sockPath)
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

	done := make(chan error, 1)
	go func() {
		slog.Info("agentd listening", "socket", sockPath)
		fmt.Printf("tclaude agentd listening on %s\n", sockPath)
		done <- srv.Serve(ln)
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	select {
	case err := <-done:
		if err != nil && err != http.ErrServerClosed {
			return err
		}
	case <-sig:
		slog.Info("agentd shutting down")
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}
	return nil
}

func buildMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/whoami", handleWhoami)
	mux.HandleFunc("/v1/lookup", handleLookup)
	mux.HandleFunc("/v1/peers", handlePeers)
	mux.HandleFunc("/v1/messages", handleMessages)
	mux.HandleFunc("/v1/messages/", handleMessageByIDOrReply)
	mux.HandleFunc("/v1/inbox", handleInbox)
	mux.HandleFunc("/v1/groups", handleGroups)
	mux.HandleFunc("/v1/groups/", handleGroupByName)
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
