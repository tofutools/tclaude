package opencode

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/tofutools/tclaude/internal/backend/host"
)

func (r *Runtime) dialSandboxControl(ctx context.Context) (net.Conn, error) {
	r.controlMu.Lock()
	if r.controlIdentity == nil {
		if r.artifact == nil {
			r.controlMu.Unlock()
			return nil, fmt.Errorf("OpenCode sandbox control artifact is unavailable")
		}
		identity, err := host.ReadSandboxControl(*r.artifact)
		if err != nil {
			r.controlMu.Unlock()
			return nil, err
		}
		r.controlIdentity = &identity
	}
	identity := *r.controlIdentity
	r.controlMu.Unlock()
	// Every newly opened connection proves the retained process, socket inode,
	// and kernel peer before the HTTP client can send its password or request.
	return r.process.DialUnixControl(ctx, identity)
}

func (r *Runtime) sandboxHTTPClient() *http.Client {
	r.controlMu.Lock()
	defer r.controlMu.Unlock()
	if r.controlTransport == nil {
		r.controlTransport = &http.Transport{DisableKeepAlives: true, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return r.dialSandboxControl(ctx)
		}}
	}
	timeout := 2 * time.Second
	if r.provider.httpClient != nil {
		timeout = r.provider.httpClient.Timeout
	}
	return &http.Client{Transport: r.controlTransport, Timeout: timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func (r *Runtime) sandboxControlEvidence() *host.UnixControlIdentity {
	r.controlMu.Lock()
	defer r.controlMu.Unlock()
	if r.controlIdentity == nil {
		return nil
	}
	identity := *r.controlIdentity
	return &identity
}
