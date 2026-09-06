//go:build linux || darwin

package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

type testPermit struct {
	execution model.ExecutionID
	operation model.OperationID
	consumed  atomic.Bool
}

func (p *testPermit) ExecutionID() model.ExecutionID { return p.execution }
func (p *testPermit) OperationID() model.OperationID { return p.operation }
func (p *testPermit) Consume(context.Context) error {
	if !p.consumed.CompareAndSwap(false, true) {
		return os.ErrPermission
	}
	return nil
}

func TestServerProviderLaunchInteractionAttachmentRecoveryAndStop(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "tclaude-opencode-provider-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(root)) })
	executable := filepath.Join(root, "opencode-fake")
	script := "#!/bin/sh\nexec \"$OPENCODE_TEST_BINARY\" -test.run=TestOpenCodeServerHelper -- \"$@\"\n"
	require.NoError(t, os.WriteFile(executable, []byte(script), 0o700))
	promptPath := filepath.Join(root, "prompt")
	provider, err := New(Config{
		Executable: executable, PrivateRoot: root,
		Environment: []string{"OPENCODE_TEST_BINARY=" + os.Args[0], "OPENCODE_TEST_PROMPT=" + promptPath},
	})
	require.NoError(t, err)
	request := ports.PreparationRequest{Intent: ports.StartFresh, Spec: model.ResolvedExecutionSpec{
		ExecutionID: "execution_opencode", Harness: Name, Model: "provider/model",
		WorkingDirectory: root, Approval: model.ApprovalSupervised, Sandbox: model.SandboxUnconfined,
	}}
	prepared, err := provider.Prepare(context.Background(), request)
	require.NoError(t, err)
	description := prepared.Describe()
	require.Equal(t, ports.TopologyIndependentServer, description.Topology)
	require.Equal(t, []model.SandboxMode{model.SandboxUnconfined}, description.Requirements.Policy.SupportedSandbox)
	require.True(t, description.EffectivePolicy.SandboxEnforced,
		"the explicit absence of confinement is preserved without claiming native rules are a sandbox")

	permit := &testPermit{execution: request.Spec.ExecutionID, operation: "operation_launch"}
	released, err := prepared.Release(context.Background(), permit)
	require.NoError(t, err)
	require.True(t, permit.consumed.Load())
	require.Equal(t, ports.ReleaseStarted, released.State)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, _ = released.Runtime.Stop(ctx, ports.StopRequest{Force: true})
	})

	observation, err := released.Runtime.Observe(context.Background())
	require.NoError(t, err)
	require.Equal(t, ports.WorkloadRunning, observation.Workload)
	require.Equal(t, ports.ContextReady, observation.Context)
	require.False(t, observation.AttachmentActive)
	require.Equal(t, "ses_test", observation.NativeConversation.Reference)

	interaction, err := released.Runtime.Interact(context.Background(), ports.Interaction{Text: "perform work"})
	require.NoError(t, err)
	require.Equal(t, ports.EffectAccepted, interaction.Disposition)
	require.Eventually(t, func() bool {
		value, readErr := os.ReadFile(promptPath)
		return readErr == nil && strings.Contains(string(value), "perform work") && strings.Contains(string(value), "providerID")
	}, time.Second, 10*time.Millisecond)

	attachment, err := released.Runtime.Attach(context.Background(), ports.AttachmentRequest{Kind: ports.AttachmentTerminal})
	require.NoError(t, err)
	require.Equal(t, ports.EffectAccepted, attachment.Disposition)
	require.NoError(t, attachment.Attachment.Close())
	require.NoError(t, attachment.Attachment.Close(), "attachment close is idempotent")
	observation, err = released.Runtime.Observe(context.Background())
	require.NoError(t, err)
	require.Equal(t, ports.WorkloadRunning, observation.Workload,
		"closing an attach client must not stop the authoritative server")
	require.False(t, observation.AttachmentActive)

	recovered, err := provider.Recover(context.Background(), ports.RecoveryRequest{
		ExecutionID: request.Spec.ExecutionID, Spec: request.Spec, Evidence: released.Evidence,
	})
	require.NoError(t, err)
	require.Equal(t, ports.RecoveryControlled, recovered.State)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	stopped, err := recovered.Runtime.Stop(ctx, ports.StopRequest{Force: true})
	require.NoError(t, err)
	require.True(t, stopped.Acknowledged)
	require.True(t, stopped.Exited)
}

func TestProviderRefusesConfinementInsteadOfDowngrading(t *testing.T) {
	root := t.TempDir()
	provider, err := New(Config{Executable: os.Args[0], PrivateRoot: root})
	require.NoError(t, err)
	for _, sandbox := range []model.SandboxMode{model.SandboxReadOnly, model.SandboxWorkspaceWrite} {
		_, err := provider.Prepare(context.Background(), ports.PreparationRequest{
			Intent: ports.StartFresh,
			Spec: model.ResolvedExecutionSpec{ExecutionID: "execution_refused", Harness: Name,
				WorkingDirectory: root, Approval: model.ApprovalSupervised, Sandbox: sandbox},
		})
		require.ErrorContains(t, err, "explicitly selected")
	}
}

func TestOpenCodeServerHelper(t *testing.T) {
	args := argumentsAfterDoubleDash(os.Args)
	if len(args) == 0 {
		return
	}
	if args[0] == "attach" {
		fmt.Println("attached")
		select {}
	}
	if args[0] != "serve" {
		return
	}
	port := argumentValue(args, "--port")
	listener, err := net.Listen("tcp", "127.0.0.1:"+port)
	require.NoError(t, err)
	password := os.Getenv("OPENCODE_SERVER_PASSWORD")
	mux := http.NewServeMux()
	mux.HandleFunc("/global/health", func(writer http.ResponseWriter, request *http.Request) {
		if !validBasicAuth(request, password) {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(writer).Encode(map[string]bool{"healthy": true})
	})
	mux.HandleFunc("/session", func(writer http.ResponseWriter, request *http.Request) {
		if !validBasicAuth(request, password) {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(writer).Encode(map[string]string{"id": "ses_test"})
	})
	mux.HandleFunc("/session/", func(writer http.ResponseWriter, request *http.Request) {
		if !validBasicAuth(request, password) {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		if strings.HasSuffix(request.URL.Path, "/prompt_async") {
			body := make(map[string]any)
			_ = json.NewDecoder(request.Body).Decode(&body)
			encoded, _ := json.Marshal(body)
			_ = os.WriteFile(os.Getenv("OPENCODE_TEST_PROMPT"), encoded, 0o600)
			writer.WriteHeader(http.StatusNoContent)
			return
		}
		_ = json.NewEncoder(writer).Encode(map[string]string{"id": "ses_test"})
	})
	require.NoError(t, http.Serve(listener, mux))
}

func argumentsAfterDoubleDash(args []string) []string {
	for index, value := range args {
		if value == "--" && index+1 < len(args) {
			return args[index+1:]
		}
	}
	return nil
}

func argumentValue(args []string, name string) string {
	for index, value := range args {
		if value == name && index+1 < len(args) {
			return args[index+1]
		}
	}
	return strconv.Itoa(0)
}

func validBasicAuth(request *http.Request, password string) bool {
	username, supplied, ok := request.BasicAuth()
	return ok && username == serverUsername && supplied == password
}
