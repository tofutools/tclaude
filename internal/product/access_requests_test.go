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

func TestAccessRequestCLIUsesAuthenticatedActorAndExplicitDecisionCommands(t *testing.T) {
	root, err := os.MkdirTemp("", "access-cli-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	socket := filepath.Join(root, "api.sock")
	credential := filepath.Join(root, "credential")
	token := strings.Repeat("a", 64)
	if err := os.WriteFile(credential, []byte(token), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TCLAUDE_BACKEND_SOCKET", socket)
	t.Setenv("TCLAUDE_BACKEND_CREDENTIAL_FILE", credential)
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	type receivedRequest struct {
		Method string
		Path   string
		Body   map[string]any
	}
	received := make(chan receivedRequest, 2)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var body map[string]any
		if r.Body != nil && r.ContentLength != 0 {
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
		}
		received <- receivedRequest{Method: r.Method, Path: r.URL.RequestURI(), Body: body}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"request":{"id":"access_one"},"decision":{"decision_id":"decision_one"},"repeated":false}`))
	})}
	defer server.Close()
	go func() { _ = server.Serve(listener) }()

	write := func(name, content string) string {
		path := filepath.Join(root, name)
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	requestFile := write("request.json", `{"request_id":"ask_one","action":"message.send","resource":{"Kind":"agent","AgentID":"target"},"bounds":{},"reason":"exact recipient","lifetime_seconds":60}`)
	decisionFile := write("decision.json", `{"request_id":"decide_one","expected_window_revision":1,"reason":"reviewed"}`)
	run := func(args ...string) error {
		command := ClientCommand()
		var output bytes.Buffer
		command.SetOut(&output)
		command.SetErr(&output)
		command.SetArgs(args)
		return command.Execute()
	}
	if err := run("access-request", "ask", "--file", requestFile); err != nil {
		t.Fatal(err)
	}
	if err := run("access-request", "approve", "access_one", "--file", decisionFile); err != nil {
		t.Fatal(err)
	}
	ask := <-received
	if ask.Method != http.MethodPost || ask.Path != "/v2/access-requests" || ask.Body["request_id"] != "ask_one" {
		t.Fatalf("unexpected ask: %+v", ask)
	}
	if _, exists := ask.Body["principal"]; exists {
		t.Fatalf("CLI accepted a body principal: %+v", ask.Body)
	}
	decision := <-received
	if decision.Method != http.MethodPost || decision.Path != "/v2/access-requests/access_one/decision" || decision.Body["answer"] != "approve" || decision.Body["request_id"] != "decide_one" {
		t.Fatalf("unexpected decision: %+v", decision)
	}
}
