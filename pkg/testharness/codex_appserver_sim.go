package testharness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/tofutools/tclaude/pkg/claude/codexappserver"
)

const codexAppServerSimQueue = 256

// CodexAppServerMessage is one client request or notification observed by a
// CodexAppServerSim. ID is absent for notifications.
type CodexAppServerMessage struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

// CodexAppServerSim is a small WebSocket-over-Unix fake for client unit tests
// and later agentd flow tests. It answers initialize accurately, records every
// client message, and leaves all other replies and server pushes under test
// control.
type CodexAppServerSim struct {
	socketPath string
	listener   net.Listener
	server     *http.Server
	messages   chan CodexAppServerMessage

	mu      sync.Mutex
	conn    *websocket.Conn
	nextID  int64
	closed  bool
	writeMu sync.Mutex

	InitializeResult codexappserver.InitializeResult
}

// StartCodexAppServerSim binds a fresh Unix socket. The caller owns socketPath
// and should call Close; an existing path is never removed or replaced.
func StartCodexAppServerSim(socketPath string) (*CodexAppServerSim, error) {
	if _, err := os.Lstat(socketPath); err == nil {
		return nil, fmt.Errorf("codex app-server fake socket already exists: %s", socketPath)
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("inspect Codex app-server fake socket: %w", err)
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("listen on Codex app-server fake socket: %w", err)
	}
	if err := os.Chmod(socketPath, 0o600); err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("chmod Codex app-server fake socket: %w", err)
	}
	sim := &CodexAppServerSim{
		socketPath: socketPath,
		listener:   listener,
		messages:   make(chan CodexAppServerMessage, codexAppServerSimQueue),
		InitializeResult: codexappserver.InitializeResult{
			UserAgent:      "codex_app_server/0.147.0",
			CodexHome:      "/fake/codex-home",
			PlatformFamily: "unix",
			PlatformOS:     "linux",
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", sim.serveWebSocket)
	sim.server = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = sim.server.Serve(listener) }()
	return sim, nil
}

func (s *CodexAppServerSim) SocketPath() string                     { return s.socketPath }
func (s *CodexAppServerSim) Messages() <-chan CodexAppServerMessage { return s.messages }

func (s *CodexAppServerSim) serveWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
	if err != nil {
		return
	}
	conn.SetReadLimit(codexappserver.DefaultMaxMessageBytes)
	s.mu.Lock()
	if s.closed || s.conn != nil {
		s.mu.Unlock()
		_ = conn.Close()
		return
	}
	s.conn = conn
	s.mu.Unlock()
	s.readLoop(conn)
}

func (s *CodexAppServerSim) readLoop(conn *websocket.Conn) {
	defer func() {
		s.mu.Lock()
		if s.conn == conn {
			s.conn = nil
		}
		s.mu.Unlock()
		_ = conn.Close()
	}()
	for {
		messageType, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if messageType != websocket.TextMessage {
			return
		}
		var message CodexAppServerMessage
		if err := json.Unmarshal(data, &message); err != nil || message.Method == "" {
			return
		}
		select {
		case s.messages <- message:
		default:
			return
		}
		if message.Method == codexappserver.MethodInitialize && len(message.ID) != 0 {
			if err := s.Reply(message.ID, s.InitializeResult); err != nil {
				return
			}
		}
	}
}

// Reply answers one client request.
func (s *CodexAppServerSim) Reply(id json.RawMessage, result any) error {
	return s.writeJSON(struct {
		ID     json.RawMessage `json:"id"`
		Result any             `json:"result"`
	}{ID: id, Result: result})
}

func (s *CodexAppServerSim) ReplyError(id json.RawMessage, rpcErr codexappserver.RPCError) error {
	return s.writeJSON(struct {
		ID    json.RawMessage         `json:"id"`
		Error codexappserver.RPCError `json:"error"`
	}{ID: id, Error: rpcErr})
}

func (s *CodexAppServerSim) SendNotification(method string, params any) error {
	return s.writeJSON(struct {
		Method string `json:"method"`
		Params any    `json:"params"`
	}{Method: method, Params: params})
}

// SendRequest pushes a server-initiated request. A production M1 client will
// surface it and immediately quarantine its connection without replying.
func (s *CodexAppServerSim) SendRequest(method string, params any) (int64, error) {
	s.mu.Lock()
	s.nextID++
	id := s.nextID
	s.mu.Unlock()
	err := s.writeJSON(struct {
		ID     int64  `json:"id"`
		Method string `json:"method"`
		Params any    `json:"params"`
	}{ID: id, Method: method, Params: params})
	return id, err
}

func (s *CodexAppServerSim) SendRaw(messageType int, data []byte) error {
	return s.write(messageType, data)
}

func (s *CodexAppServerSim) writeJSON(value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return s.write(websocket.TextMessage, data)
}

func (s *CodexAppServerSim) write(messageType int, data []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.mu.Lock()
	conn := s.conn
	closed := s.closed
	s.mu.Unlock()
	if closed || conn == nil {
		return errors.New("codex app-server fake has no connected client")
	}
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	err := conn.WriteMessage(messageType, data)
	_ = conn.SetWriteDeadline(time.Time{})
	return err
}

func (s *CodexAppServerSim) CloseClient() error {
	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()
	if conn == nil {
		return nil
	}
	return conn.Close()
}

func (s *CodexAppServerSim) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	conn := s.conn
	s.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := s.server.Shutdown(ctx)
	_ = s.listener.Close()
	return err
}
