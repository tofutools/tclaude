package agentipc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// BrokerStatusError is a brokered endpoint's answer other than 200. Its
// message carries the status and a short preview of the body; callers that
// treat some statuses specially (a refusal, a throttle) read Status.
type BrokerStatusError struct {
	Status  int
	Preview []byte
}

func (e *BrokerStatusError) Error() string {
	return fmt.Sprintf("agentd returned %d: %s", e.Status, bytes.TrimSpace(e.Preview))
}

// PostToBroker POSTs a JSON body to one of agentd's brokered endpoints, the
// ones a sandboxed agent's hook and status-line callbacks use, over the
// first of ClientSocketPaths that accepts a connection, and decodes a 200
// answer of at most 1 MiB into out. Any other status is a *BrokerStatusError.
func PostToBroker(path string, body []byte, timeout time.Duration, out any) error {
	return postToBrokerAt(ClientSocketPaths(), path, body, timeout, out)
}

func postToBrokerAt(socks []string, path string, body []byte, timeout time.Duration, out any) error {
	if len(socks) == 0 {
		return fmt.Errorf("no agentd socket path resolved")
	}
	client := &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var lastErr error
				for _, sock := range socks {
					conn, err := (&net.Dialer{}).DialContext(ctx, "unix", sock)
					if err == nil {
						return conn, nil
					}
					lastErr = err
				}
				return nil, lastErr
			},
		},
	}
	req, err := http.NewRequest(http.MethodPost, "http://tclaude"+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		preview, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return &BrokerStatusError{Status: resp.StatusCode, Preview: preview}
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out); err != nil {
		return fmt.Errorf("decode broker response: %w", err)
	}
	return nil
}
