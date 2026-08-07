package copilotapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// SupportedProtocolVersion is the `connect` protocol version this package was
// written against, matching SDK_PROTOCOL_VERSION in the shipped
// copilot-sdk. Copilot CLI 1.0.78 reports 3.
const SupportedProtocolVersion = 3

// DefaultSubscriptionBuffer is the per-subscription event queue depth. A
// single busy turn emits on the order of tens of events, so this leaves a
// consumer room to fall behind briefly without losing the stream.
const DefaultSubscriptionBuffer = 256

// defaultWriteTimeout bounds a single request write, so a wedged peer that
// stops reading cannot block a caller indefinitely.
const defaultWriteTimeout = 30 * time.Second

// Options configures a [Client].
type Options struct {
	// Token is sent with the handshake. TUI+server mode ignores it; only the
	// headless `--server` path honours COPILOT_CONNECTION_TOKEN.
	Token string
	// SubscriptionBuffer overrides [DefaultSubscriptionBuffer].
	SubscriptionBuffer int
	// AllowProtocolMismatch keeps the connection when the server's protocol
	// version differs from [SupportedProtocolVersion]. The mismatch is still
	// recorded in [Client.ProtocolMismatch]. Off by default: a client that
	// silently talks a contract it was not built for produces wrong answers
	// rather than obvious failures.
	AllowProtocolMismatch bool
	// Dialer overrides how the TCP connection is established. Tests use it;
	// production should leave it nil.
	Dialer func(ctx context.Context, address string) (net.Conn, error)
}

func (o *Options) subscriptionBuffer() int {
	if o != nil && o.SubscriptionBuffer > 0 {
		return o.SubscriptionBuffer
	}
	return DefaultSubscriptionBuffer
}

// Client is a connection to one Copilot embedded server.
//
// It is safe for concurrent use. A Client wraps exactly one TCP connection and
// does not reconnect on its own: when the server goes away every in-flight and
// subsequent call fails with the cause, [Client.Done] closes, and the caller
// decides whether to redial. [DialRetry] implements the usual retry policy.
//
// Copilot broadcasts events to every connected client and lets any connection
// drive a session another one created, so holding several Clients against one
// server is legitimate — an event consumer need not be the connection that
// sends prompts.
type Client struct {
	conn    net.Conn
	address string

	writeMu sync.Mutex

	mu              sync.Mutex
	nextID          int64
	pending         map[int64]chan *rpcMessage
	nextSub         int64
	subs            map[int64]*Subscription
	closed          bool
	closeErr        error
	malformedFrames int

	done chan struct{}

	bufferSize int

	// Server identity, set once by the handshake before the Client is
	// returned to the caller, and read-only thereafter.
	protocolVersion  int
	serverVersion    string
	protocolMismatch bool
}

// rpcMessage is any framed JSON-RPC message. Requests and notifications carry
// Method; responses never do, which is how the reader tells them apart.
type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// Dial connects to a Copilot embedded server at address (for example
// "127.0.0.1:4599") and performs the `connect` handshake.
func Dial(ctx context.Context, address string, opts *Options) (*Client, error) {
	dial := func(ctx context.Context, address string) (net.Conn, error) {
		var dialer net.Dialer
		return dialer.DialContext(ctx, "tcp", address)
	}
	if opts != nil && opts.Dialer != nil {
		dial = opts.Dialer
	}
	conn, err := dial(ctx, address)
	if err != nil {
		return nil, fmt.Errorf("copilotapi: dial %s: %w", address, err)
	}
	client := &Client{
		conn:       conn,
		address:    address,
		pending:    make(map[int64]chan *rpcMessage),
		subs:       make(map[int64]*Subscription),
		done:       make(chan struct{}),
		bufferSize: opts.subscriptionBuffer(),
	}
	go client.readLoop()

	var token string
	var allowMismatch bool
	if opts != nil {
		token = opts.Token
		allowMismatch = opts.AllowProtocolMismatch
	}
	if err := client.handshake(ctx, token, allowMismatch); err != nil {
		_ = client.Close()
		return nil, err
	}
	return client, nil
}

// DialRetry calls [Dial] until it succeeds or ctx ends, backing off between
// attempts. The embedded server binds its port a few seconds after the process
// starts, and a pane's Copilot process can be restarted underneath us, so both
// "not up yet" and "came back" are ordinary.
//
// It returns the last dial error when ctx ends, so a caller that gave up still
// learns why.
func DialRetry(ctx context.Context, address string, opts *Options) (*Client, error) {
	const (
		initialBackoff = 100 * time.Millisecond
		maxBackoff     = 2 * time.Second
	)
	backoff := initialBackoff
	var lastErr error
	for {
		client, err := Dial(ctx, address, opts)
		if err == nil {
			return client, nil
		}
		lastErr = err
		// A protocol-version mismatch is a property of the server build, not
		// a transient condition; retrying only delays the same failure.
		if errors.Is(err, ErrProtocolVersion) {
			return nil, err
		}
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, fmt.Errorf("copilotapi: dial %s: %w (last attempt: %w)", address, ctx.Err(), lastErr)
		case <-timer.C:
		}
		if backoff *= 2; backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

func (c *Client) handshake(ctx context.Context, token string, allowMismatch bool) error {
	var result ConnectResult
	if err := c.Call(ctx, MethodConnect, ConnectParams{Token: token}, &result); err != nil {
		return fmt.Errorf("copilotapi: handshake: %w", err)
	}
	if !result.OK {
		return errors.New("copilotapi: handshake: server did not report ok")
	}
	c.protocolVersion = result.ProtocolVersion
	c.serverVersion = result.Version
	if result.ProtocolVersion != SupportedProtocolVersion {
		c.protocolMismatch = true
		if !allowMismatch {
			return fmt.Errorf("%w: server reports %d (CLI %s), this client supports %d",
				ErrProtocolVersion, result.ProtocolVersion, result.Version, SupportedProtocolVersion)
		}
	}
	return nil
}

// ProtocolVersion is the protocol version reported by the handshake.
func (c *Client) ProtocolVersion() int { return c.protocolVersion }

// ServerVersion is the Copilot CLI package version reported by the handshake,
// for example "1.0.78".
func (c *Client) ServerVersion() string { return c.serverVersion }

// ProtocolMismatch reports whether the server's protocol version differed from
// [SupportedProtocolVersion]. It can only be true when
// [Options.AllowProtocolMismatch] was set, since otherwise the handshake fails.
func (c *Client) ProtocolMismatch() bool { return c.protocolMismatch }

// Address is the server address this Client was dialled with.
func (c *Client) Address() string { return c.address }

// Done closes when the connection is finished, whether from [Client.Close] or
// because the server went away. [Client.Err] reports which.
func (c *Client) Done() <-chan struct{} { return c.done }

// Err reports why the connection ended, or nil while it is still live. A
// server that hung up cleanly reports [ErrClosed], as does [Client.Close].
func (c *Client) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closeErr
}

// Close shuts the connection down and fails every in-flight call. It is
// idempotent.
func (c *Client) Close() error {
	c.shutdown(ErrClosed)
	return nil
}

// shutdown records the terminating cause once and wakes everything waiting.
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
	subs := c.subs
	c.subs = nil
	c.mu.Unlock()

	_ = c.conn.Close()
	close(c.done)

	// Waiters select on both their reply channel and c.done, so closing the
	// reply channels only speeds up the common case.
	for _, ch := range pending {
		close(ch)
	}
	for _, sub := range subs {
		sub.finish(cause)
	}
}

// readLoop owns all reads from the connection for the Client's lifetime.
func (c *Client) readLoop() {
	reader := newFrameReader(c.conn)
	for {
		frame, err := readFrame(reader)
		if err != nil {
			c.shutdown(translateConnError(err))
			return
		}
		var message rpcMessage
		if err := json.Unmarshal(frame, &message); err != nil {
			// Framing is length-delimited, so a body we cannot parse costs us
			// that one message and nothing else: the next frame boundary is
			// already known. Tearing the connection down here would turn one
			// unrecognised message into the loss of every in-flight call and
			// every subscription. Only a *framing* error is unrecoverable,
			// and readFrame reports that separately above.
			c.mu.Lock()
			c.malformedFrames++
			c.mu.Unlock()
			continue
		}
		c.dispatch(&message)
	}
}

// MalformedFrames counts messages dropped because their body did not decode.
// Framing is length-delimited so these are survivable, but a non-zero count
// means we are silently ignoring something the server sent, which is worth
// surfacing rather than leaving invisible.
func (c *Client) MalformedFrames() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.malformedFrames
}

// translateConnError maps a peer hangup onto ErrClosed so callers can treat
// "server gone" uniformly whether it was noticed on a read or a write, and
// leaves genuine faults intact.
func translateConnError(err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) {
		return ErrClosed
	}
	return err
}

func (c *Client) dispatch(message *rpcMessage) {
	switch {
	case message.Method != "" && message.ID != nil:
		// A server-to-client request. We never opt into the handlers that
		// produce these (requestUserInput and friends on session.create), but
		// the server blocks its own turn waiting for a reply, so an
		// unanswered one would wedge the session rather than just be ignored.
		c.replyMethodNotFound(*message.ID, message.Method)
	case message.Method != "":
		c.fanOut(Notification{Method: message.Method, Params: message.Params})
	case message.ID != nil:
		c.deliver(*message.ID, message)
	default:
		// A message with neither method nor id is unaddressable; there is no
		// caller to fail and no subscriber it belongs to.
	}
}

func (c *Client) deliver(id int64, message *rpcMessage) {
	c.mu.Lock()
	ch, ok := c.pending[id]
	if ok {
		delete(c.pending, id)
	}
	c.mu.Unlock()
	if !ok {
		// A reply to a call that already gave up on it.
		return
	}
	ch <- message
	close(ch)
}

func (c *Client) replyMethodNotFound(id int64, method string) {
	reply := rpcMessage{
		JSONRPC: "2.0",
		ID:      &id,
		Error: &Error{
			Code:    CodeMethodNotFound,
			Message: fmt.Sprintf("tclaude copilotapi client does not handle %s", method),
		},
	}
	encoded, err := json.Marshal(reply)
	if err != nil {
		return
	}
	// Best effort: a failure here means the connection is going away anyway,
	// and the read loop will see it on its next frame.
	_ = c.writeRaw(encoded)
}

func (c *Client) fanOut(notification Notification) {
	c.mu.Lock()
	subs := make([]*Subscription, 0, len(c.subs))
	for _, sub := range c.subs {
		subs = append(subs, sub)
	}
	c.mu.Unlock()
	for _, sub := range subs {
		if !sub.offer(notification) {
			// The subscriber fell behind. Drop it rather than block the read
			// loop, which would stall every other subscriber and every
			// in-flight call on this connection.
			c.removeSubscription(sub.id)
			sub.finish(ErrSubscriptionOverrun)
		}
	}
}

// Call sends a request and waits for its reply.
//
// params is marshalled as the request params and may be nil. result, when
// non-nil, receives the unmarshalled reply. A JSON-RPC error reply is returned
// as an [*Error].
func (c *Client) Call(ctx context.Context, method string, params any, result any) error {
	var encodedParams json.RawMessage
	if params != nil {
		encoded, err := json.Marshal(params)
		if err != nil {
			return fmt.Errorf("copilotapi: encode params for %s: %w", method, err)
		}
		encodedParams = encoded
	}

	c.mu.Lock()
	if c.closed {
		err := c.closeErr
		c.mu.Unlock()
		return fmt.Errorf("copilotapi: call %s: %w", method, err)
	}
	c.nextID++
	id := c.nextID
	replies := make(chan *rpcMessage, 1)
	c.pending[id] = replies
	c.mu.Unlock()

	encoded, err := json.Marshal(rpcMessage{
		JSONRPC: "2.0",
		ID:      &id,
		Method:  method,
		Params:  encodedParams,
	})
	if err != nil {
		c.abandon(id)
		return fmt.Errorf("copilotapi: encode request %s: %w", method, err)
	}
	if err := c.writeRaw(encoded); err != nil {
		c.abandon(id)
		return fmt.Errorf("copilotapi: send %s: %w", method, err)
	}

	select {
	case <-ctx.Done():
		c.abandon(id)
		return fmt.Errorf("copilotapi: call %s: %w", method, ctx.Err())
	case <-c.done:
		// A reply can land in the same instant the connection dies, and select
		// picks uniformly among ready cases. Check for one before reporting
		// the connection error, so a caller never retries a `session.send`
		// whose prompt the server already accepted.
		select {
		case reply, ok := <-replies:
			if ok {
				return decodeReply(method, reply, result)
			}
		default:
		}
		c.abandon(id)
		return fmt.Errorf("copilotapi: call %s: %w", method, c.Err())
	case reply, ok := <-replies:
		if !ok {
			return fmt.Errorf("copilotapi: call %s: %w", method, c.Err())
		}
		return decodeReply(method, reply, result)
	}
}

// decodeReply turns a server response into an error or a decoded result.
func decodeReply(method string, reply *rpcMessage, result any) error {
	if reply.Error != nil {
		return reply.Error
	}
	if result == nil || len(reply.Result) == 0 {
		return nil
	}
	if err := json.Unmarshal(reply.Result, result); err != nil {
		return fmt.Errorf("copilotapi: decode %s result: %w", method, err)
	}
	return nil
}

// Notify sends a notification, which has no reply.
func (c *Client) Notify(method string, params any) error {
	var encodedParams json.RawMessage
	if params != nil {
		encoded, err := json.Marshal(params)
		if err != nil {
			return fmt.Errorf("copilotapi: encode params for %s: %w", method, err)
		}
		encodedParams = encoded
	}
	encoded, err := json.Marshal(rpcMessage{JSONRPC: "2.0", Method: method, Params: encodedParams})
	if err != nil {
		return fmt.Errorf("copilotapi: encode notification %s: %w", method, err)
	}
	if err := c.writeRaw(encoded); err != nil {
		return fmt.Errorf("copilotapi: send %s: %w", method, err)
	}
	return nil
}

// abandon forgets a pending call whose caller has stopped waiting, so a late
// reply is discarded instead of delivered to nobody.
func (c *Client) abandon(id int64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

func (c *Client) writeRaw(body []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return ErrClosed
	}

	if err := c.conn.SetWriteDeadline(time.Now().Add(defaultWriteTimeout)); err != nil {
		return err
	}
	if err := writeFrame(c.conn, body); err != nil {
		// A partial frame leaves the peer's parser mid-message, so the
		// connection cannot be reused. Translate the same way as the read
		// path, so a caller gating redial on ErrClosed sees "server gone"
		// whichever side noticed it first.
		translated := translateConnError(err)
		c.shutdown(translated)
		return translated
	}
	return c.conn.SetWriteDeadline(time.Time{})
}

// Subscription is a stream of server notifications. Every subscription
// receives every notification.
type Subscription struct {
	id     int64
	client *Client
	events chan Notification

	mu       sync.Mutex
	finished bool
	err      error
}

// C is the notification channel. It closes when the subscription ends, either
// from [Subscription.Close], because the connection ended, or because the
// consumer fell behind; [Subscription.Err] distinguishes them.
func (s *Subscription) C() <-chan Notification { return s.events }

// Err reports why the subscription ended, or nil while it is still live.
func (s *Subscription) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *Subscription) offer(notification Notification) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished {
		// Already ended; report success so the caller does not try to end it
		// a second time.
		return true
	}
	select {
	case s.events <- notification:
		return true
	default:
		return false
	}
}

// finish ends the subscription once, under the same lock that guards offer, so
// the channel is never closed with a send in flight.
func (s *Subscription) finish(cause error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished {
		return
	}
	s.finished = true
	s.err = cause
	close(s.events)
}

// Subscribe returns a stream of every notification the server pushes,
// including any method this package does not model.
//
// A subscriber that stops draining its channel is dropped once the buffer
// fills, with [ErrSubscriptionOverrun] on [Subscription.Err]. Losing events
// silently would let a consumer report stale agent state indefinitely, so the
// loss is made visible and the consumer is expected to re-subscribe and
// re-read authoritative state.
//
// Callers must [Subscription.Close] subscriptions they no longer need.
func (c *Client) Subscribe() *Subscription {
	sub := &Subscription{client: c, events: make(chan Notification, c.bufferSize)}
	c.mu.Lock()
	if c.closed {
		cause := c.closeErr
		c.mu.Unlock()
		sub.finish(cause)
		return sub
	}
	c.nextSub++
	sub.id = c.nextSub
	c.subs[sub.id] = sub
	c.mu.Unlock()
	return sub
}

func (c *Client) removeSubscription(id int64) {
	c.mu.Lock()
	delete(c.subs, id)
	c.mu.Unlock()
}

// Close ends the subscription and detaches it from its client. It is
// idempotent, and safe to call after the client has closed.
func (s *Subscription) Close() {
	if s.client != nil {
		s.client.removeSubscription(s.id)
	}
	s.finish(ErrClosed)
}
