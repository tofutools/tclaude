package mcpserve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
)

const (
	requestTimeout  = 30 * time.Second
	maxResponseSize = 16 << 20
)

// Client talks to the daemon as the principal. It is not the agent package's
// client on purpose: that one attaches TCLAUDE_HUMAN_TOKEN and a session claim
// from the environment, and an assistant's process may have either. This one
// sends the credential and nothing else that identifies a caller.
type Client struct {
	http       *http.Client
	credential string
}

// NewClient returns a client over the daemon's Unix socket.
func NewClient(credential string) *Client {
	socket := agentipc.ClientSocketPath()
	return NewClientWithHTTP(&http.Client{
		Timeout: requestTimeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var dialer net.Dialer
				return dialer.DialContext(ctx, "unix", socket)
			},
		},
	}, credential)
}

// NewClientWithHTTP uses a caller's transport, for tests.
func NewClientWithHTTP(httpClient *http.Client, credential string) *Client {
	return &Client{http: httpClient, credential: credential}
}

// DaemonError is a refusal or failure the daemon reported.
type DaemonError struct {
	Status int
	Code   string
	Msg    string
}

func (e *DaemonError) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("tclaude daemon answered %d: %s", e.Status, e.Msg)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Msg)
}

// call makes one request and returns the response body. A request that was sent
// is never retried: it may be a message, and the caller can decide.
func (c *Client) call(ctx context.Context, method, path string, in any) ([]byte, error) {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://_"+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set(agentipc.ExternalStewardCredentialHeader, c.credential)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("the tclaude daemon is not reachable; ask the operator to start it")
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize+1))
	if err != nil {
		return nil, fmt.Errorf("read daemon response: %w", err)
	}
	if len(raw) > maxResponseSize {
		return nil, errors.New("the daemon's response is too large")
	}
	if resp.StatusCode >= http.StatusBadRequest {
		var refusal struct {
			Code  string `json:"code"`
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &refusal)
		return nil, &DaemonError{Status: resp.StatusCode, Code: refusal.Code, Msg: refusal.Error}
	}
	return raw, nil
}
