package transport

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/client"
	"github.com/tofutools/tclaude/internal/backend/host"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
	"github.com/tofutools/tclaude/internal/backend/providers"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
)

// Only the native workload is doubled. Delivery, authentication, durable state,
// wire projection and the client's renewed-resource read are production code.
type deliveredLifecycleProvider struct {
	lifecycleProvider
	delivery ports.ActionCredentialDelivery
	receipts map[model.ExecutionID]ports.ActionCredentialReceipt
	runtimes map[model.ExecutionID]*lifecycleRuntime
}

// Cancellation is injected only after production host replacement succeeds.
// SQLite settlement must outlive it, or the resource and database generations diverge.
type cancelAfterRotation struct {
	ports.ActionCredentialDelivery
	cancel context.CancelFunc
}

func (d cancelAfterRotation) RotateActionCredential(ctx context.Context, current ports.ActionCredentialReceipt, material ports.ActionCredentialMaterial) (ports.ActionCredentialReceipt, error) {
	receipt, err := d.ActionCredentialDelivery.RotateActionCredential(ctx, current, material)
	if err == nil {
		d.cancel()
	}
	return receipt, err
}

func (p *deliveredLifecycleProvider) ActionCredentials() ports.ActionCredentialDelivery {
	return p.delivery
}
func (p *deliveredLifecycleProvider) Prepare(ctx context.Context, req ports.PreparationRequest) (ports.PreparedAttempt, error) {
	native, err := p.lifecycleProvider.Prepare(ctx, req)
	if err != nil {
		return nil, err
	}
	receipt, err := p.delivery.PrepareActionCredential(ctx, *req.ActionCredential)
	if err != nil {
		return nil, err
	}
	p.receipts[req.Spec.ExecutionID] = receipt
	return &deliveredPrepared{PreparedAttempt: native, provider: p, receipt: receipt}, nil
}

type deliveredPrepared struct {
	ports.PreparedAttempt
	provider *deliveredLifecycleProvider
	receipt  ports.ActionCredentialReceipt
}

func (p *deliveredPrepared) Describe() ports.PreparedDescription {
	d := p.PreparedAttempt.Describe()
	d.AccessDelivery = &p.receipt
	return d
}
func (p *deliveredPrepared) Release(ctx context.Context, permit ports.ReleasePermit) (ports.ReleaseResult, error) {
	result, err := p.PreparedAttempt.Release(ctx, permit)
	if runtime, ok := result.Runtime.(*lifecycleRuntime); ok {
		p.provider.runtimes[runtime.id] = runtime
	}
	return result, err
}
func (p *deliveredLifecycleProvider) Recover(ctx context.Context, req ports.RecoveryRequest) (ports.RecoveryResult, error) {
	runtime := p.runtimes[req.ExecutionID]
	if runtime == nil {
		return ports.RecoveryResult{State: ports.RecoveryUnknown}, nil
	}
	runtime.sink = req.Observations
	observed, err := runtime.Observe(ctx)
	if err != nil {
		return ports.RecoveryResult{}, err
	}
	result := ports.RecoveryResult{State: ports.RecoveryControlled, Runtime: runtime, Observation: observed, Evidence: lifecycleEvidence(), Attempt: req.Attempt}
	if req.Access != nil {
		proof, err := p.delivery.InspectActionCredential(ctx, *req.Access)
		if err != nil {
			return ports.RecoveryResult{}, err
		}
		result.AccessProof = &proof
	}
	return result, nil
}

func TestExecutionClientCredentialRenewalRevocationAndRestart(t *testing.T) {
	ctx := context.Background()
	root, err := os.MkdirTemp("", "backend-agent-flow-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	socket, dbPath := filepath.Join(root, "api.sock"), filepath.Join(root, "state.sqlite")
	operatorFile := filepath.Join(root, "operator")
	if err := os.WriteFile(operatorFile, []byte(testCredential), 0600); err != nil {
		t.Fatal(err)
	}
	provider := &deliveredLifecycleProvider{delivery: host.ActionCredentialHost{PrivateRoot: filepath.Join(root, "credentials")}, receipts: map[model.ExecutionID]ports.ActionCredentialReceipt{}, runtimes: map[model.ExecutionID]*lifecycleRuntime{}}
	var store *sqlite.Store
	var service *app.Service
	var stopHTTP func()
	start := func() {
		var err error
		store, err = sqlite.Open(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		service = app.New(store, providers.NewRegistry(provider)).WithAgentAPIEndpoint(socket)
		operator, err := NewOperatorToken(testCredential)
		if err != nil {
			t.Fatal(err)
		}
		callers, err := NewCallerAuthenticator(operator, service)
		if err != nil {
			t.Fatal(err)
		}
		handler, err := NewHandler(service, callers)
		if err != nil {
			t.Fatal(err)
		}
		if err := handler.RegisterAgentAPI(service, service); err != nil {
			t.Fatal(err)
		}
		listener, err := net.Listen("unix", socket)
		if err != nil {
			t.Fatal(err)
		}
		server := &http.Server{Handler: handler, ReadHeaderTimeout: time.Second}
		done := make(chan struct{})
		go func() { defer close(done); _ = server.Serve(listener) }()
		stopHTTP = func() { _ = server.Close(); <-done }
	}
	start()
	defer func() { stopHTTP(); _ = store.Close() }()
	operator, err := client.New(socket, operatorFile)
	if err != nil {
		t.Fatal(err)
	}
	defer operator.Close()
	call := func(c *client.Client, method, path string, body, result any) {
		t.Helper()
		if err := c.Call(ctx, method, path, body, result); err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
	}
	desired := model.DesiredConfiguration{Harness: "test-native", WorkingDirectory: root, Approval: model.ApprovalSupervised, Sandbox: model.SandboxUnconfined}
	var agent model.Agent
	call(operator, "POST", "/v2/agents", map[string]any{"id": "worker", "name": "Worker", "desired": desired}, &agent)
	var launch struct{ Execution executionView }
	call(operator, "POST", "/v2/launch", map[string]any{"request_id": "launch-worker", "target": map[string]any{"agent": map[string]any{"agent_id": agent.ID, "expected_revision": agent.Revision}}}, &launch)
	credentialFile := provider.receipts[launch.Execution.ID].Resource
	agentClient, err := client.New(socket, credentialFile)
	if err != nil {
		t.Fatal(err)
	}
	defer agentClient.Close()
	var identity struct {
		Agent     model.Agent
		Execution executionView
	}
	call(agentClient, "GET", "/v2/identity", nil, &identity)
	if identity.Agent.ID != agent.ID || identity.Execution.ID != launch.Execution.ID {
		t.Fatalf("wrong authenticated identity: %+v", identity)
	}
	var recipient model.Agent
	call(operator, "POST", "/v2/agents", map[string]any{"id": "recipient", "name": "Offline recipient", "desired": desired}, &recipient)
	call(operator, "PUT", "/v2/authority/grants/mail-grant", map[string]any{"subject": model.AuthoritySubject{Kind: model.AuthorityAgent, AgentID: agent.ID}, "action": model.ActionSendMessage, "resource": model.ResourceSelector{Kind: model.ResourceAgent, AgentID: recipient.ID}, "expected_revision": 0}, nil)
	mail := map[string]any{"request_id": "agent-mail", "recipients": []model.AgentID{recipient.ID}, "body": "accepted once across renewal"}
	var accepted model.Message
	call(agentClient, "POST", "/v2/messages", mail, &accepted)
	// A manager must obtain the peer's association revision through the same
	// scoped public status route used by the client, without an operator snapshot.
	var peerLaunch struct{ Execution executionView }
	call(operator, "POST", "/v2/launch", map[string]any{"request_id": "launch-peer", "target": map[string]any{"agent": map[string]any{"agent_id": recipient.ID, "expected_revision": recipient.Revision}}}, &peerLaunch)
	for grantID, action := range map[string]model.Action{"peer-status": model.ActionReadStatus, "peer-context": model.ActionChangeContext} {
		resource := model.ResourceSelector{Kind: model.ResourceAgent, AgentID: recipient.ID}
		if action == model.ActionChangeContext {
			resource = model.ResourceSelector{Kind: model.ResourceExecution, ExecutionID: peerLaunch.Execution.ID}
		}
		call(operator, "PUT", "/v2/authority/grants/"+grantID, map[string]any{"subject": model.AuthoritySubject{Kind: model.AuthorityAgent, AgentID: agent.ID}, "action": action, "resource": resource, "expected_revision": 0}, nil)
	}
	var peerStatus struct {
		Executions   []executionView
		Associations []model.ConversationAssociation
	}
	call(agentClient, "POST", "/v2/status", map[string]any{"target": model.ResourceSelector{Kind: model.ResourceAgent, AgentID: recipient.ID}}, &peerStatus)
	if len(peerStatus.Associations) != 1 || peerStatus.Associations[0].AgentID != recipient.ID || peerStatus.Associations[0].Revision == 0 || len(peerStatus.Executions) != 1 || peerStatus.Executions[0].ContextReadiness != model.ContextReadinessReady {
		t.Fatalf("scoped peer selection missing or leaks another agent: %+v", peerStatus)
	}
	selected := peerStatus.Associations[0]
	var changed struct{ Operation operationView }
	call(agentClient, "POST", "/v2/context", map[string]any{"request_id": "peer-context", "execution_id": peerLaunch.Execution.ID, "intent": "reset", "expected_conversation_id": selected.ConversationID, "expected_association_revision": selected.Revision}, &changed)
	if changed.Operation.State != model.OperationSucceeded {
		t.Fatalf("context from public peer revision did not settle: %+v", changed)
	}
	call(agentClient, "POST", "/v2/status", map[string]any{"target": model.ResourceSelector{Kind: model.ResourceAgent, AgentID: recipient.ID}}, &peerStatus)
	if len(peerStatus.Associations) != 1 || peerStatus.Associations[0].ConversationID == selected.ConversationID || peerStatus.Associations[0].Revision <= selected.Revision {
		t.Fatalf("peer's revised selection unavailable: %+v", peerStatus)
	}
	oldCredential, err := os.ReadFile(credentialFile)
	if err != nil {
		t.Fatal(err)
	}
	var access accessView
	call(operator, "GET", "/v2/executions/"+string(launch.Execution.ID)+"/access", nil, &access)
	renewalContext, cancelRenewal := context.WithCancel(ctx)
	defer cancelRenewal()
	delivery := provider.delivery
	provider.delivery = cancelAfterRotation{ActionCredentialDelivery: delivery, cancel: cancelRenewal}
	if _, err := service.RenewExecutionAccess(renewalContext, app.RenewExecutionAccessRequest{ExecutionID: launch.Execution.ID, ExpectedRevision: access.Revision}); err != nil {
		t.Fatal(err)
	}
	provider.delivery = delivery
	if renewalContext.Err() == nil {
		t.Fatal("test did not cancel after successful host rotation")
	}
	call(agentClient, "GET", "/v2/identity", nil, &identity)
	var repeated model.Message
	call(agentClient, "POST", "/v2/messages", mail, &repeated)
	if repeated.ID != accepted.ID {
		t.Fatal("credential rotation changed durable request identity")
	}
	staleFile := filepath.Join(root, "stale")
	if err := os.WriteFile(staleFile, oldCredential, 0600); err != nil {
		t.Fatal(err)
	}
	staleClient, err := client.New(socket, staleFile)
	if err != nil {
		t.Fatal(err)
	}
	defer staleClient.Close()
	assertDenied := func(c *client.Client) {
		t.Helper()
		var response json.RawMessage
		err := c.Call(ctx, "GET", "/v2/identity", nil, &response)
		var failure *client.Error
		if !errors.As(err, &failure) || failure.Status != 401 {
			t.Fatalf("expected credential refusal, got %v", err)
		}
	}
	assertDenied(staleClient)

	var sent model.Message
	call(operator, "POST", "/v2/messages", map[string]any{"request_id": "durable-mail", "recipients": []model.AgentID{agent.ID}, "body": "survive restart"}, &sent)
	stopHTTP()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	start()
	assertDenied(agentClient) // Opening storage suspends access; reads cannot reactivate it.
	call(operator, "POST", "/v2/recover", struct{}{}, nil)
	call(agentClient, "GET", "/v2/identity", nil, &identity)
	var inbox struct{ Messages []model.Message }
	call(agentClient, "GET", "/v2/inbox?unread_only=true", nil, &inbox)
	if len(inbox.Messages) != 1 || inbox.Messages[0].ID != sent.ID {
		t.Fatalf("accepted inbox lost across restart: %+v", inbox)
	}
	call(agentClient, "POST", "/v2/inbox/"+string(sent.ID)+"/read", map[string]string{"request_id": "read-existing"}, nil)
	call(agentClient, "GET", "/v2/inbox?unread_only=true", nil, &inbox)
	if len(inbox.Messages) != 0 {
		t.Fatal("message read state was not recorded")
	}
	call(operator, "GET", "/v2/executions/"+string(launch.Execution.ID)+"/access", nil, &access)
	currentCredential, err := os.ReadFile(credentialFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staleFile, currentCredential, 0600); err != nil {
		t.Fatal(err)
	}
	call(staleClient, "GET", "/v2/identity", nil, &identity) // The retained copy is current before revocation.
	call(operator, "POST", "/v2/executions/"+string(launch.Execution.ID)+"/access/revoke", map[string]any{"expected_revision": access.Revision}, nil)
	// Revocation removes delivery. A retained copy still cannot authenticate.
	assertDenied(staleClient)
	if _, err := os.Stat(credentialFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("revoked delivery remains: %v", err)
	}
}
