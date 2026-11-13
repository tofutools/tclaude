package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// DaemonStreamGet opens an authenticated streaming response. The caller owns
// closing the body and supplies its deadline through ctx. It does not replay a
// stream automatically; consumers resume with their own output cursors.
func DaemonStreamGet(ctx context.Context, path string) (io.ReadCloser, error) {
	return DaemonStreamRequest(ctx, http.MethodGet, path, nil)
}

// DaemonStreamRequest is DaemonStreamGet for any method, with an optional
// JSON body.
func DaemonStreamRequest(ctx context.Context, method, path string, in any) (io.ReadCloser, error) {
	var body io.Reader
	if in != nil {
		buf, e := json.Marshal(in)
		if e != nil {
			return nil, e
		}
		body = bytes.NewReader(buf)
	}
	req, e := http.NewRequestWithContext(ctx, method, "http://_"+path, body)
	if e != nil {
		return nil, e
	}
	attachCallerIdentity(req)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
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
