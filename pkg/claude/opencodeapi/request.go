// Package opencodeapi contains the authenticated control-plane boundary shared
// by the OpenCode harness and agentd's managed runtime.
package opencodeapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// ServerUsername is the fixed Basic-auth username used by tclaude-managed
// OpenCode servers. Each runtime has its own random password.
const ServerUsername = "opencode"

// NewRequest builds an authenticated request only after proving that the
// recorded managed process (or one of its children) still owns the endpoint.
// A health response alone is insufficient: a foreign local process could win
// the bind-close-exec race and capture the password from our next request.
func NewRequest(method, endpoint string, runtime db.OpenCodeRuntime, body any) (*http.Request, error) {
	if err := db.ValidateOpenCodeRuntimeTransport(runtime); err != nil {
		return nil, err
	}
	if runtime.Transport == db.OpenCodeTransportLoopbackTCP ||
		runtime.Transport == "" {
		if !ProcessOwnsEndpoint(runtime.PID, runtime.ServerURL) {
			return nil, fmt.Errorf("managed OpenCode process does not own %s", runtime.ServerURL)
		}
	}
	baseURL, baseErr := url.Parse(runtime.ServerURL)
	targetURL, targetErr := url.Parse(endpoint)
	if baseErr != nil || targetErr != nil || baseURL.Scheme != targetURL.Scheme ||
		baseURL.Host != targetURL.Host {
		return nil, fmt.Errorf("OpenCode request endpoint %q is outside managed server %s",
			endpoint, runtime.ServerURL)
	}
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, endpoint, reader)
	if err != nil {
		return nil, err
	}
	request.SetBasicAuth(ServerUsername, runtime.Password)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	return request, nil
}

// Do sends a request through its runtime's proven transport. For Unix relay
// rows the identity checks happen during DialContext, before HTTP writes Basic
// auth bytes to the connected peer.
func Do(client *http.Client, request *http.Request, runtime db.OpenCodeRuntime) (*http.Response, error) {
	if runtime.Transport == db.OpenCodeTransportUnixRelay {
		return doUnixRequest(client, request, runtime)
	}
	return client.Do(request)
}

func RuntimeOwnsEndpoint(runtime db.OpenCodeRuntime) bool {
	if runtime.Transport != db.OpenCodeTransportUnixRelay {
		return ProcessOwnsEndpoint(runtime.PID, runtime.ServerURL)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	conn, err := dialVerifiedUnix(ctx, runtime)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
