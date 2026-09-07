package server

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tofutools/tclaude/internal/backend/providers"
)

func TestIsolatedServerPersistsOfflineCatalogAcrossRestart(t *testing.T) {
	// Short root keeps Unix socket paths below platform limits.
	parent, err := os.MkdirTemp("", "backend-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(parent)
	dir := filepath.Join(parent, "state")
	if err := Initialize(dir); err != nil {
		t.Fatal(err)
	}
	if err := Initialize(dir); err == nil {
		t.Fatal("existing directory must not be initialized again")
	}
	token, err := os.ReadFile(filepath.Join(dir, "operator.token"))
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(dir, "api.sock"))
	}}}
	defer client.CloseIdleConnections()
	call := func(method, path, body string) (int, string, error) {
		req, err := http.NewRequest(method, "http://backend"+path, strings.NewReader(body))
		if err != nil {
			return 0, "", err
		}
		req.Header.Set("Authorization", "Bearer "+string(token))
		res, err := client.Do(req)
		if err != nil {
			return 0, "", err
		}
		defer res.Body.Close()
		data, err := io.ReadAll(res.Body)
		return res.StatusCode, string(data), err
	}
	for round := 0; round < 2; round++ {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- Serve(ctx, dir, providers.NewRegistry()) }()
		deadline := time.Now().Add(20 * time.Second)
		for {
			select {
			case serveErr := <-done:
				cancel()
				t.Fatalf("server exited before readiness: %v", serveErr)
			default:
			}
			status, _, err := call("GET", "/v2/snapshot", "")
			if err == nil && status == 200 {
				break
			}
			if time.Now().After(deadline) {
				cancel()
				serveErr := <-done
				t.Fatalf("server did not become available: status=%d probe=%v serve=%v", status, err, serveErr)
			}
			time.Sleep(10 * time.Millisecond)
		}
		if round == 0 {
			status, body, err := call("POST", "/v2/agents", `{"id":"worker","name":"offline","desired":{"Harness":"claude","WorkingDirectory":"/tmp","Approval":"supervised","Sandbox":"workspace_write"}}`)
			if err != nil || status != 201 {
				cancel()
				t.Fatalf("create %d %s %v", status, body, err)
			}
		} else {
			status, body, err := call("GET", "/v2/snapshot", "")
			if err != nil || status != 200 || !strings.Contains(body, "offline") {
				cancel()
				t.Fatalf("reopen %d %s %v", status, body, err)
			}
		}
		cancel()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		client.CloseIdleConnections()
	}
}
