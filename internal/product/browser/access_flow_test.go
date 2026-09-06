package browser

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/client"
	"github.com/tofutools/tclaude/internal/backend/host"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
	"github.com/tofutools/tclaude/internal/backend/providers"
	backend "github.com/tofutools/tclaude/internal/backend/server"
)

// The workload is doubled; credentials, execution authentication, access
// admission/decision/grant, native browser controls and SQLite are production.
type accessBrowserProvider struct {
	ports.Provider
	delivery  host.ActionCredentialHost
	delivered chan ports.ActionCredentialReceipt
}

func (*accessBrowserProvider) Name() string { return "access-fixture" }
func (*accessBrowserProvider) Capabilities() ports.ProviderCapabilities {
	return ports.ProviderCapabilities{}
}
func (p *accessBrowserProvider) ActionCredentials() ports.ActionCredentialDelivery { return p.delivery }
func (p *accessBrowserProvider) Prepare(ctx context.Context, request ports.PreparationRequest) (ports.PreparedAttempt, error) {
	receipt, err := p.delivery.PrepareActionCredential(ctx, *request.ActionCredential)
	if err != nil {
		return nil, err
	}
	p.delivered <- receipt
	return &accessBrowserPrepared{spec: request.Spec, receipt: receipt}, nil
}

type accessBrowserPrepared struct {
	spec    model.ResolvedExecutionSpec
	receipt ports.ActionCredentialReceipt
}

func (p *accessBrowserPrepared) Describe() ports.PreparedDescription {
	return ports.PreparedDescription{ExecutionID: p.spec.ExecutionID, Attempt: p.spec.Attempt, Topology: ports.TopologyTerminalAuthoritative, EffectivePolicy: ports.EffectivePolicy{Approval: p.spec.Approval, Sandbox: p.spec.Sandbox, ApprovalEnforced: true, SandboxEnforced: true}, Evidence: model.ProviderEvidence{Provider: "access-fixture", Version: 1, Payload: []byte(`{}`)}, AccessDelivery: &p.receipt}
}
func (*accessBrowserPrepared) Abort(context.Context) error { return nil }
func (p *accessBrowserPrepared) Release(ctx context.Context, permit ports.ReleasePermit) (ports.ReleaseResult, error) {
	if err := permit.Consume(ctx); err != nil {
		return ports.ReleaseResult{}, err
	}
	return ports.ReleaseResult{State: ports.ReleaseStarted, Runtime: &accessBrowserRuntime{id: p.spec.ExecutionID}, Evidence: p.Describe().Evidence}, nil
}

type accessBrowserRuntime struct {
	ports.Runtime
	id model.ExecutionID
}

func (r *accessBrowserRuntime) ExecutionID() model.ExecutionID { return r.id }
func (r *accessBrowserRuntime) Observe(context.Context) (ports.Observation, error) {
	return ports.Observation{ObservedAt: time.Now(), Workload: ports.WorkloadRunning, Context: ports.ContextUnknown}, nil
}

func TestBrowserAccessApprovalAuthorizesExplicitRetry(t *testing.T) {
	if os.Getenv("TCLAUDE_BROWSER_SMOKE") != "1" {
		t.Skip("set TCLAUDE_BROWSER_SMOKE=1 for installed-Chrome product acceptance")
	}
	chrome, err := exec.LookPath("google-chrome")
	if err != nil {
		chrome, err = exec.LookPath("chromium")
	}
	require.NoError(t, err)
	root, err := os.MkdirTemp("", "browser-access-")
	require.NoError(t, err)
	defer os.RemoveAll(root)
	state := filepath.Join(root, "state")
	require.NoError(t, backend.Initialize(state))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	provider := &accessBrowserProvider{delivery: host.ActionCredentialHost{PrivateRoot: filepath.Join(root, "credentials")}, delivered: make(chan ports.ActionCredentialReceipt, 1)}
	backendDone := make(chan error, 1)
	go func() { backendDone <- backend.Serve(ctx, state, providers.NewRegistry(provider)) }()
	socket := filepath.Join(state, "api.sock")
	require.Eventually(t, func() bool { _, err := os.Stat(socket); return err == nil }, 10*time.Second, 10*time.Millisecond)
	operator, err := client.New(socket, filepath.Join(state, "operator.token"))
	require.NoError(t, err)
	defer operator.Close()
	desired := model.DesiredConfiguration{Harness: "access-fixture", Model: "fixture", WorkingDirectory: root, Approval: model.ApprovalSupervised, Sandbox: model.SandboxUnconfined}
	require.NoError(t, operator.Call(ctx, "POST", "/v2/agents", map[string]any{"id": "requester", "name": "Requesting agent", "desired": desired}, nil))
	require.NoError(t, operator.Call(ctx, "POST", "/v2/agents", map[string]any{"id": "recipient", "name": "Recipient", "desired": desired}, nil))
	require.NoError(t, operator.Call(ctx, "POST", "/v2/launch", map[string]any{"request_id": "launch", "target": map[string]any{"agent": map[string]any{"agent_id": "requester", "expected_revision": 1}}}, nil))
	receipt := <-provider.delivered
	actor, err := client.New(socket, receipt.Resource)
	require.NoError(t, err)
	defer actor.Close()
	message := map[string]any{"request_id": "explicit-retry", "recipients": []string{"recipient"}, "body": "Sent after approval"}
	require.Error(t, actor.Call(ctx, "POST", "/v2/messages", message, nil))
	var requested struct {
		Request struct {
			ID string `json:"id"`
		} `json:"request"`
	}
	require.NoError(t, actor.Call(ctx, "POST", "/v2/access-requests", map[string]any{"request_id": "ask", "action": "message.send", "resource": model.ResourceSelector{Kind: model.ResourceAgent, AgentID: "recipient"}, "reason": "Coordinate this task with Recipient", "lifetime_seconds": 120}, &requested))
	view, err := Open(state, "127.0.0.1:0")
	require.NoError(t, err)
	viewDone := make(chan error, 1)
	go func() { viewDone <- view.Serve(ctx) }()
	defer func() { cancel(); require.NoError(t, <-viewDone); require.NoError(t, <-backendDone) }()
	profile := filepath.Join(root, "chrome")
	l := launcher.New().Context(ctx).Bin(chrome).UserDataDir(filepath.Join(profile, "profile")).Headless(true).NoSandbox(true).Leakless(false).Set("disable-gpu")
	env := []string{}
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "XDG_CONFIG_HOME=") && !strings.HasPrefix(entry, "XDG_CACHE_HOME=") && !strings.HasPrefix(entry, "XDG_DATA_HOME=") {
			env = append(env, entry)
		}
	}
	l.Env(append(env, "XDG_CONFIG_HOME="+filepath.Join(profile, "config"), "XDG_CACHE_HOME="+filepath.Join(profile, "cache"), "XDG_DATA_HOME="+filepath.Join(profile, "data"))...)
	defer l.Kill()
	control, err := l.Launch()
	require.NoError(t, err)
	browser := rod.New().Context(ctx).ControlURL(control)
	require.NoError(t, browser.Connect())
	defer browser.Close()
	page := browser.MustPage(view.URL())
	page.MustElement("[data-tab=decisions]").MustClick()
	card := page.MustElement(`[data-access-request="` + requested.Request.ID + `"]`)
	require.Contains(t, card.MustText(), "message.send")
	require.Contains(t, card.MustText(), "recipient")
	card.MustElementR("button", "Decide access").MustClick()
	page.MustElement("[name=answer]").MustSelect("Approve requested access")
	page.MustElement("[name=reason]").MustInput("Exact recipient scope reviewed")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	page.MustElementR(`[data-access-request="`+requested.Request.ID+`"]`, "approved")
	require.NoError(t, actor.Call(ctx, "POST", "/v2/messages", message, nil))
	message["recipients"] = []string{"requester"}
	message["request_id"] = "outside-scope"
	require.Error(t, actor.Call(ctx, "POST", "/v2/messages", message, nil), "approval cannot widen to another recipient")
}
