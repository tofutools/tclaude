package codexappserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/tofutools/tclaude/pkg/common/buildversion"
)

const (
	DefaultNotificationBuffer  = 256
	DefaultServerRequestBuffer = 4
	DefaultMaxMessageBytes     = 16 << 20
	defaultWriteTimeout        = 30 * time.Second
	defaultHandshakeTimeout    = 15 * time.Second
)

var codexVersionRE = regexp.MustCompile(`(?i)codex[^0-9]*([0-9]+)\.([0-9]+)\.([0-9]+)`) //nolint:gochecknoglobals // immutable protocol parser

type Options struct {
	// ClientVersion overrides the truthful tclaude build version used in the
	// initialize clientInfo. Empty uses buildversion.AppVersion().
	ClientVersion string
	// CodexVersion is the version proven from the exact binary that launched
	// app-server. When empty, Dial requires a parseable Codex version in the
	// initialize response's userAgent. When both exist they must agree.
	CodexVersion        string
	NotificationBuffer  int
	ServerRequestBuffer int
	MaxMessageBytes     int64
	WriteTimeout        time.Duration
	HandshakeTimeout    time.Duration
	// BearerToken authenticates the WebSocket upgrade. It is never included in
	// errors or retained separately from the live connection.
	BearerToken string
	// DialContext exists for focused transport tests. Production leaves it nil
	// and the client dials socketPath as a Unix socket.
	DialContext func(context.Context, string, string) (net.Conn, error)
}

func (o *Options) normalized() Options {
	var out Options
	if o != nil {
		out = *o
	}
	if out.ClientVersion == "" {
		out.ClientVersion = buildversion.AppVersion()
	}
	if out.NotificationBuffer <= 0 {
		out.NotificationBuffer = DefaultNotificationBuffer
	}
	if out.ServerRequestBuffer <= 0 {
		out.ServerRequestBuffer = DefaultServerRequestBuffer
	}
	if out.MaxMessageBytes <= 0 {
		out.MaxMessageBytes = DefaultMaxMessageBytes
	}
	if out.WriteTimeout <= 0 {
		out.WriteTimeout = defaultWriteTimeout
	}
	if out.HandshakeTimeout <= 0 {
		out.HandshakeTimeout = defaultHandshakeTimeout
	}
	return out
}

type wireMessage struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *RPCError       `json:"error,omitempty"`
}

// Client owns one initialized Codex app-server WebSocket connection.
type Client struct {
	conn         *websocket.Conn
	socketPath   string
	writeTimeout time.Duration

	writeGate chan struct{}
	mu        sync.Mutex
	nextID    int64
	pending   map[string]chan *wireMessage
	closed    bool
	closeErr  error

	done           chan struct{}
	notifications  chan Notification
	serverRequests chan ServerRequest

	initializeResult InitializeResult
	codexVersion     string
}

// Dial connects over socketPath, validates the Codex 0.147 compatibility
// window, and completes initialize followed by initialized.
func Dial(ctx context.Context, socketPath string, opts *Options) (*Client, error) {
	if strings.TrimSpace(socketPath) == "" {
		return nil, errors.New("codexappserver: Unix socket path is empty")
	}
	options := opts.normalized()
	canonicalVersion := ""
	if options.CodexVersion != "" {
		var err error
		canonicalVersion, err = normalizeCodexVersion(options.CodexVersion)
		if err != nil {
			return nil, err
		}
		if err := CheckVersion(canonicalVersion); err != nil {
			return nil, err
		}
	}

	dialContext := options.DialContext
	if dialContext == nil {
		dialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
		}
	}
	dialer := websocket.Dialer{
		HandshakeTimeout: options.HandshakeTimeout,
		NetDialContext:   dialContext,
	}
	headers := http.Header{}
	if options.BearerToken != "" {
		headers.Set("Authorization", "Bearer "+options.BearerToken)
	}
	conn, response, err := dialer.DialContext(ctx, "ws://localhost/", headers)
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		return nil, fmt.Errorf("codexappserver: dial Unix socket %s: %w", socketPath, err)
	}
	conn.SetReadLimit(options.MaxMessageBytes)
	c := &Client{
		conn: conn, socketPath: socketPath, writeTimeout: options.WriteTimeout,
		writeGate: make(chan struct{}, 1),
		pending:   make(map[string]chan *wireMessage), done: make(chan struct{}),
		notifications:  make(chan Notification, options.NotificationBuffer),
		serverRequests: make(chan ServerRequest, options.ServerRequestBuffer),
	}
	go c.readLoop()

	title := "tclaude Codex app-server client"
	params := InitializeParams{ClientInfo: ClientInfo{
		Name: "tclaude", Title: &title, Version: options.ClientVersion,
	}}
	var initialized InitializeResult
	if err := c.Call(ctx, MethodInitialize, params, &initialized); err != nil {
		c.shutdown(err)
		return nil, fmt.Errorf("codexappserver: initialize: %w", err)
	}
	if err := validateInitializeResult(initialized); err != nil {
		c.shutdown(err)
		return nil, err
	}
	reportedVersion, found := versionFromUserAgent(initialized.UserAgent)
	version := canonicalVersion
	if version == "" {
		if !found {
			err := fmt.Errorf("%w: initialize userAgent %q does not identify the Codex version",
				ErrUnsupportedVersion, initialized.UserAgent)
			c.shutdown(err)
			return nil, err
		}
		version = reportedVersion
	} else if found && reportedVersion != version {
		err := fmt.Errorf("%w: launched binary reports %s but app-server userAgent reports %s",
			ErrUnsupportedVersion, version, reportedVersion)
		c.shutdown(err)
		return nil, err
	}
	if err := CheckVersion(version); err != nil {
		c.shutdown(err)
		return nil, err
	}
	if err := c.Notify(MethodInitialized, struct{}{}); err != nil {
		c.shutdown(err)
		return nil, fmt.Errorf("codexappserver: initialized notification: %w", err)
	}
	c.initializeResult = initialized
	c.codexVersion = version
	return c, nil
}

func validateInitializeResult(result InitializeResult) error {
	if result.UserAgent == "" || result.CodexHome == "" ||
		result.PlatformFamily == "" || result.PlatformOS == "" {
		return fmt.Errorf("%w: initialize response is missing required server identity fields", ErrProtocol)
	}
	return nil
}

func versionFromUserAgent(userAgent string) (string, bool) {
	m := codexVersionRE.FindStringSubmatch(userAgent)
	if len(m) != 4 {
		return "", false
	}
	return strings.Join(m[1:], "."), true
}

// CheckVersion fails closed outside the schema-validated M1 range.
func CheckVersion(version string) error {
	canonical, err := normalizeCodexVersion(version)
	if err != nil {
		return err
	}
	parts := strings.Split(canonical, ".")
	major, _ := strconv.Atoi(parts[0])
	minor, _ := strconv.Atoi(parts[1])
	if major != 0 || minor != 147 {
		return fmt.Errorf("%w: %s (need >=%s,<%s)", ErrUnsupportedVersion,
			version, MinimumCodexVersion, MaximumCodexVersion)
	}
	return nil
}

func normalizeCodexVersion(version string) (string, error) {
	trimmed := strings.TrimSpace(version)
	trimmed = strings.TrimSpace(strings.TrimPrefix(trimmed, "codex-cli "))
	parts := strings.Split(trimmed, ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("%w: %q (need >=%s,<%s)", ErrUnsupportedVersion,
			version, MinimumCodexVersion, MaximumCodexVersion)
	}
	for i, part := range parts {
		value, err := strconv.Atoi(part)
		if err != nil || value < 0 {
			return "", fmt.Errorf("%w: %q (need >=%s,<%s)", ErrUnsupportedVersion,
				version, MinimumCodexVersion, MaximumCodexVersion)
		}
		parts[i] = strconv.Itoa(value)
	}
	return strings.Join(parts, "."), nil
}

func (c *Client) SocketPath() string                   { return c.socketPath }
func (c *Client) CodexVersion() string                 { return c.codexVersion }
func (c *Client) InitializeResult() InitializeResult   { return c.initializeResult }
func (c *Client) Done() <-chan struct{}                { return c.done }
func (c *Client) Notifications() <-chan Notification   { return c.notifications }
func (c *Client) ServerRequests() <-chan ServerRequest { return c.serverRequests }

func (c *Client) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closeErr
}

// Close sends a normal WebSocket close control frame and terminates all calls.
func (c *Client) Close() error {
	_ = c.conn.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
		time.Now().Add(time.Second))
	c.shutdown(ErrClosed)
	return nil
}

func (c *Client) shutdown(cause error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.closeErr = cause
	pending := c.pending
	c.pending = nil
	c.mu.Unlock()

	_ = c.conn.Close()
	close(c.done)
	for _, replies := range pending {
		close(replies)
	}
}

func (c *Client) readLoop() {
	for {
		messageType, data, err := c.conn.ReadMessage()
		if err != nil {
			c.shutdown(translateWSError(err))
			return
		}
		if messageType != websocket.TextMessage {
			c.shutdown(fmt.Errorf("%w: expected text WebSocket message, got type %d", ErrProtocol, messageType))
			return
		}
		var message wireMessage
		if err := json.Unmarshal(data, &message); err != nil {
			c.shutdown(fmt.Errorf("%w: malformed JSON: %v", ErrProtocol, err))
			return
		}
		if err := c.dispatch(&message); err != nil {
			c.shutdown(err)
			return
		}
	}
}

func translateWSError(err error) error {
	if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) ||
		errors.Is(err, net.ErrClosed) {
		return ErrClosed
	}
	return fmt.Errorf("codexappserver: WebSocket read: %w", err)
}

func (c *Client) dispatch(message *wireMessage) error {
	hasID := len(message.ID) != 0 && string(message.ID) != "null"
	switch {
	case message.Method != "" && hasID:
		request := ServerRequest{ID: cloneRaw(message.ID), Method: message.Method, Params: cloneRaw(message.Params)}
		select {
		case c.serverRequests <- request:
		default:
		}
		return &UnexpectedServerRequestError{Request: request}
	case message.Method != "" && !hasID:
		notification := Notification{Method: message.Method, Params: cloneRaw(message.Params)}
		select {
		case c.notifications <- notification:
			return nil
		default:
			return ErrNotificationOverrun
		}
	case message.Method == "" && hasID:
		key := string(message.ID)
		c.mu.Lock()
		replies, ok := c.pending[key]
		if ok {
			delete(c.pending, key)
		}
		c.mu.Unlock()
		if ok {
			replies <- message
			close(replies)
		}
		return nil // a late reply after caller cancellation is tolerated
	default:
		return fmt.Errorf("%w: message has neither method nor id", ErrProtocol)
	}
}

func cloneRaw(raw json.RawMessage) json.RawMessage { return append(json.RawMessage(nil), raw...) }

// Call sends one request and correlates its response. Context expiry after the
// write is an ambiguous outcome and is marked as such in *CallError.
func (c *Client) Call(ctx context.Context, method string, params, result any) error {
	if method == "" {
		return &CallError{Method: method, Cause: errors.New("empty method")}
	}
	encodedParams, err := json.Marshal(params)
	if err != nil {
		return &CallError{Method: method, Cause: fmt.Errorf("encode params: %w", err)}
	}

	c.mu.Lock()
	if c.closed {
		cause := c.closeErr
		c.mu.Unlock()
		return &CallError{Method: method, Cause: cause}
	}
	c.nextID++
	id := c.nextID
	key := strconv.FormatInt(id, 10)
	replies := make(chan *wireMessage, 1)
	c.pending[key] = replies
	c.mu.Unlock()

	idRaw := json.RawMessage(key)
	encoded, err := json.Marshal(wireMessage{ID: idRaw, Method: method, Params: encodedParams})
	if err != nil {
		c.abandon(key)
		return &CallError{Method: method, Cause: fmt.Errorf("encode request: %w", err)}
	}
	attempted, err := c.writeRaw(ctx, encoded)
	if err != nil {
		c.abandon(key)
		return &CallError{Method: method, Cause: err, Ambiguous: attempted}
	}

	select {
	case reply, ok := <-replies:
		if !ok {
			return &CallError{Method: method, Cause: c.Err(), Ambiguous: true}
		}
		return decodeReply(method, reply, result)
	case <-ctx.Done():
		c.abandon(key)
		return &CallError{Method: method, Cause: ctx.Err(), Ambiguous: true}
	case <-c.done:
		select {
		case reply, ok := <-replies:
			if ok {
				return decodeReply(method, reply, result)
			}
		default:
		}
		c.abandon(key)
		return &CallError{Method: method, Cause: c.Err(), Ambiguous: true}
	}
}

func decodeReply(method string, reply *wireMessage, result any) error {
	if reply.Error != nil {
		return reply.Error
	}
	if len(reply.Result) == 0 {
		return &CallError{Method: method, Cause: fmt.Errorf("%w: response has neither result nor error", ErrProtocol)}
	}
	if result == nil {
		return nil
	}
	if err := json.Unmarshal(reply.Result, result); err != nil {
		return &CallError{Method: method, Cause: fmt.Errorf("decode result: %w", err)}
	}
	return nil
}

func (c *Client) Notify(method string, params any) error {
	encodedParams, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("codexappserver: encode notification %s: %w", method, err)
	}
	encoded, err := json.Marshal(wireMessage{Method: method, Params: encodedParams})
	if err != nil {
		return fmt.Errorf("codexappserver: encode notification %s: %w", method, err)
	}
	if _, err := c.writeRaw(context.Background(), encoded); err != nil {
		return fmt.Errorf("codexappserver: send notification %s: %w", method, err)
	}
	return nil
}

func (c *Client) abandon(key string) {
	c.mu.Lock()
	if c.pending != nil {
		delete(c.pending, key)
	}
	c.mu.Unlock()
}

func (c *Client) writeRaw(ctx context.Context, data []byte) (bool, error) {
	select {
	case c.writeGate <- struct{}{}:
		defer func() { <-c.writeGate }()
	case <-ctx.Done():
		return false, ctx.Err()
	case <-c.done:
		return false, c.Err()
	}
	// Cancellation and the gate can become ready together. Recheck after
	// serialization so cancellation known before the write always wins.
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	default:
	}

	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return false, ErrClosed
	}
	deadline := time.Now().Add(c.writeTimeout)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	if err := c.conn.SetWriteDeadline(deadline); err != nil {
		return false, err
	}
	// Cancellation during a write makes its outcome ambiguous. Advance the
	// connection deadline to abort a wedged write, then wait for the callback
	// before clearing it so a late callback cannot poison the next writer.
	cancelApplied := make(chan struct{})
	stopCancel := context.AfterFunc(ctx, func() {
		_ = c.conn.SetWriteDeadline(time.Now())
		close(cancelApplied)
	})
	if err := c.conn.WriteMessage(websocket.TextMessage, data); err != nil {
		if !stopCancel() {
			<-cancelApplied
		}
		c.shutdown(fmt.Errorf("codexappserver: WebSocket write: %w", err))
		return true, err
	}
	if !stopCancel() {
		<-cancelApplied
	}
	_ = c.conn.SetWriteDeadline(time.Time{})
	return true, nil
}
