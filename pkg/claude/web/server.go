package web

import (
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"

	"github.com/creack/pty"
	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func serve(bind string, port int, user, pass, tmuxSession string, useTLS bool) error {
	mux := http.NewServeMux()

	auth := basicAuth(user, pass)

	mux.HandleFunc("/", auth(handleIndex))
	mux.HandleFunc("/ws", auth(handleWS(tmuxSession)))

	addr := fmt.Sprintf("%s:%d", bind, port)
	server := &http.Server{Addr: addr, Handler: mux}

	// Graceful shutdown on Ctrl+C
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		fmt.Println("\nShutting down...")
		server.Close()
	}()

	if useTLS {
		tlsConfig, fingerprint, err := loadOrGenerateCert()
		if err != nil {
			return fmt.Errorf("failed to generate TLS certificate: %w", err)
		}
		fmt.Printf("  fingerprint: %s\n", fingerprint)
		server.TLSConfig = tlsConfig

		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return err
		}
		tlsLn := tls.NewListener(ln, tlsConfig)
		return server.Serve(tlsLn)
	}

	return server.ListenAndServe()
}

func basicAuth(user, pass string) func(http.HandlerFunc) http.HandlerFunc {
	return func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			u, p, ok := r.BasicAuth()
			if !ok ||
				subtle.ConstantTimeCompare([]byte(u), []byte(user)) != 1 ||
				subtle.ConstantTimeCompare([]byte(p), []byte(pass)) != 1 {
				w.Header().Set("WWW-Authenticate", `Basic realm="tofu claude web"`)
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
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
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Printf("websocket upgrade: %v", err)
			return
		}
		defer conn.Close()

		// Set window-size to smallest so all clients (desktop + phone)
		// see the same content fitted to the smallest screen
		exec.Command("tmux", "set-option", "-t", tmuxSession, "window-size", "smallest").Run()

		// Spawn tmux attach in a PTY
		cmd := exec.Command("tmux", "attach-session", "-t", tmuxSession)
		cmd.Env = append(os.Environ(), "TERM=xterm-256color")

		ptmx, err := pty.Start(cmd)
		if err != nil {
			log.Printf("pty start: %v", err)
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
