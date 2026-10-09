package agent

import (
	"context"
	"fmt"
	"io"
	"net/http"
)

// DaemonStreamGet opens an authenticated streaming response. The caller owns
// closing the body and supplies its deadline through ctx. It does not replay a
// stream automatically; consumers resume with their own output cursors.
func DaemonStreamGet(ctx context.Context, path string) (io.ReadCloser, error) {
	req, e := http.NewRequestWithContext(ctx, http.MethodGet, "http://_"+path, nil)
	if e != nil {
		return nil, e
	}
	attachCallerIdentity(req)
	resp, e := httpClientWithTimeout(0).Do(req)
	if e != nil {
		return nil, e
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		raw, e := daemonResponseBytes(resp)
		if e != nil {
			return nil, fmt.Errorf("read stream error: %w", e)
		}
		return nil, decodeDaemonError(resp.StatusCode, raw)
	}
	return resp.Body, nil
}
