package client

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestClientRereadsRotatedCredentialAndDoesNotRetry(t *testing.T) {
	root, err := os.MkdirTemp("", "backend-client-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	socket := filepath.Join(root, "api.sock")
	credential := filepath.Join(root, "access")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var received []string
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		received = append(received, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v2/stop" {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"code":"uncertain"}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"execution-a"}`))
	})}
	defer server.Close()
	go func() { _ = server.Serve(listener) }()
	c, err := New(socket, credential)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for _, value := range []string{strings.Repeat("a", 64), strings.Repeat("b", 64)} {
		temporary := credential + ".next"
		if err := os.WriteFile(temporary, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(temporary, credential); err != nil {
			t.Fatal(err)
		}
		var result struct{ ID string }
		if err := c.Call(context.Background(), "GET", "/v2/identity", nil, &result); err != nil {
			t.Fatal(err)
		}
		if result.ID != "execution-a" {
			t.Fatal(result)
		}
	}
	err = c.Call(context.Background(), "POST", "/v2/stop", map[string]string{"request_id": "same-request"}, nil)
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != "uncertain" {
		t.Fatalf("uncertain outcome: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(received) != 3 || received[0] != "Bearer "+strings.Repeat("a", 64) || received[1] != "Bearer "+strings.Repeat("b", 64) {
		t.Fatalf("credential reread/no-retry failed: %d calls", len(received))
	}
}
