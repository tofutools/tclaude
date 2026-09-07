package browser

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/input"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/client"
	"github.com/tofutools/tclaude/internal/backend/host"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
	"github.com/tofutools/tclaude/internal/backend/providers"
	backend "github.com/tofutools/tclaude/internal/backend/server"
)

// Only the external harness is doubled. Browser input, xterm, WebSockets,
// authentication, execution admission, attachment ownership and SQLite are real.
type terminalBrowserProvider struct {
	delivery     host.ActionCredentialHost
	mu           sync.Mutex
	runtimes     map[model.AgentID]*terminalBrowserRuntime
	preparations atomic.Int32
}

func (*terminalBrowserProvider) Name() string { return "terminal-fixture" }
func (*terminalBrowserProvider) Capabilities() ports.ProviderCapabilities {
	return ports.ProviderCapabilities{}
}
func (p *terminalBrowserProvider) ActionCredentials() ports.ActionCredentialDelivery {
	return p.delivery
}
func (p *terminalBrowserProvider) Prepare(ctx context.Context, request ports.PreparationRequest) (ports.PreparedAttempt, error) {
	receipt, err := p.delivery.PrepareActionCredential(ctx, *request.ActionCredential)
	if err != nil {
		return nil, err
	}
	runtime := &terminalBrowserRuntime{id: request.Spec.ExecutionID, resize: request.Spec.AgentID == "alpha", sizes: make(chan ports.TerminalSize, 32)}
	p.mu.Lock()
	p.runtimes[request.Spec.AgentID] = runtime
	p.mu.Unlock()
	p.preparations.Add(1)
	return &terminalBrowserPrepared{accessBrowserPrepared: accessBrowserPrepared{spec: request.Spec, receipt: receipt}, runtime: runtime}, nil
}
func (*terminalBrowserProvider) Recover(context.Context, ports.RecoveryRequest) (ports.RecoveryResult, error) {
	return ports.RecoveryResult{}, fmt.Errorf("unused fixture recovery")
}
func (p *terminalBrowserProvider) runtime(id model.AgentID) *terminalBrowserRuntime {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.runtimes[id]
}

type terminalBrowserPrepared struct {
	accessBrowserPrepared
	runtime *terminalBrowserRuntime
}

func (p *terminalBrowserPrepared) Describe() ports.PreparedDescription {
	d := p.accessBrowserPrepared.Describe()
	d.Evidence.Provider = "terminal-fixture"
	return d
}
func (p *terminalBrowserPrepared) Release(ctx context.Context, permit ports.ReleasePermit) (ports.ReleaseResult, error) {
	if err := permit.Consume(ctx); err != nil {
		return ports.ReleaseResult{}, err
	}
	return ports.ReleaseResult{State: ports.ReleaseStarted, Runtime: p.runtime, Evidence: p.Describe().Evidence}, nil
}

type terminalBrowserRuntime struct {
	ports.Runtime
	id          model.ExecutionID
	resize      bool
	sizes       chan ports.TerminalSize
	attachments atomic.Int32
	active      atomic.Int32
	stops       atomic.Int32
}

func (r *terminalBrowserRuntime) ExecutionID() model.ExecutionID { return r.id }
func (*terminalBrowserRuntime) Observe(context.Context) (ports.Observation, error) {
	return ports.Observation{ObservedAt: time.Now(), Workload: ports.WorkloadRunning, Context: ports.ContextUnknown}, nil
}
func (r *terminalBrowserRuntime) Stop(context.Context, ports.StopRequest) (ports.StopResult, error) {
	r.stops.Add(1)
	return ports.StopResult{}, fmt.Errorf("browser must not stop a workload")
}
func (r *terminalBrowserRuntime) Attach(context.Context, ports.AttachmentRequest) (ports.AttachmentResult, error) {
	front, process := net.Pipe()
	r.attachments.Add(1)
	r.active.Add(1)
	go func() {
		defer process.Close()
		defer r.active.Add(-1)
		reader := bufio.NewReader(process)
		for {
			line, err := reader.ReadString('\r')
			if err != nil {
				return
			}
			if _, err = io.WriteString(process, strings.TrimSuffix(line, "\r")+"\r\n"); err != nil {
				return
			}
		}
	}()
	base := terminalBrowserAttachment{Conn: front}
	var attachment ports.Attachment = base
	if r.resize {
		attachment = terminalBrowserResizable{terminalBrowserAttachment: base, sizes: r.sizes}
	}
	return ports.AttachmentResult{Disposition: ports.EffectAccepted, Attachment: attachment}, nil
}

type terminalBrowserAttachment struct{ net.Conn }

func (terminalBrowserAttachment) Kind() ports.AttachmentKind { return ports.AttachmentTerminal }

type terminalBrowserResizable struct {
	terminalBrowserAttachment
	sizes chan ports.TerminalSize
}

func (a terminalBrowserResizable) Resize(ctx context.Context, size ports.TerminalSize) error {
	select {
	case a.sizes <- size:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestBrowserConcurrentTerminalAttachments(t *testing.T) {
	if os.Getenv("TCLAUDE_BROWSER_SMOKE") != "1" {
		t.Skip("set TCLAUDE_BROWSER_SMOKE=1 for installed-Chrome product acceptance")
	}
	chrome, err := exec.LookPath("google-chrome")
	if err != nil {
		chrome, err = exec.LookPath("chromium")
	}
	require.NoError(t, err)
	root, err := os.MkdirTemp("/tmp", "term-ui-")
	require.NoError(t, err)
	defer os.RemoveAll(root)
	state := filepath.Join(root, "state")
	require.NoError(t, backend.Initialize(state))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	provider := &terminalBrowserProvider{delivery: host.ActionCredentialHost{PrivateRoot: filepath.Join(root, "credentials")}, runtimes: map[model.AgentID]*terminalBrowserRuntime{}}
	backendDone := make(chan error, 1)
	go func() { backendDone <- backend.Serve(ctx, state, providers.NewRegistry(provider)) }()
	require.Eventually(t, func() bool { _, err := os.Stat(filepath.Join(state, "api.sock")); return err == nil }, 5*time.Second, 10*time.Millisecond)
	operator, err := client.New(filepath.Join(state, "api.sock"), filepath.Join(state, "operator.token"))
	require.NoError(t, err)
	defer operator.Close()
	for _, id := range []string{"alpha", "beta"} {
		desired := model.DesiredConfiguration{Harness: provider.Name(), Model: "fixture", WorkingDirectory: root, Approval: model.ApprovalSupervised, Sandbox: model.SandboxUnconfined}
		require.NoError(t, operator.Call(ctx, "POST", "/v2/agents", map[string]any{"id": id, "name": id, "desired": desired}, nil))
		require.NoError(t, operator.Call(ctx, "POST", "/v2/launch", map[string]any{"request_id": "launch-" + id, "target": map[string]any{"agent": map[string]any{"agent_id": id, "expected_revision": 1}}}, nil))
	}
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
	attach := func(name string) {
		page.MustElement("[data-tab=groups]").MustClick()
		page.MustElementR("#roster .row", name).MustElementR("button", "^Attach$").MustClick()
		page.MustElementR("#terminal-status", "Attached · "+name)
	}
	send := func(text string) {
		page.MustElement(".terminal-panel:not([hidden]) .xterm-helper-textarea").MustFocus()
		page.MustInsertText(text)
		page.Keyboard.MustType(input.Enter)
		page.MustElementR(".terminal-panel:not([hidden]) .xterm-rows", text)
	}
	selectTab := func(name string) { page.MustElementR("#terminal-tabs [role=tab]", "^"+name+"$").MustClick() }
	attach("alpha")
	send("alpha-first")
	attach("beta")
	send("beta-only")
	require.Len(t, page.MustElements("#terminal-tabs [role=tab]"), 2)
	require.True(t, page.MustElement("#resize-terminal").MustProperty("disabled").Bool(), "fixed-size runtime must not advertise resize")
	alpha, beta := provider.runtime("alpha"), provider.runtime("beta")
	require.EqualValues(t, 1, alpha.active.Load())
	require.EqualValues(t, 1, beta.active.Load())
	selectTab("alpha")
	page.MustElementR(".terminal-panel:not([hidden]) .xterm-rows", "alpha-first")
	require.NotContains(t, page.MustElement(".terminal-panel:not([hidden]) .xterm-rows").MustText(), "beta-only")
	send("alpha-second")
	page.MustElement("#terminal-size [name=columns]").MustSelectAllText().MustInput("92")
	page.MustElement("#terminal-size [name=rows]").MustSelectAllText().MustInput("30")
	page.MustElement("#resize-terminal").MustClick()
	require.Eventually(t, func() bool {
		select {
		case size := <-alpha.sizes:
			return size.Columns == 92 && size.Rows == 30
		default:
			return false
		}
	}, 3*time.Second, 10*time.Millisecond)
	selectTab("beta")
	send("beta-second")
	require.Equal(t, "80", page.MustElement("#terminal-size [name=columns]").MustProperty("value").Str())
	// Opening an already attached execution selects its tab, not a new socket.
	attach("alpha")
	require.EqualValues(t, 1, alpha.attachments.Load())
	require.Equal(t, "92", page.MustElement("#terminal-size [name=columns]").MustProperty("value").Str())
	page.MustElement("#close-terminal").MustClick()
	require.Eventually(t, func() bool { return alpha.active.Load() == 0 }, 3*time.Second, 10*time.Millisecond)
	require.EqualValues(t, 1, beta.active.Load())
	selectTab("beta")
	send("beta-survives")
	selectTab("alpha")
	page.MustElement("#reconnect-terminal").MustClick()
	page.MustElementR("#terminal-status", "Attached · alpha")
	send("alpha-reconnected")
	require.EqualValues(t, 2, alpha.attachments.Load())
	require.EqualValues(t, 1, beta.attachments.Load())
	// Keyboard navigation and removal keep the other terminal selectable.
	page.MustElementR("#terminal-tabs [role=tab]", "^alpha$").MustFocus()
	page.Keyboard.MustType(input.ArrowRight)
	page.MustElementR("#terminal-status", "Attached · beta")
	page.MustElement("#remove-terminal").MustClick()
	page.MustElementR("#terminal-status", "Attached · alpha")
	require.Eventually(t, func() bool { return beta.active.Load() == 0 }, 3*time.Second, 10*time.Millisecond)
	send("alpha-after-close")
	require.EqualValues(t, 2, provider.preparations.Load())
	require.Zero(t, alpha.stops.Load())
	require.Zero(t, beta.stops.Load())
	require.False(t, page.MustElement("#error").MustVisible())
	if output := os.Getenv("TCLAUDE_PARITY_SCREENSHOT"); output != "" {
		page.MustScreenshot(output)
	}
	page.MustElement("#logout").MustClick()
	require.Eventually(t, func() bool { return alpha.active.Load() == 0 && beta.active.Load() == 0 }, 3*time.Second, 10*time.Millisecond)
	require.Empty(t, page.MustElements("#terminal-tabs [role=tab]"))
}
