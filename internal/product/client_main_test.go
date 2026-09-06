package product

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAgentCLIUsesDeliveredResourceAndOwnInboxRequest(t *testing.T) {
	root, err := os.MkdirTemp("", "agent-cli-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	socket := filepath.Join(root, "api.sock")
	credential := filepath.Join(root, "action")
	token := strings.Repeat("e", 64)
	if err := os.WriteFile(credential, []byte(token), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TCLAUDE_BACKEND_SOCKET", socket)
	t.Setenv("TCLAUDE_BACKEND_CREDENTIAL_FILE", credential)
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	received := make(chan map[string]any, 1)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(401)
			return
		}
		if r.URL.Path != "/v2/inbox/message-a/read" {
			t.Errorf("unexpected route: %s", r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		received <- body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"read":true}`))
	})}
	defer server.Close()
	go func() { _ = server.Serve(listener) }()
	cmd := ClientCommand()
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	cmd.SetArgs([]string{"read", "message-a", "--request-id", "read-one"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	body := <-received
	if len(body) != 1 || body["request_id"] != "read-one" {
		t.Fatalf("client supplied identity or lost idempotency: %+v", body)
	}
	if !strings.Contains(output.String(), `"read": true`) || strings.Contains(output.String(), token) {
		t.Fatalf("unsafe or missing output: %s", output.String())
	}
}

func TestAgentCLIHasNoOperatorFallback(t *testing.T) {
	t.Setenv("TCLAUDE_BACKEND_SOCKET", "")
	t.Setenv("TCLAUDE_BACKEND_CREDENTIAL_FILE", "")
	cmd := ClientCommand()
	cmd.SetArgs([]string{"whoami"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("missing execution delivery metadata accepted")
	}
	cmd = ClientCommand()
	cmd.SetArgs([]string{"read", "message-a"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("mutation without request identity accepted")
	}
}
