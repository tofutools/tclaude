package product

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers"
	"github.com/tofutools/tclaude/internal/backend/server"
)

func TestOperatorCommandsCreateConfigureAndRevokeThroughBackend(t *testing.T) {
	t.Setenv("TCLAUDE_BACKEND_SOCKET", "")
	t.Setenv("TCLAUDE_BACKEND_CREDENTIAL_FILE", "")
	parent, err := os.MkdirTemp("", "product-cli-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(parent)
	state := filepath.Join(parent, "state")
	if err := server.Initialize(state); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, state, providers.NewRegistry()) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(10 * time.Second):
			t.Error("server failed to join")
		}
	}()
	run := func(args ...string) ([]byte, error) {
		cmd := ClientCommand()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(append([]string{"--operator-state", state}, args...))
		err := cmd.ExecuteContext(ctx)
		return out.Bytes(), err
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := run("snapshot"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("backend unavailable")
		}
		time.Sleep(10 * time.Millisecond)
	}
	file := func(name string, body any) string {
		path := filepath.Join(parent, name+".json")
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	desired := model.DesiredConfiguration{Harness: "claude", Model: "test", WorkingDirectory: parent, Approval: model.ApprovalSupervised, Sandbox: model.SandboxUnconfined}
	create := file("agent", map[string]any{"id": "worker", "name": "Worker", "desired": desired})
	data, err := run("agent", "create", "--file", create)
	if err != nil {
		t.Fatal(err)
	}
	var agent model.Agent
	if err := json.Unmarshal(data, &agent); err != nil {
		t.Fatal(err)
	}
	if agent.ID != "worker" || agent.PrimaryExecutionID != "" {
		t.Fatalf("unexpected offline agent: %+v", agent)
	}
	update := file("update", map[string]any{"name": "Configured worker", "desired": desired, "expected_revision": agent.Revision})
	if _, err := run("agent", "update", "worker", "--file", update); err != nil {
		t.Fatal(err)
	}
	if _, err := run("agent", "update", "worker", "--file", update); err == nil {
		t.Fatal("stale update succeeded")
	}
	grant := file("grant", map[string]any{"subject": model.AuthoritySubject{Kind: model.AuthorityAgent, AgentID: "worker"}, "action": model.ActionReadStatus, "resource": model.ResourceSelector{Kind: model.ResourceAgent, AgentID: "worker"}, "expected_revision": 0})
	data, err = run("authority", "grant", "status-grant", "--file", grant)
	if err != nil {
		t.Fatal(err)
	}
	var saved model.AuthorityGrant
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	revoke := file("revoke", map[string]any{"expected_revision": saved.Revision})
	data, err = run("authority", "revoke-grant", "status-grant", "--file", revoke)
	if err != nil || len(data) != 0 {
		t.Fatalf("successful no-content revoke reported failure: %s %v", data, err)
	}
	data, err = run("authority", "list")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "status-grant") {
		t.Fatal("revoked grant remains active")
	}
	data, err = run("snapshot")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "Configured worker") {
		t.Fatal("updated agent absent from snapshot")
	}
	credential, err := os.ReadFile(filepath.Join(state, "operator.token"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, credential) {
		t.Fatal("credential exposed in public output")
	}
}

func TestClientRejectsMixedOperatorAndExecutionSelection(t *testing.T) {
	t.Setenv("TCLAUDE_BACKEND_SOCKET", "/tmp/delivered.sock")
	t.Setenv("TCLAUDE_BACKEND_CREDENTIAL_FILE", "/tmp/delivered-token")
	cmd := ClientCommand()
	cmd.SetArgs([]string{"--operator-state", "/tmp/operator", "snapshot"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "cannot be combined") {
		t.Fatalf("mixed principal selection accepted: %v", err)
	}
}
