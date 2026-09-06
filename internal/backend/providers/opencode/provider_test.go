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

type observationSink struct {
	values []ports.PrimaryContextEvidence
}

func (s *observationSink) ObservePrimaryContext(_ context.Context, evidence ports.PrimaryContextEvidence) error {
	s.values = append(s.values, evidence)
	return nil
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
	bootstrapPath := filepath.Join(root, "bootstrap")
	agentSocket := filepath.Join(root, "backend.sock")
	provider, err := New(Config{
		Executable: executable, PrivateRoot: root, AgentSocket: agentSocket,
		Environment: []string{"OPENCODE_TEST_BINARY=" + os.Args[0], "OPENCODE_TEST_PROMPT=" + promptPath,
			"OPENCODE_TEST_BOOTSTRAP=" + bootstrapPath},
	})
	require.NoError(t, err)
	observations := &observationSink{}
	request := ports.PreparationRequest{Intent: ports.StartFresh, Observations: observations,
		ActionCredential: &ports.ActionCredentialMaterial{ExecutionID: "execution_opencode", Generation: 1,
			DeliveryID: "delivery-opencode", Secret: []byte("provider-secret-value"), ExpiresAt: time.Now().Add(time.Hour)},
		Spec: model.ResolvedExecutionSpec{
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
	require.NotNil(t, description.AccessDelivery)
	require.NotContains(t, string(description.Evidence.Payload), "provider-secret-value")
	recorded, err := decodeEvidence(description.Evidence)
	require.NoError(t, err)
	require.NotEmpty(t, recorded.PasswordFile)
	serverPassword, err := os.ReadFile(recorded.PasswordFile)
	require.NoError(t, err)
	require.NotEmpty(t, serverPassword)
	require.NotContains(t, string(description.Evidence.Payload), string(serverPassword))
	info, err := os.Stat(recorded.PasswordFile)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	access := &model.ExecutionAccessBinding{ExecutionID: request.Spec.ExecutionID, Generation: 1,
		DeliveryID: "delivery-opencode", State: model.ExecutionAccessSuspended, ExpiresAt: request.ActionCredential.ExpiresAt}

	permit := &testPermit{execution: request.Spec.ExecutionID, operation: "operation_launch"}
	released, err := prepared.Release(context.Background(), permit)
	require.NoError(t, err)
	require.True(t, permit.consumed.Load())
	require.Equal(t, ports.ReleaseStarted, released.State)
	require.Len(t, observations.values, 1)
	require.Equal(t, ports.PrimaryContextInitial, observations.values[0].Disposition)
	require.Eventually(t, func() bool {
		value, readErr := os.ReadFile(bootstrapPath)
		return readErr == nil && strings.Contains(string(value), agentSocket) &&
			strings.Contains(string(value), "provider-secret-value") &&
			!strings.Contains(string(value), "TCLAUDE_BACKEND_CREDENTIAL=provider-secret-value")
	}, time.Second, 10*time.Millisecond)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, _ = released.Runtime.Stop(ctx, ports.StopRequest{Force: true})
	})

	preparedRecovery, err := provider.Recover(context.Background(), ports.RecoveryRequest{
		ExecutionID: request.Spec.ExecutionID, Spec: request.Spec, Evidence: description.Evidence,
		Attempt: request.Spec.Attempt, Access: access, Observations: observations,
	})
	require.NoError(t, err)
	require.Equal(t, ports.RecoveryControlled, preparedRecovery.State,
		"the evidence persisted before release must rediscover the marked server and its private session")
	require.Equal(t, "ses_test", preparedRecovery.Observation.NativeConversation.Reference)
	require.NotNil(t, preparedRecovery.AccessProof)

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
		Attempt: request.Spec.Attempt, Access: access, Observations: observations,
	})
	require.NoError(t, err)
	require.Equal(t, ports.RecoveryControlled, recovered.State)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	controlled := recovered.Runtime.(*Runtime)
	_, exited, err := controlled.process.Stop(ctx, true)
	require.NoError(t, err)
	require.True(t, exited)
	observation, err = recovered.Runtime.Observe(context.Background())
	require.NoError(t, err)
	require.Equal(t, ports.WorkloadExited, observation.Workload)
	require.NoFileExists(t, description.AccessDelivery.Resource)
	require.NoFileExists(t, recorded.PasswordFile)
	afterCleanup, err := provider.Recover(context.Background(), ports.RecoveryRequest{
		ExecutionID: request.Spec.ExecutionID, Spec: request.Spec, Evidence: released.Evidence,
		Attempt: request.Spec.Attempt, Access: access, Observations: observations,
	})
	require.NoError(t, err)
	require.Equal(t, ports.RecoveryExited, afterCleanup.State,
		"confirmed exit remains recoverable after resource cleanup")
	stopped, err := recovered.Runtime.Stop(ctx, ports.StopRequest{Force: true})
	require.NoError(t, err)
	require.True(t, stopped.Exited)
}

func TestOpenCodeResetPublishesConfirmedPrimaryRotation(t *testing.T) {
	root, executable := prepareOpenCodeHelper(t, "rotation")
	sink := &observationSink{}
	provider, err := New(Config{Executable: executable, PrivateRoot: root,
		Environment: []string{"OPENCODE_TEST_BINARY=" + os.Args[0], "OPENCODE_TEST_ROTATE=1"}})
	require.NoError(t, err)
	request := ports.PreparationRequest{Intent: ports.StartFresh, Observations: sink, Spec: model.ResolvedExecutionSpec{
		ExecutionID: "execution_rotation", Attempt: 3, Harness: Name, WorkingDirectory: root,
		Approval: model.ApprovalSupervised, Sandbox: model.SandboxUnconfined,
	}}
	prepared, err := provider.Prepare(context.Background(), request)
	require.NoError(t, err)
	released, err := prepared.Release(context.Background(), &testPermit{execution: request.Spec.ExecutionID, operation: "operation_rotation"})
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, _ = released.Runtime.Stop(ctx, ports.StopRequest{Force: true})
	})
	require.Equal(t, "ses_1", sink.values[0].NextBinding.Reference)

	changed, err := released.Runtime.ChangeContext(context.Background(), ports.ContextChange{
		Intent: ports.ContextReset, ExpectedConversation: "conversation_before", ExpectedAssociationRevision: 5,
		TransitionCorrelation: "transition-issued-by-app",
	})
	require.NoError(t, err)
	require.Equal(t, ports.EffectAccepted, changed.Disposition)
	require.Equal(t, "ses_2", changed.NativeConversation.Reference)
	require.Len(t, sink.values, 2)
	require.Equal(t, ports.PrimaryContextReset, sink.values[1].Disposition)
	require.Equal(t, "ses_1", sink.values[1].PriorBinding.Reference)
	require.Equal(t, "ses_2", sink.values[1].NextBinding.Reference)
	require.Equal(t, "transition-issued-by-app", sink.values[1].TransitionCorrelation)
	require.Equal(t, model.ConversationID("conversation_before"), sink.values[1].ExpectedConversation)
	require.Equal(t, model.Revision(5), sink.values[1].ExpectedAssociationRevision)
	require.Equal(t, sink.values[0].ProviderOrder, sink.values[1].PriorProviderOrder)
}

func TestOpenCodeRejectsNestedSessionAsPrimary(t *testing.T) {
	root, executable := prepareOpenCodeHelper(t, "nested")
	sink := &observationSink{}
	provider, err := New(Config{Executable: executable, PrivateRoot: root,
		Environment: []string{"OPENCODE_TEST_BINARY=" + os.Args[0], "OPENCODE_TEST_PARENT=ses_parent"}})
	require.NoError(t, err)
	request := ports.PreparationRequest{Intent: ports.StartFresh, Observations: sink, Spec: model.ResolvedExecutionSpec{
		ExecutionID: "execution_nested", Attempt: 9, Harness: Name, WorkingDirectory: root,
		Approval: model.ApprovalSupervised, Sandbox: model.SandboxUnconfined,
	}}
	prepared, err := provider.Prepare(context.Background(), request)
	require.NoError(t, err)
	released, err := prepared.Release(context.Background(), &testPermit{execution: request.Spec.ExecutionID, operation: "operation_nested"})
	require.ErrorContains(t, err, "invalid native session")
	require.Equal(t, ports.ReleaseUncertain, released.State)
	require.Empty(t, sink.values)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, _ = released.Runtime.Stop(ctx, ports.StopRequest{Force: true})
}

func prepareOpenCodeHelper(t *testing.T, name string) (string, string) {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "tclaude-opencode-"+name+"-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(root)) })
	executable := filepath.Join(root, "opencode-fake")
	script := "#!/bin/sh\nexec \"$OPENCODE_TEST_BINARY\" -test.run=TestOpenCodeServerHelper -- \"$@\"\n"
	require.NoError(t, os.WriteFile(executable, []byte(script), 0o700))
	return root, executable
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

func TestOpenCodeForkCreatesIndependentStateAndMessagePoint(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "tclaude-opencode-fork-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(root)) })
	workspace := filepath.Join(root, "workspace")
	require.NoError(t, os.Mkdir(workspace, 0o700))
	sourceRoot := filepath.Join(root, "execution-source")
	require.NoError(t, os.Mkdir(sourceRoot, 0o700))
	require.NoError(t, writeHistoryManifest(sourceRoot, historyManifest{NativeID: "ses_source", CWD: workspace}))
	exportPath := filepath.Join(root, "export.json")
	writeOpenCodeExport(t, exportPath, "ses_source", workspace, "source answer")
	forkPointPath := filepath.Join(root, "fork-point")
	importPath := filepath.Join(root, "imported")
	executable := filepath.Join(root, "opencode-fake")
	script := "#!/bin/sh\nif [ \"$1\" = export ]; then cat \"$OPENCODE_EXPORT_FIXTURE\"; exit; fi\nif [ \"$1\" = import ]; then printf imported > \"$OPENCODE_TEST_IMPORT\"; exit; fi\nexec \"$OPENCODE_TEST_BINARY\" -test.run=TestOpenCodeServerHelper -- \"$@\"\n"
	require.NoError(t, os.WriteFile(executable, []byte(script), 0o700))
	provider, err := New(Config{Executable: executable, PrivateRoot: root,
		Environment: []string{"OPENCODE_TEST_BINARY=" + os.Args[0], "OPENCODE_EXPORT_FIXTURE=" + exportPath,
			"OPENCODE_TEST_IMPORT=" + importPath, "OPENCODE_TEST_FORK_POINT=" + forkPointPath}})
	require.NoError(t, err)
	discovered, err := provider.History().Discover(context.Background(), ports.HistoryDiscoveryRequest{})
	require.NoError(t, err)
	require.Len(t, discovered.Histories, 1)
	source := discovered.Histories[0]
	selection := &ports.HistorySourceSelection{ConversationID: "conversation_source", Provider: Name, Native: source.Native,
		SourceToken: source.SourceToken, SourceRevision: source.Coverage.SourceRevision,
		SourceFingerprint: source.SourceFingerprint, Point: &source.Points[0], Evidence: source.Evidence}
	selection.UseClaim = &model.HistoryUseClaim{ID: "history_use", ConversationID: selection.ConversationID,
		OperationID: "operation_fork", SourceRevision: selection.SourceRevision,
		SourceFingerprint: selection.SourceFingerprint, State: model.HistoryUseHeld}
	request := ports.PreparationRequest{Intent: ports.StartFork, History: selection,
		Spec: model.ResolvedExecutionSpec{ExecutionID: "execution_fork", Harness: Name,
			WorkingDirectory: workspace, Approval: model.ApprovalSupervised, Sandbox: model.SandboxUnconfined}}
	prepared, err := provider.Prepare(context.Background(), request)
	require.NoError(t, err)
	preparedEvidence, err := decodeEvidence(prepared.Describe().Evidence)
	require.NoError(t, err)
	require.NotEqual(t, sourceRoot, preparedEvidence.StateRoot)
	require.FileExists(t, importPath)
	wrongPermit := &testPermit{execution: request.Spec.ExecutionID, operation: "operation_other"}
	_, err = prepared.Release(context.Background(), wrongPermit)
	require.ErrorContains(t, err, "history use claim")
	require.False(t, wrongPermit.consumed.Load())
	result, err := prepared.Release(context.Background(), &testPermit{execution: request.Spec.ExecutionID, operation: "operation_fork"})
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, _ = result.Runtime.Stop(ctx, ports.StopRequest{Force: true})
	})
	require.Equal(t, ports.ReleaseStarted, result.State)
	observed, err := result.Runtime.Observe(context.Background())
	require.NoError(t, err)
	require.Equal(t, "ses_fork", observed.NativeConversation.Reference)
	resultEvidence, err := decodeEvidence(result.Evidence)
	require.NoError(t, err)
	require.Equal(t, "ses_source", resultEvidence.ForkSourceID)
	require.Empty(t, resultEvidence.ParentID, "OpenCode native fork is top-level; platform retains lineage")
	point, err := os.ReadFile(forkPointPath)
	require.NoError(t, err)
	require.Equal(t, "msg_one", string(point))
	require.Equal(t, "source answer", mustExportSecondText(t, exportPath), "source export remains unchanged")
}

func TestOpenCodeForkUseClaimBindsRevisionAndFingerprint(t *testing.T) {
	selection := ports.HistorySourceSelection{ConversationID: "conversation_source", SourceRevision: "revision",
		SourceFingerprint: "fingerprint", UseClaim: &model.HistoryUseClaim{ID: "history_use",
			ConversationID: "conversation_source", OperationID: "operation_fork", SourceRevision: "revision",
			SourceFingerprint: "different", State: model.HistoryUseHeld}}
	require.Error(t, validateHistoryUseClaim(selection))
	selection.UseClaim.SourceFingerprint = selection.SourceFingerprint
	require.NoError(t, validateHistoryUseClaim(selection))
	selection.UseClaim.State = model.HistoryUseReleased
	require.Error(t, validateHistoryUseClaim(selection))
}

func TestContinuationReappliesSupervisedApproval(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "tclaude-opencode-continuation-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(root)) })
	executable := filepath.Join(root, "opencode-fake")
	script := "#!/bin/sh\nexec \"$OPENCODE_TEST_BINARY\" -test.run=TestOpenCodeServerHelper -- \"$@\"\n"
	require.NoError(t, os.WriteFile(executable, []byte(script), 0o700))
	provider, err := New(Config{Executable: executable, PrivateRoot: root,
		Environment: []string{"OPENCODE_TEST_BINARY=" + os.Args[0]}})
	require.NoError(t, err)

	automatic := ports.PreparationRequest{Intent: ports.StartFresh, Observations: &observationSink{}, Spec: model.ResolvedExecutionSpec{
		ExecutionID: "execution_automatic", Harness: Name, WorkingDirectory: root,
		Approval: model.ApprovalAutomatic, Sandbox: model.SandboxUnconfined,
	}}
	firstPrepared, err := provider.Prepare(context.Background(), automatic)
	require.NoError(t, err)
	first, err := firstPrepared.Release(context.Background(), &testPermit{execution: automatic.Spec.ExecutionID, operation: "operation_first"})
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	_, err = first.Runtime.Stop(ctx, ports.StopRequest{Force: true})
	cancel()
	require.NoError(t, err)

	prior, err := decodeEvidence(first.Evidence)
	require.NoError(t, err)
	supervised := ports.PreparationRequest{
		Intent: ports.StartContinue, Observations: &observationSink{},
		Spec: model.ResolvedExecutionSpec{ExecutionID: "execution_supervised", Harness: Name,
			WorkingDirectory: root, Approval: model.ApprovalSupervised, Sandbox: model.SandboxUnconfined},
		Continuation:  &model.NativeConversationEvidence{Namespace: NativeNamespace, Reference: "ses_test"},
		PriorEvidence: first.Evidence,
	}
	secondPrepared, err := provider.Prepare(context.Background(), supervised)
	require.NoError(t, err)
	description := secondPrepared.Describe()
	releaseCtx, releaseCancel := context.WithCancel(context.Background())
	releaseCancel()
	second, err := secondPrepared.Release(releaseCtx, &testPermit{execution: supervised.Spec.ExecutionID, operation: "operation_second"})
	require.Error(t, err)
	require.Equal(t, ports.ReleaseUncertain, second.State)
	require.NotNil(t, second.Runtime)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, _ = second.Runtime.Stop(ctx, ports.StopRequest{Force: true})
	})

	var recovered ports.RecoveryResult
	require.Eventually(t, func() bool {
		recovered, err = provider.Recover(context.Background(), ports.RecoveryRequest{
			ExecutionID: supervised.Spec.ExecutionID, Spec: supervised.Spec, Evidence: description.Evidence, Observations: &observationSink{},
		})
		return err == nil && recovered.State == ports.RecoveryControlled
	}, 2*time.Second, 20*time.Millisecond,
		"prepared continuation recovery must enforce policy before returning controlled: %v", err)

	data, err := os.ReadFile(filepath.Join(prior.StateRoot, "data", "permission.json"))
	require.NoError(t, err)
	var rules []permissionRule
	require.NoError(t, json.Unmarshal(data, &rules))
	require.Contains(t, rules, permissionRule{Permission: "bash", Pattern: "*", Action: "ask"})
	require.NotContains(t, rules, permissionRule{Permission: "bash", Pattern: "*", Action: "allow"})
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
	if path := os.Getenv("OPENCODE_TEST_BOOTSTRAP"); path != "" {
		credential, _ := os.ReadFile(os.Getenv("TCLAUDE_BACKEND_CREDENTIAL_FILE"))
		value, _ := json.Marshal(map[string]string{
			"socket": os.Getenv("TCLAUDE_BACKEND_SOCKET"), "credential": string(credential),
			"credential_env": os.Getenv("TCLAUDE_BACKEND_CREDENTIAL"),
		})
		_ = os.WriteFile(path, value, 0o600)
	}
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
		if request.Method == http.MethodGet {
			permissions := readHelperPermission()
			if len(permissions) == 0 {
				_ = json.NewEncoder(writer).Encode([]any{})
				return
			}
			_ = json.NewEncoder(writer).Encode([]any{helperSession(request, "ses_test", permissions)})
			return
		}
		var body struct {
			Permission []permissionRule `json:"permission"`
		}
		_ = json.NewDecoder(request.Body).Decode(&body)
		writeHelperPermission(body.Permission)
		sessionID := "ses_test"
		if os.Getenv("OPENCODE_TEST_ROTATE") != "" {
			sessionID = nextHelperSessionID()
		}
		_ = json.NewEncoder(writer).Encode(helperSession(request, sessionID, body.Permission))
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
		if strings.HasSuffix(request.URL.Path, "/fork") {
			var body struct {
				MessageID string `json:"messageID"`
			}
			_ = json.NewDecoder(request.Body).Decode(&body)
			_ = os.WriteFile(os.Getenv("OPENCODE_TEST_FORK_POINT"), []byte(body.MessageID), 0o600)
			_ = json.NewEncoder(writer).Encode(helperSession(request, "ses_fork", readHelperPermission()))
			return
		}
		if request.Method == http.MethodPatch {
			var body struct {
				Permission []permissionRule `json:"permission"`
			}
			_ = json.NewDecoder(request.Body).Decode(&body)
			writeHelperPermission(body.Permission)
			_ = json.NewEncoder(writer).Encode(helperSession(request, helperSessionIDFromPath(request.URL.Path), body.Permission))
			return
		}
		_ = json.NewEncoder(writer).Encode(helperSession(request, helperSessionIDFromPath(request.URL.Path), readHelperPermission()))
	})
	require.NoError(t, http.Serve(listener, mux))
}

func mustExportSecondText(t *testing.T, path string) string {
	t.Helper()
	value, err := os.ReadFile(path)
	require.NoError(t, err)
	var exported exportedHistory
	require.NoError(t, json.Unmarshal(value, &exported))
	return exported.Messages[1].Parts[0].Text
}

func helperSession(request *http.Request, id string, permission []permissionRule) map[string]any {
	result := map[string]any{"id": id, "directory": request.URL.Query().Get("directory"), "permission": permission}
	if parent := os.Getenv("OPENCODE_TEST_PARENT"); parent != "" {
		result["parentID"] = parent
	}
	return result
}

func helperSessionIDFromPath(path string) string {
	value := strings.TrimPrefix(path, "/session/")
	if index := strings.IndexByte(value, '/'); index >= 0 {
		value = value[:index]
	}
	return value
}

func nextHelperSessionID() string {
	path := filepath.Join(os.Getenv("XDG_DATA_HOME"), "session-count")
	value, _ := os.ReadFile(path)
	count, _ := strconv.Atoi(string(value))
	count++
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	_ = os.WriteFile(path, []byte(strconv.Itoa(count)), 0o600)
	return fmt.Sprintf("ses_%d", count)
}

func helperPermissionPath() string {
	return filepath.Join(os.Getenv("XDG_DATA_HOME"), "permission.json")
}

func writeHelperPermission(rules []permissionRule) {
	_ = os.MkdirAll(filepath.Dir(helperPermissionPath()), 0o700)
	data, _ := json.Marshal(rules)
	_ = os.WriteFile(helperPermissionPath(), data, 0o600)
}

func readHelperPermission() []permissionRule {
	data, _ := os.ReadFile(helperPermissionPath())
	var rules []permissionRule
	_ = json.Unmarshal(data, &rules)
	return rules
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
