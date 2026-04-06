package web

import (
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/creack/pty"
	"github.com/gorilla/websocket"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// preparedServer holds a server ready to serve on an already-bound listener.
type preparedServer struct {
	server   *http.Server
	listener net.Listener
}

// prepare creates the HTTP server, binds the TCP listener, and sets up TLS if needed.
// The listener is bound immediately so the port is reserved before any output is printed.
func prepare(bind string, port int, user, pass, tmuxSession string, useTLS bool) (*preparedServer, error) {
	mux := http.NewServeMux()
	auth := basicAuth(user, pass)
	mux.HandleFunc("/", auth(handleIndex))
	mux.HandleFunc("/ws", auth(handleWS(tmuxSession)))

	addr := fmt.Sprintf("%s:%d", bind, port)
	server := &http.Server{
		Addr:     addr,
		Handler:  mux,
		ErrorLog: log.New(io.Discard, "", 0), // suppress noisy TLS handshake errors from net/http
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}

	if useTLS {
		tlsConfig, fingerprint, err := loadOrGenerateCert()
		if err != nil {
			ln.Close()
			return nil, fmt.Errorf("failed to generate TLS certificate: %w", err)
		}
		fmt.Printf("  fingerprint: %s\n", fingerprint)
		server.TLSConfig = tlsConfig
		ln = tls.NewListener(ln, tlsConfig)
	}

	return &preparedServer{server: server, listener: ln}, nil
}

// serve starts accepting connections and blocks until shutdown.
func (ps *preparedServer) serve() error {
	// Graceful shutdown on Ctrl+C
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		fmt.Println("\nShutting down...")
		ps.server.Close()
	}()

	return ps.server.Serve(ps.listener)
}

func clientAddr(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		return fwd
	}
	return r.RemoteAddr
}

func basicAuth(user, pass string) func(http.HandlerFunc) http.HandlerFunc {
	return func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			u, p, ok := r.BasicAuth()
			if !ok ||
				subtle.ConstantTimeCompare([]byte(u), []byte(user)) != 1 ||
				subtle.ConstantTimeCompare([]byte(p), []byte(pass)) != 1 {
				slog.Warn("auth failed", "method", r.Method, "path", r.URL.Path, "remote", clientAddr(r))
				w.Header().Set("WWW-Authenticate", `Basic realm="tclaude web"`)
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
			slog.Info("authenticated", "method", r.Method, "path", r.URL.Path, "remote", clientAddr(r))
			next(w, r)
		}
	}
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(indexHTML))
}

// resizeMsg is sent from the browser when the terminal is resized
type resizeMsg struct {
	Type string `json:"type"`
	Cols int    `json:"cols"`
	Rows int    `json:"rows"`
}

func handleWS(tmuxSession string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		addr := clientAddr(r)
		slog.Info("client connected", "remote", addr)

		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			slog.Error("websocket upgrade failed", "remote", addr, "error", err)
			return
		}
		defer func() {
			conn.Close()
			slog.Info("client disconnected", "remote", addr)
		}()

		// Set window-size to latest so the most recently active client
		// dictates the size - avoids dots filling the phone screen
		clcommon.TmuxCommand("set-option", "-t", tmuxSession, "window-size", "latest").Run()

		// Spawn tmux attach in a PTY
		cmd := clcommon.TmuxCommand("attach-session", "-t", tmuxSession)
		cmd.Env = append(os.Environ(), "TERM=xterm-256color")

		ptmx, err := pty.Start(cmd)
		if err != nil {
			slog.Error("pty start failed", "error", err)
			conn.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf("Error: %v\r\n", err)))
			return
		}
		defer func() {
			ptmx.Close()
			cmd.Process.Signal(syscall.SIGHUP)
			cmd.Wait()
		}()

		var wg sync.WaitGroup

		// PTY -> WebSocket
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, 4096)
			for {
				n, err := ptmx.Read(buf)
				if err != nil {
					return
				}
				if err := conn.WriteMessage(websocket.BinaryMessage, buf[:n]); err != nil {
					return
				}
			}
		}()

		// WebSocket -> PTY (input + resize)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				msgType, data, err := conn.ReadMessage()
				if err != nil {
					// Client disconnected - detach from tmux cleanly
					ptmx.Close()
					return
				}

				if msgType == websocket.TextMessage {
					// Check if it's a resize message
					var msg resizeMsg
					if json.Unmarshal(data, &msg) == nil && msg.Type == "resize" {
						if msg.Cols > 0 && msg.Rows > 0 {
							pty.Setsize(ptmx, &pty.Winsize{
								Cols: uint16(msg.Cols),
								Rows: uint16(msg.Rows),
							})
						}
						continue
					}
				}

				// Regular input
				ptmx.Write(data)
			}
		}()

		wg.Wait()
	}
}
