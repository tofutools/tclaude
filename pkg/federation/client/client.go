// Package client is the instance side of the hub protocol: it dials out to
// a tclaude-hub, answers the challenge, reconnects with backoff, tracks the
// directory, and sends/receives sealed envelopes. agentd and test peers use
// the same client.
package client

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/tofutools/tclaude/pkg/federation/proto"
)

// State is the connection state.
type State string

const (
	StateDisconnected State = "disconnected"
	StateConnecting   State = "connecting"
	StateConnected    State = "connected"
	// StateRefused means the hub rejected us (not admitted, bad auth); the
	// client keeps retrying slowly because an operator may admit us later.
	StateRefused State = "refused"
)

// Status is a snapshot of the connection.
type Status struct {
	IdentityRotationVersion int
	HubAdminVersion         int
	State                   State
	HubID                   string
	Spaces                  []string
	LastError               string
	Since                   time.Time
}

// Options configure a Client.
type Options struct {
	RotationChain []proto.Rotation
	URL           string
	Identity      *proto.Identity
	Name          string
	Version       string
	// Invite is presented on the first connection; once admitted it is
	// ignored by the hub.
	Invite string
	TLS    *tls.Config
	// OnDeliver receives every inbound envelope. It runs on the read
	// goroutine and should not block for long.
	OnDeliver func(from string, s *proto.Sealed)
	// OnDirectory receives every directory snapshot.
	OnDirectory func([]proto.DirectoryEntry)
	// OnState is called on every state change.
	OnState func(Status)
	Logger  *slog.Logger
	// MaxBackoff caps reconnect delay (default 60s).
	MaxBackoff time.Duration
}

// ValidateURL accepts wss:// anywhere and ws:// only to loopback.
func ValidateURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	switch u.Scheme {
	case "wss":
	case "ws":
		host := u.Hostname()
		ip := net.ParseIP(host)
		if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return nil, fmt.Errorf("plain ws:// is only allowed to loopback; use wss:// for %s", host)
		}
	default:
		return nil, fmt.Errorf("hub url must be ws:// (loopback) or wss://, got %q", u.Scheme)
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = proto.WSPath
	}
	return u, nil
}

// Client maintains one hub connection.
type Client struct {
	opts Options
	u    *url.URL
	log  *slog.Logger

	mu              sync.Mutex
	ws              *websocket.Conn
	wmu             sync.Mutex
	status          Status
	directory       []proto.DirectoryEntry
	pending         map[string]chan *proto.Frame
	refSeq          uint64
	adminNonce      string
	adminGeneration string
}

// New validates opts and returns an idle client; call Run.
func New(opts Options) (*Client, error) {
	if opts.Identity == nil {
		return nil, errors.New("client: identity required")
	}
	u, err := ValidateURL(opts.URL)
	if err != nil {
		return nil, err
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.MaxBackoff <= 0 {
		opts.MaxBackoff = 60 * time.Second
	}
	return &Client{
		opts: opts, u: u, log: opts.Logger.With("component", "federation-client"),
		status:  Status{State: StateDisconnected, Since: time.Now()},
		pending: map[string]chan *proto.Frame{},
	}, nil
}

// Status returns the current connection status.
func (c *Client) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.status
	st.Spaces = append([]string(nil), st.Spaces...)
	return st
}

// Directory returns the latest directory snapshot.
func (c *Client) Directory() []proto.DirectoryEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]proto.DirectoryEntry(nil), c.directory...)
}

func (c *Client) setState(st State, hubID string, spaces []string, errMsg string, rotationVersion ...int) {
	c.mu.Lock()
	c.status = Status{State: st, HubID: hubID, Spaces: spaces, LastError: errMsg, Since: time.Now()}
	if len(rotationVersion) > 0 {
		c.status.IdentityRotationVersion = rotationVersion[0]
	}
	if hubID == "" && st != StateConnected {
		c.status.HubID = ""
	}
	snap := c.status
	c.mu.Unlock()
	if c.opts.OnState != nil {
		c.opts.OnState(snap)
	}
}

// Run connects and reconnects until ctx is done.
func (c *Client) Run(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		c.setState(StateConnecting, "", nil, "")
		start := time.Now()
		err := c.runOnce(ctx)
		if ctx.Err() != nil {
			break
		}
		var rf *RefusedError
		st := StateDisconnected
		if errors.As(err, &rf) {
			st = StateRefused
		}
		msg := ""
		if err != nil {
			msg = err.Error()
		}
		c.setState(st, "", nil, msg)
		c.log.Info("hub connection ended", "error", err)
		if time.Since(start) > 2*time.Minute {
			backoff = time.Second
		}
		wait := backoff
		if st == StateRefused && wait < 30*time.Second {
			wait = 30 * time.Second
		}
		select {
		case <-ctx.Done():
		case <-time.After(wait):
		}
		backoff *= 2
		if backoff > c.opts.MaxBackoff {
			backoff = c.opts.MaxBackoff
		}
	}
	c.setState(StateDisconnected, "", nil, "")
}

// RefusedError is a hub refusal during handshake.
type RefusedError struct{ Code, Message string }

func (e *RefusedError) Error() string { return "hub refused: " + e.Code + ": " + e.Message }

func (c *Client) runOnce(ctx context.Context) error {
	d := websocket.Dialer{TLSClientConfig: c.opts.TLS, HandshakeTimeout: 15 * time.Second, Proxy: http.ProxyFromEnvironment}
	ws, _, err := d.DialContext(ctx, c.u.String(), nil)
	if err != nil {
		return err
	}
	defer func() { _ = ws.Close() }()
	ws.SetReadLimit(proto.MaxEnvelopeBytes*2 + 64<<10)

	_ = ws.SetReadDeadline(time.Now().Add(15 * time.Second))
	var ch proto.Frame
	if err := ws.ReadJSON(&ch); err != nil {
		return err
	}
	if ch.Type != proto.FrameChallenge {
		return fmt.Errorf("expected challenge, got %q", ch.Type)
	}
	id := c.opts.Identity
	hello := &proto.Frame{
		Type: proto.FrameHello, Proto: proto.ProtocolVersion,
		InstanceID: id.ID(), PubKey: id.Pub, Name: c.opts.Name, Version: c.opts.Version,
		Sig: proto.SignHello(id, ch.HubID, ch.Nonce), Invite: c.opts.Invite, RotationChain: c.opts.RotationChain,
	}
	_ = ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if err := ws.WriteJSON(hello); err != nil {
		return err
	}
	var welcome proto.Frame
	if err := ws.ReadJSON(&welcome); err != nil {
		return err
	}
	if welcome.Type == proto.FrameError {
		return &RefusedError{Code: welcome.Code, Message: welcome.Message}
	}
	if welcome.Type != proto.FrameWelcome {
		return fmt.Errorf("expected welcome, got %q", welcome.Type)
	}
	_ = ws.SetReadDeadline(time.Now().Add(90 * time.Second))
	ws.SetPingHandler(func(data string) error {
		_ = ws.SetReadDeadline(time.Now().Add(90 * time.Second))
		c.wmu.Lock()
		defer c.wmu.Unlock()
		return ws.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(5*time.Second))
	})

	c.mu.Lock()
	c.ws = ws
	c.adminNonce = ch.Nonce
	c.adminGeneration = welcome.AdminGeneration
	c.mu.Unlock()
	c.setState(StateConnected, welcome.HubID, welcome.Spaces, "", welcome.IdentityRotationVersion)
	c.mu.Lock()
	c.status.HubAdminVersion = welcome.HubAdminVersion
	c.mu.Unlock()
	c.log.Info("connected to hub", "hub", welcome.HubID, "spaces", welcome.Spaces)

	stop := context.AfterFunc(ctx, func() { _ = ws.Close() })
	defer stop()
	err = c.readLoop(ws)

	c.mu.Lock()
	c.ws = nil
	for ref, chn := range c.pending {
		close(chn)
		delete(c.pending, ref)
	}
	c.directory = nil
	c.mu.Unlock()
	if c.opts.OnDirectory != nil {
		c.opts.OnDirectory(nil)
	}
	return err
}

func (c *Client) readLoop(ws *websocket.Conn) error {
	for {
		var f proto.Frame
		if err := ws.ReadJSON(&f); err != nil {
			return err
		}
		_ = ws.SetReadDeadline(time.Now().Add(90 * time.Second))
		switch f.Type {
		case proto.FrameDirectory:
			c.mu.Lock()
			c.directory = f.Instances
			c.mu.Unlock()
			if c.opts.OnDirectory != nil {
				c.opts.OnDirectory(f.Instances)
			}
		case proto.FrameDeliver:
			if f.Sealed != nil && c.opts.OnDeliver != nil {
				c.opts.OnDeliver(f.From, f.Sealed)
			}
		case proto.FrameAdminResult:
			if f.AdminResult != nil && proto.ValidStreamID(f.AdminResult.ID) {
				c.mu.Lock()
				chn := c.pending[f.AdminResult.ID]
				delete(c.pending, f.AdminResult.ID)
				if c.ws == ws {
					c.adminGeneration = f.AdminResult.Generation
				}
				c.mu.Unlock()
				if chn != nil {
					fc := f
					chn <- &fc
				}
			}
		case proto.FrameSendResult:
			c.mu.Lock()
			chn := c.pending[f.Ref]
			delete(c.pending, f.Ref)
			c.mu.Unlock()
			if chn != nil {
				fc := f
				chn <- &fc
			}
		case proto.FrameError:
			if f.Code != "" {
				return &RefusedError{Code: f.Code, Message: f.Message}
			}
		}
	}
}

// ErrNotConnected is returned by Send while no hub connection is live.
var ErrNotConnected = errors.New("not connected to hub")

// SendResult is the hub's answer to one send.
type SendResult struct {
	Status  string
	Code    string
	Message string
}

// Send hands s to the hub for instance `to` and waits for the hub's
// routing result (not an end-to-end ack).
func (c *Client) Send(ctx context.Context, to string, s *proto.Sealed) (*SendResult, error) {
	c.mu.Lock()
	ws := c.ws
	if ws == nil {
		c.mu.Unlock()
		return nil, ErrNotConnected
	}
	c.refSeq++
	ref := fmt.Sprintf("r%d", c.refSeq)
	chn := make(chan *proto.Frame, 1)
	c.pending[ref] = chn
	c.mu.Unlock()

	c.wmu.Lock()
	_ = ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
	err := ws.WriteJSON(&proto.Frame{Type: proto.FrameSend, Ref: ref, To: to, Sealed: s})
	c.wmu.Unlock()
	if err != nil {
		c.mu.Lock()
		delete(c.pending, ref)
		c.mu.Unlock()
		return nil, err
	}
	select {
	case f, ok := <-chn:
		if !ok {
			return nil, ErrNotConnected
		}
		return &SendResult{Status: f.Status, Code: f.Code, Message: f.Message}, nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, ref)
		c.mu.Unlock()
		return nil, ctx.Err()
	}
}

// LookupKey returns the directory's public key for instanceID.
func (c *Client) LookupKey(instanceID string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.directory {
		if e.InstanceID == instanceID {
			return e.PubKey, true
		}
	}
	return nil, false
}

// IsLoopbackURL reports whether raw targets a loopback host.
func IsLoopbackURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	h := u.Hostname()
	ip := net.ParseIP(h)
	return strings.EqualFold(h, "localhost") || (ip != nil && ip.IsLoopback())
}
