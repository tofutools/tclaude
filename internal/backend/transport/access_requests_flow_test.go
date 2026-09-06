package transport

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/client"
	"github.com/tofutools/tclaude/internal/backend/host"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
	"github.com/tofutools/tclaude/internal/backend/providers"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
)

func TestAuthenticatedAccessRequestApprovalExplicitRetryIsolationAndRestart(t *testing.T) {
	ctx := context.Background()
	root, err := os.MkdirTemp("", "access-flow-")
	require.NoError(t, err)
	defer os.RemoveAll(root)
	socket, dbPath := filepath.Join(root, "api.sock"), filepath.Join(root, "state.sqlite")
	operatorFile := filepath.Join(root, "operator")
	require.NoError(t, os.WriteFile(operatorFile, []byte(testCredential), 0600))
	provider := &deliveredLifecycleProvider{
		delivery: host.ActionCredentialHost{PrivateRoot: filepath.Join(root, "credentials")},
		receipts: map[model.ExecutionID]ports.ActionCredentialReceipt{},
		runtimes: map[model.ExecutionID]*lifecycleRuntime{},
	}
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	var store *sqlite.Store
	var service *app.Service
	var stopHTTP func()
	start := func(recoverRuntime bool) {
		var err error
		store, err = sqlite.Open(dbPath)
		require.NoError(t, err)
		service = app.New(store, providers.NewRegistry(provider)).WithAgentAPIEndpoint(socket).WithClock(func() time.Time { return now })
		if recoverRuntime {
			_, err = service.Recover(ctx, app.RecoverRequest{Principal: model.OperatorPrincipal()})
			require.NoError(t, err)
		}
		operatorToken, err := NewOperatorToken(testCredential)
		require.NoError(t, err)
		callers, err := NewCallerAuthenticator(operatorToken, service)
		require.NoError(t, err)
		handler, err := NewHandler(service, callers)
		require.NoError(t, err)
		require.NoError(t, handler.RegisterAgentAPI(service, service))
		require.NoError(t, handler.RegisterAccessRequestAPI(service))
		listener, err := net.Listen("unix", socket)
		require.NoError(t, err)
		httpServer := &http.Server{Handler: handler, ReadHeaderTimeout: time.Second}
		done := make(chan struct{})
		go func() { defer close(done); _ = httpServer.Serve(listener) }()
		stopHTTP = func() {
			_ = httpServer.Close()
			<-done
			_ = os.Remove(socket)
		}
	}
	start(false)
	defer func() {
		stopHTTP()
		require.NoError(t, store.Close())
	}()

	operator, err := client.New(socket, operatorFile)
	require.NoError(t, err)
	defer operator.Close()
	call := func(c *client.Client, method, path string, body, result any) error {
		t.Helper()
		return c.Call(ctx, method, path, body, result)
	}
	desired := model.DesiredConfiguration{Harness: "test-native", WorkingDirectory: root, Approval: model.ApprovalSupervised, Sandbox: model.SandboxUnconfined}
	createAndLaunch := func(id string) (model.Agent, model.ExecutionID, string) {
		var agent model.Agent
		require.NoError(t, call(operator, http.MethodPost, "/v2/agents", map[string]any{"id": id, "name": id, "desired": desired}, &agent))
		var launch struct {
			Execution struct {
				ID model.ExecutionID `json:"id"`
			} `json:"execution"`
		}
		require.NoError(t, call(operator, http.MethodPost, "/v2/launch", map[string]any{
			"request_id": "launch_" + id,
			"target":     map[string]any{"agent": map[string]any{"agent_id": agent.ID, "expected_revision": agent.Revision}},
		}, &launch))
		return agent, launch.Execution.ID, provider.receipts[launch.Execution.ID].Resource
	}
	_, executionA, credentialA := createAndLaunch("agent_access_a")
	_, executionB, credentialB := createAndLaunch("agent_access_b")
	agentA, err := client.New(socket, credentialA)
	require.NoError(t, err)
	agentB, err := client.New(socket, credentialB)
	require.NoError(t, err)

	stop := func(c *client.Client, requestID string, execution model.ExecutionID) error {
		return call(c, http.MethodPost, "/v2/stop", map[string]any{"request_id": requestID, "execution_id": execution}, new(any))
	}
	requireForbidden(t, stop(agentA, "before_grant", executionA))

	requested, err := agentA.RequestAccess(ctx, client.RequestAccessInput{
		RequestID: "request_stop", Action: model.ActionStop,
		Resource: model.ResourceSelector{Kind: model.ResourceExecution, ExecutionID: executionA},
		Reason:   "allow one bounded stop capability", LifetimeSeconds: 300,
	})
	require.NoError(t, err)
	require.Equal(t, model.AccessRequestPending, requested.Request.State)
	require.Equal(t, model.DecisionAccess, requested.Decision.Kind)
	require.Equal(t, "access_request", requested.Decision.SourceType)
	require.Empty(t, requested.Request.GrantID, "pending projection does not imply a grant")

	repeated, err := agentA.RequestAccess(ctx, client.RequestAccessInput{
		RequestID: "request_stop", Action: model.ActionStop,
		Resource: model.ResourceSelector{Kind: model.ResourceExecution, ExecutionID: executionA},
		Reason:   "allow one bounded stop capability", LifetimeSeconds: 300,
	})
	require.NoError(t, err)
	require.True(t, repeated.Repeated)
	require.Equal(t, requested.Request.ID, repeated.Request.ID)
	other, err := agentB.ListAccessRequests(ctx, false)
	require.NoError(t, err)
	require.Empty(t, other, "another agent cannot enumerate the request")

	approved, err := operator.DecideAccessRequest(ctx, requested.Request.ID, client.DecideAccessInput{
		RequestID: "approve_stop", ExpectedWindowRevision: requested.Decision.Revision,
		Answer: model.AccessAnswerApprove, Reason: "operator accepts exact target",
	})
	require.NoError(t, err)
	require.Equal(t, model.AccessRequestApproved, approved.Request.State)
	require.NotEmpty(t, approved.Request.GrantID)
	requireForbidden(t, stop(agentA, "wrong_resource", executionB))
	require.NoError(t, stop(agentA, "after_grant", executionA), "request approval never replays; this explicit ordinary call uses the live grant")

	duplicateDecision, err := operator.DecideAccessRequest(ctx, requested.Request.ID, client.DecideAccessInput{
		RequestID: "approve_stop", ExpectedWindowRevision: requested.Decision.Revision,
		Answer: model.AccessAnswerApprove, Reason: "operator accepts exact target",
	})
	require.NoError(t, err)
	require.True(t, duplicateDecision.Repeated)
	_, err = operator.DecideAccessRequest(ctx, requested.Request.ID, client.DecideAccessInput{
		RequestID: "competing_decision", ExpectedWindowRevision: requested.Decision.Revision,
		Answer: model.AccessAnswerDeny, Reason: "competing answer",
	})
	requireConflict(t, err)

	revocable, err := agentB.RequestAccess(ctx, client.RequestAccessInput{
		RequestID: "request_revocable", Action: model.ActionStop,
		Resource: model.ResourceSelector{Kind: model.ResourceExecution, ExecutionID: executionB},
		Reason:   "grant will be revoked before use", LifetimeSeconds: 300,
	})
	require.NoError(t, err)
	revocable, err = operator.DecideAccessRequest(ctx, revocable.Request.ID, client.DecideAccessInput{
		RequestID: "approve_revocable", ExpectedWindowRevision: revocable.Decision.Revision,
		Answer: model.AccessAnswerApprove, Reason: "temporarily allowed",
	})
	require.NoError(t, err)
	pending, err := agentB.RequestAccess(ctx, client.RequestAccessInput{
		RequestID: "request_expiring", Action: model.ActionInteract,
		Resource: model.ResourceSelector{Kind: model.ResourceExecution, ExecutionID: executionB},
		Reason:   "short request that must not create authority", LifetimeSeconds: 2,
	})
	require.NoError(t, err)

	agentA.Close()
	agentB.Close()
	operator.Close()
	stopHTTP()
	require.NoError(t, store.Close())
	start(true)
	operator, err = client.New(socket, operatorFile)
	require.NoError(t, err)
	agentB, err = client.New(socket, credentialB)
	require.NoError(t, err)
	restartedApproved, err := operator.GetAccessRequest(ctx, requested.Request.ID)
	require.NoError(t, err)
	require.Equal(t, model.AccessRequestApproved, restartedApproved.Request.State)
	restartedPending, err := operator.GetAccessRequest(ctx, pending.Request.ID)
	require.NoError(t, err)
	require.Equal(t, model.AccessRequestPending, restartedPending.Request.State)
	require.NoError(t, call(operator, http.MethodDelete, "/v2/authority/grants/"+string(revocable.Request.GrantID), map[string]any{"expected_revision": 1}, nil))
	requireForbidden(t, stop(agentB, "revoked_grant", executionB))

	now = now.Add(3 * time.Second)
	expired, err := operator.GetAccessRequest(ctx, pending.Request.ID)
	require.NoError(t, err)
	require.Equal(t, model.AccessRequestExpired, expired.Request.State)
	require.Equal(t, model.DecisionExpired, expired.Decision.State)
	_, err = operator.DecideAccessRequest(ctx, pending.Request.ID, client.DecideAccessInput{
		RequestID: "late_approval", ExpectedWindowRevision: pending.Decision.Revision,
		Answer: model.AccessAnswerApprove, Reason: "too late",
	})
	requireConflict(t, err)
	requireForbidden(t, call(agentB, http.MethodPost, "/v2/interact", map[string]any{"request_id": "expired_effect", "execution_id": executionB, "text": "still denied"}, new(any)))
}

func requireForbidden(t *testing.T, err error) {
	t.Helper()
	var failure *client.Error
	require.ErrorAs(t, err, &failure)
	require.Equal(t, http.StatusForbidden, failure.Status)
}

func requireConflict(t *testing.T, err error) {
	t.Helper()
	var failure *client.Error
	require.True(t, errors.As(err, &failure), "expected client conflict, got %v", err)
	require.Equal(t, http.StatusConflict, failure.Status)
}
