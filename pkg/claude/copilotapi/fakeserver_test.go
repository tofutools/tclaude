package copilotapi

import (
	"bufio"
	"encoding/json"
	"net"
	"sync"
	"testing"
)

// handlerFunc answers one request. Returning a nil error sends result;
// returning an *Error sends that error object instead.
type handlerFunc func(params json.RawMessage) (any, *Error)

// fakeServer is an in-process stand-in for Copilot's embedded server. It
// speaks the same framing and JSON-RPC shapes so client tests exercise the
// real codec and correlation paths rather than a mock of them.
type fakeServer struct {
	t        *testing.T
	listener net.Listener

	mu       sync.Mutex
	handlers map[string]handlerFunc
	conns    []net.Conn
	// requests records every method the server was asked for, in order.
	requests []string
	// clientReplies collects responses the client sent back to server-issued
	// requests.
	clientReplies chan *rpcMessage
}

func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := &fakeServer{
		t:             t,
		listener:      listener,
		handlers:      map[string]handlerFunc{},
		clientReplies: make(chan *rpcMessage, 8),
	}
	// A working handshake and liveness check by default; tests that care
	// override them.
	server.handle(MethodConnect, func(json.RawMessage) (any, *Error) {
		return ConnectResult{OK: true, ProtocolVersion: SupportedProtocolVersion, Version: "1.0.78"}, nil
	})
	server.handle(MethodPing, func(params json.RawMessage) (any, *Error) {
		var request PingParams
		_ = json.Unmarshal(params, &request)
		return PingResult{Message: "pong: " + request.Message, ProtocolVersion: SupportedProtocolVersion}, nil
	})
	go server.acceptLoop()
	t.Cleanup(server.close)
	return server
}

func (s *fakeServer) addr() string { return s.listener.Addr().String() }

func (s *fakeServer) handle(method string, handler handlerFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[method] = handler
}

func (s *fakeServer) methodsSeen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.requests...)
}

func (s *fakeServer) acceptLoop() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.conns = append(s.conns, conn)
		s.mu.Unlock()
		go s.serve(conn)
	}
}

func (s *fakeServer) serve(conn net.Conn) {
	reader := bufio.NewReader(conn)
	for {
		frame, err := readFrame(reader)
		if err != nil {
			return
		}
		var message rpcMessage
		if err := json.Unmarshal(frame, &message); err != nil {
			return
		}
		if message.Method == "" {
			// A response to something we sent the client.
			select {
			case s.clientReplies <- &message:
			default:
			}
			continue
		}
		s.mu.Lock()
		s.requests = append(s.requests, message.Method)
		handler, ok := s.handlers[message.Method]
		s.mu.Unlock()
		if message.ID == nil {
			continue // a notification; nothing to answer
		}
		// Each request is answered on its own goroutine so a handler can
		// delay and force replies to arrive out of order.
		go func(id int64, params json.RawMessage) {
			if !ok {
				s.respond(conn, id, nil, &Error{Code: CodeMethodNotFound, Message: "Unhandled method"})
				return
			}
			result, rpcErr := handler(params)
			s.respond(conn, id, result, rpcErr)
		}(*message.ID, message.Params)
	}
}

func (s *fakeServer) respond(conn net.Conn, id int64, result any, rpcErr *Error) {
	message := rpcMessage{JSONRPC: "2.0", ID: &id, Error: rpcErr}
	if rpcErr == nil {
		encoded, err := json.Marshal(result)
		if err != nil {
			s.t.Errorf("marshal result: %v", err)
			return
		}
		message.Result = encoded
	}
	s.send(conn, message)
}

func (s *fakeServer) send(conn net.Conn, message rpcMessage) {
	encoded, err := json.Marshal(message)
	if err != nil {
		s.t.Errorf("marshal message: %v", err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = writeFrame(conn, encoded)
}

// notify pushes a notification to every connected client.
func (s *fakeServer) notify(method string, params any) {
	encoded, err := json.Marshal(params)
	if err != nil {
		s.t.Errorf("marshal params: %v", err)
		return
	}
	s.mu.Lock()
	conns := append([]net.Conn(nil), s.conns...)
	s.mu.Unlock()
	for _, conn := range conns {
		s.send(conn, rpcMessage{JSONRPC: "2.0", Method: method, Params: encoded})
	}
}

// request sends a server-to-client request, which real Copilot does for
// permission and user-input prompts.
func (s *fakeServer) request(id int64, method string) {
	s.mu.Lock()
	conns := append([]net.Conn(nil), s.conns...)
	s.mu.Unlock()
	for _, conn := range conns {
		s.send(conn, rpcMessage{JSONRPC: "2.0", ID: &id, Method: method, Params: json.RawMessage(`{}`)})
	}
}

// hangUp drops every client connection, standing in for the pane's Copilot
// process dying.
func (s *fakeServer) hangUp() {
	s.mu.Lock()
	conns := s.conns
	s.conns = nil
	s.mu.Unlock()
	for _, conn := range conns {
		_ = conn.Close()
	}
}

func (s *fakeServer) close() {
	_ = s.listener.Close()
	s.hangUp()
}
