package server

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers"
)

func TestServerSettlesOfflineMessageNotificationAndKeepsInbox(t *testing.T) {
	root, err := os.MkdirTemp("", "notice-")
	require.NoError(t, err)
	defer os.RemoveAll(root)
	dir := filepath.Join(root, "state")
	require.NoError(t, Initialize(dir))
	token, err := os.ReadFile(filepath.Join(dir, "operator.token"))
	require.NoError(t, err)
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(dir, "api.sock"))
	}}}
	defer client.CloseIdleConnections()
	call := func(method, path, body string) (int, []byte, error) {
		req, err := http.NewRequest(method, "http://backend"+path, strings.NewReader(body))
		if err != nil {
			return 0, nil, err
		}
		req.Header.Set("Authorization", "Bearer "+string(token))
		res, err := client.Do(req)
		if err != nil {
			return 0, nil, err
		}
		defer res.Body.Close()
		data, err := io.ReadAll(res.Body)
		return res.StatusCode, data, err
	}
	for round := 0; round < 2; round++ {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- Serve(ctx, dir, providers.NewRegistry()) }()
		require.Eventually(t, func() bool { status, _, err := call("GET", "/v2/snapshot", ""); return err == nil && status == 200 }, 10*time.Second, 10*time.Millisecond)
		if round == 0 {
			status, data, err := call("POST", "/v2/agents", `{"id":"offline","name":"Offline recipient","desired":{"Harness":"claude","WorkingDirectory":"/tmp","Approval":"supervised","Sandbox":"unconfined"}}`)
			require.NoError(t, err)
			require.Equal(t, 201, status, string(data))
			status, data, err = call("POST", "/v2/messages", `{"request_id":"send","recipients":["offline"],"body":"Kept while offline"}`)
			require.NoError(t, err)
			require.Less(t, status, 300, string(data))
		}
		require.Eventually(t, func() bool {
			status, data, err := call("GET", "/v2/snapshot", "")
			if err != nil || status != 200 {
				return false
			}
			var snapshot struct {
				Messages []model.Message `json:"messages"`
			}
			if json.Unmarshal(data, &snapshot) != nil || len(snapshot.Messages) != 1 {
				return false
			}
			message := snapshot.Messages[0]
			return message.Body == "Kept while offline" && len(message.Recipients) == 1 && message.Recipients[0].NotificationOutcome == model.NotificationUnavailable && message.Recipients[0].ReadAt == nil
		}, 10*time.Second, 20*time.Millisecond)
		cancel()
		require.NoError(t, <-done)
		client.CloseIdleConnections()
	}
}
