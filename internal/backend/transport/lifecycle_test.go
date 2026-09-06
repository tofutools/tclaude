package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
	"github.com/tofutools/tclaude/internal/backend/providers"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
)

// Only native effects are doubled; requests exercise transport, application and SQLite.
type lifecycleProvider struct {
	ports.Provider
	releases int
}

func (*lifecycleProvider) Name() string { return "test-native" }
func (*lifecycleProvider) Capabilities() ports.ProviderCapabilities {
	return ports.ProviderCapabilities{}
}
func (p *lifecycleProvider) Prepare(_ context.Context, r ports.PreparationRequest) (ports.PreparedAttempt, error) {
	return &preparedLifecycle{p: p, spec: r.Spec, sink: r.Observations}, nil
}

type preparedLifecycle struct {
	p    *lifecycleProvider
	spec model.ResolvedExecutionSpec
	sink ports.PrimaryObservationSink
}

func (p *preparedLifecycle) Describe() ports.PreparedDescription {
	return ports.PreparedDescription{ExecutionID: p.spec.ExecutionID, Topology: ports.TopologyTerminalAuthoritative,
		Evidence: lifecycleEvidence(), EffectivePolicy: ports.EffectivePolicy{Approval: p.spec.Approval, Sandbox: p.spec.Sandbox, ApprovalEnforced: true, SandboxEnforced: true}}
}
func (*preparedLifecycle) Abort(context.Context) error { return nil }
func (p *preparedLifecycle) Release(ctx context.Context, permit ports.ReleasePermit) (ports.ReleaseResult, error) {
	if err := permit.Consume(ctx); err != nil {
		return ports.ReleaseResult{}, err
	}
	p.p.releases++
	return ports.ReleaseResult{State: ports.ReleaseStarted, Runtime: &lifecycleRuntime{id: p.spec.ExecutionID, attempt: p.spec.Attempt, sink: p.sink}, Evidence: lifecycleEvidence()}, nil
}

type lifecycleRuntime struct {
	ports.Runtime
	id       model.ExecutionID
	exited   bool
	attempt  model.AttemptGeneration
	sink     ports.PrimaryObservationSink
	binding  *model.NativeBinding
	order    string
	sequence int
}

func lifecycleEvidence() model.ProviderEvidence {
	return model.ProviderEvidence{Provider: "test-native", Version: 1, Payload: []byte(`{}`)}
}
func (r *lifecycleRuntime) ExecutionID() model.ExecutionID { return r.id }
func (r *lifecycleRuntime) Observe(ctx context.Context) (ports.Observation, error) {
	if !r.exited && r.order == "" && r.sink != nil {
		binding := &model.NativeBinding{Namespace: "test-store", Reference: string(r.id)}
		if err := r.sink.ObservePrimaryContext(ctx, ports.PrimaryContextEvidence{ExecutionID: r.id, Attempt: r.attempt, Provider: "test-native", PrimaryCorrelation: "fixture-primary", Disposition: ports.PrimaryContextInitial, NextBinding: binding, ProviderOrder: "initial", ObservedAt: time.Now()}); err != nil {
			return ports.Observation{}, err
		}
		r.binding, r.order = binding, "initial"
	}
	state := ports.WorkloadRunning
	if r.exited {
		state = ports.WorkloadExited
	}
	var native *model.NativeConversationEvidence
	if r.binding != nil {
		native = &model.NativeConversationEvidence{Namespace: r.binding.Namespace, Reference: r.binding.Reference}
	}
	return ports.Observation{ObservedAt: time.Now(), Workload: state, Context: ports.ContextReady, Evidence: lifecycleEvidence(), NativeConversation: native}, nil
}
func (r *lifecycleRuntime) Stop(context.Context, ports.StopRequest) (ports.StopResult, error) {
	r.exited = true
	return ports.StopResult{Disposition: ports.EffectAccepted, Acknowledged: true, Exited: true, Evidence: lifecycleEvidence()}, nil
}
func TestTransportDurableLaunchRetryAndStop(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "backend.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	native := &lifecycleProvider{}
	h := testHandler(t, app.New(store, providers.NewRegistry(native)))
	body := `{"request_id":"launch-one","target":{"standalone":{"desired":{"Harness":"test-native","WorkingDirectory":"/tmp","Approval":"supervised","Sandbox":"unconfined"}}}}`
	w := request(h, "POST", "/v2/launch", body, testCredential)
	var launched struct {
		Operation operationView
		Execution executionView
		Repeated  bool
	}
	if w.Code != 202 {
		t.Fatalf("launch %d %s", w.Code, w.Body)
	}
	if err := json.Unmarshal(w.Body.Bytes(), &launched); err != nil {
		t.Fatal(err)
	}
	if launched.Execution.ID == "" || launched.Execution.AgentID != "" {
		t.Fatalf("standalone execution: %+v", launched)
	}
	w = request(h, "POST", "/v2/launch", body, testCredential)
	var repeated struct {
		Operation operationView
		Repeated  bool
	}
	if err := json.Unmarshal(w.Body.Bytes(), &repeated); err != nil {
		t.Fatal(err)
	}
	if w.Code != 202 || !repeated.Repeated || repeated.Operation.ID != launched.Operation.ID || native.releases != 1 {
		t.Fatalf("retry: %d %s, releases %d", w.Code, w.Body, native.releases)
	}
	w = request(h, "POST", "/v2/stop", fmt.Sprintf(`{"request_id":"stop-one","execution_id":%q}`, launched.Execution.ID), testCredential)
	if w.Code != 202 {
		t.Fatalf("stop: %d %s", w.Code, w.Body)
	}
	w = request(h, "GET", "/v2/snapshot", "", testCredential)
	var snapshot struct {
		Executions []executionView
		Operations []operationView
	}
	if err := json.Unmarshal(w.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Executions) != 1 || snapshot.Executions[0].State != model.ExecutionExited || len(snapshot.Operations) != 2 {
		t.Fatalf("settled snapshot: %+v", snapshot)
	}
}

func (r *lifecycleRuntime) ChangeContext(ctx context.Context, change ports.ContextChange) (ports.ContextChangeResult, error) {
	r.sequence++
	next := &model.NativeBinding{Namespace: "test-store", Reference: fmt.Sprintf("%s-context-%d", r.id, r.sequence)}
	order := fmt.Sprintf("context-%d", r.sequence)
	if err := r.sink.ObservePrimaryContext(ctx, ports.PrimaryContextEvidence{ExecutionID: r.id, Attempt: r.attempt, Provider: "test-native", PrimaryCorrelation: "fixture-primary", Disposition: ports.PrimaryContextReset, PriorBinding: r.binding, NextBinding: next, PriorProviderOrder: r.order, ProviderOrder: order, ObservedAt: time.Now(), TransitionCorrelation: change.TransitionCorrelation, ExpectedConversation: change.ExpectedConversation, ExpectedAssociationRevision: change.ExpectedAssociationRevision}); err != nil {
		return ports.ContextChangeResult{}, err
	}
	r.binding, r.order = next, order
	return ports.ContextChangeResult{Disposition: ports.EffectAccepted, Evidence: lifecycleEvidence(), NativeConversation: &model.NativeConversationEvidence{Namespace: next.Namespace, Reference: next.Reference}}, nil
}

func TestContextUsesAssociationRevisionReadFromAPI(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "backend.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	h := testHandler(t, app.New(store, providers.NewRegistry(&lifecycleProvider{})))
	desired := `{"Harness":"test-native","WorkingDirectory":"/tmp","Approval":"supervised","Sandbox":"unconfined"}`
	w := request(h, "POST", "/v2/agents", `{"id":"worker","name":"worker","desired":`+desired+`}`, testCredential)
	var agent model.Agent
	if err := json.Unmarshal(w.Body.Bytes(), &agent); err != nil || w.Code != 201 {
		t.Fatalf("agent %s %v", w.Body, err)
	}
	w = request(h, "POST", "/v2/launch", fmt.Sprintf(`{"request_id":"agent-launch","target":{"agent":{"agent_id":"worker","expected_revision":%d}}}`, agent.Revision), testCredential)
	if w.Code != 202 {
		t.Fatal(w.Body.String())
	}
	var lastRevision model.Revision
	for attempt := 0; attempt < 2; attempt++ {
		w = request(h, "GET", "/v2/snapshot", "", testCredential)
		var state struct {
			Executions    []executionView
			Associations  []model.ConversationAssociation
			Conversations []model.Conversation
		}
		if err := json.Unmarshal(w.Body.Bytes(), &state); err != nil {
			t.Fatal(err)
		}
		var selected model.ConversationAssociation
		for _, association := range state.Associations {
			if association.AgentID == "worker" && association.Current {
				selected = association
			}
		}
		if selected.Revision == 0 || selected.Revision <= lastRevision {
			t.Fatalf("current revision unreadable: %+v", state)
		}
		lastRevision = selected.Revision
		found := false
		for _, conversation := range state.Conversations {
			if conversation.ID == selected.ConversationID && conversation.Revision > 0 {
				found = true
			}
		}
		if !found {
			t.Fatal("selected conversation missing from API snapshot")
		}
		w = request(h, "POST", "/v2/context", fmt.Sprintf(`{"request_id":"context-%d","execution_id":%q,"intent":"reset","expected_conversation_id":%q,"expected_association_revision":%d}`, attempt, state.Executions[0].ID, selected.ConversationID, selected.Revision), testCredential)
		if w.Code != 202 {
			t.Fatalf("context based on public selection: %d %s", w.Code, w.Body)
		}
	}
}
