//go:build linux

package agentd

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/sandboxproxy"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// WHAT THIS ARM CLAIMS, AND WHAT IT DOES NOT (TCL-889, §6.2).
//
// This is COOPERATION EVIDENCE ABOUT THE HARNESS, NOT FLOOR EVIDENCE, AND IT
// BACKS NO CAPABILITY CELL. It answers one question the OpenCode row of the
// §6.2 matrix left open: when the proxy environment a real proxy floor injects
// is present, does the OpenCode server route its MODEL traffic over it, and
// over which carriage?
//
// It cannot be a floor smoke, and the reason is structural rather than a matter
// of effort: the boundary agentd launches OpenCode through REFUSES to deploy
// the proxy engine outright (TestOpenCodeUnixRelayRefusesTheProxyEngine, and
// the refusals it pins in sandbox_bwrap.go / sandbox_bwrap_linux.go). There is
// therefore no proxy floor to run OpenCode behind at this seam, and no cell for
// any result here to flip. If that refusal is ever lifted, THIS arm is what
// says whether doing so would buy containment or only a harness that ignores
// the proxy and fails closed.
//
// The honest limit of the construction, stated rather than papered over. Under
// the real floor the audit guarantee is structural: the sandbox has an empty
// network namespace, so every connection MUST present itself to the proxy for a
// decision, and "no connection the auditor did not see" is a property of the
// floor rather than something counted. This arm does not get that. It runs the
// real agentd-owned server boundary under the HOST-OPEN posture — the only
// posture at this seam that can reach a proxy at all — and its claim is
// narrower: of the two places this traffic could go, both are watched, and the
// record says which one it went to. The model origin is a fixture reachable
// only through the smoke's own network fixture, and the credentials are
// deliberately invalid, so no packet can reach a real provider either way.
//
// What IS production here, rather than re-derived for the test:
//   - the proxy is the real sandboxproxy server with the real evaluator, the
//     same one the floor runs, and its decision callback is the auditor;
//   - the carriage environment comes from session.ProxyNetworkCarriage, the
//     single definition the launcher's own exec seam injects from, so a harness
//     recorded as cooperating cooperated with the exact assignments a real
//     floor makes;
//   - the launch is the real agentd OpenCode server launch, confined by
//     bubblewrap, driven by a real model request over its real HTTP API.
const (
	openCodeCarriageSmokeEnv = "TCLAUDE_OPENCODE_PROXY_CARRIAGE_SMOKE"
	// The origin must NOT be loopback. Clients commonly refuse to send a
	// loopback destination through a proxy at all (Go's httpproxy.useProxy does
	// exactly this), which would report "OpenCode ignored the carriage" for a
	// destination no cooperating client would have proxied either. The flow
	// provides a fixture address and a name that resolves to it.
	openCodeCarriageOriginAddrEnv = "TCLAUDE_OPENCODE_CARRIAGE_ORIGIN_ADDR"
	openCodeCarriageOriginHostEnv = "TCLAUDE_OPENCODE_CARRIAGE_ORIGIN_HOST"
	// The per-case record the flow greps out of the log. It is the deliverable:
	// the answer to the §6.2 carriage question, per carriage, in one line.
	openCodeCarriageRecordPrefix = "opencode-proxy-carriage: "
	openCodeCarriageModel        = "test/test-model"
	openCodeCarriageSessionID    = "opencode-carriage-smoke"
)

// TestOpenCodeProxyCarriageCooperation runs one real OpenCode server launch per
// carriage and records where the model request went.
//
// Three cases, and the third is what makes the other two mean anything:
//
//	http    — only the HTTP_PROXY/HTTPS_PROXY pair is offered
//	socks5  — only ALL_PROXY (socks5h) is offered
//	direct  — NO carriage is offered at all
//
// The direct case is the falsifiability control in both directions at once: it
// must show the proxy seeing NOTHING and the origin being reached DIRECTLY. If
// it showed no origin traffic either, then a carriage case recording "not
// carried" would prove nothing — the launch might simply never have made a
// model request. If it showed proxy traffic, the environment under test would
// not be the carriage at all.
func TestOpenCodeProxyCarriageCooperation(t *testing.T) {
	if os.Getenv(openCodeCarriageSmokeEnv) != "1" {
		t.Skipf("set %s=1; this arm launches a real OpenCode server and needs the smoke's network fixture",
			openCodeCarriageSmokeEnv)
	}
	tclaudeBinary := strings.TrimSpace(os.Getenv(openCodeLayerSmokeTclaudeEnv))
	require.NotEmpty(t, tclaudeBinary, openCodeLayerSmokeTclaudeEnv)
	tclaudeBinary, err := filepath.Abs(tclaudeBinary)
	require.NoError(t, err)
	_, err = harness.OpenCodeExecutable()
	require.NoError(t, err)
	originAddr := requireOpenCodeCarriageEnv(t, openCodeCarriageOriginAddrEnv)
	originHost := requireOpenCodeCarriageEnv(t, openCodeCarriageOriginHostEnv)
	require.NotEqual(t, "127.0.0.1", originAddr,
		"a loopback origin cannot answer the carriage question: clients skip the proxy for it")

	records := make([]string, 0, 3)
	for _, testCase := range []struct {
		name     string
		carriage sandboxproxy.Carriage
	}{
		{name: "http", carriage: sandboxproxy.CarriageHTTP},
		{name: "socks5", carriage: sandboxproxy.CarriageSOCKS5},
		{name: "direct", carriage: ""},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			record := runOpenCodeProxyCarriageCase(
				t, tclaudeBinary, originAddr, originHost, testCase.carriage)
			records = append(records, record)
		})
	}

	// Emitted at the end as well as per case, so the operator-facing answer is
	// one block in the log rather than three findings scattered through a real
	// server's output.
	for _, record := range records {
		t.Log(openCodeCarriageRecordPrefix + record)
	}
}

// runOpenCodeProxyCarriageCase launches one confined OpenCode server with the
// carriage under test offered to it and returns the record line for it.
func runOpenCodeProxyCarriageCase(
	t *testing.T,
	tclaudeBinary, originAddr, originHost string,
	carriage sandboxproxy.Carriage,
) string {
	t.Helper()
	// Short temp root under /tmp: the OpenCode control path is built beneath it
	// and a long one overruns the Linux sockaddr limit.
	home, err := os.MkdirTemp("/tmp", "occ-*")
	require.NoError(t, err)
	home, err = filepath.EvalSymlinks(home)
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	// Some hosts export proxy routing variables of their own (TCL-867). The
	// case under test is defined by what the LAUNCH offers, so the ambient ones
	// are REMOVED, not blanked: the server environment appends the launch's
	// entries after the ambient ones, and glibc's getenv answers with the FIRST
	// occurrence of a name — a blanked ambient variable would therefore shadow
	// the carriage this case exists to offer, and the arm would record "not
	// carried" for every case.
	for _, entry := range session.ProxyNetworkCarriage("127.0.0.1:1") {
		previous, present := os.LookupEnv(entry.Name)
		if !present {
			continue
		}
		require.NoError(t, os.Unsetenv(entry.Name))
		t.Cleanup(func() { _ = os.Setenv(entry.Name, previous) })
	}
	cwd := filepath.Join(home, "workspace")
	require.NoError(t, os.MkdirAll(cwd, 0o700))

	db.ResetForTest()
	t.Cleanup(db.ResetForTest)
	agentSocket := filepath.Join(home, ".tclaude", "api", "agentd.sock")
	t.Setenv(agentipc.SocketEnv, agentSocket)
	stopAgentd := startOpenCodeLayerSmokeAgentd(t, tclaudeBinary, agentSocket)
	t.Cleanup(stopAgentd)

	origin := newOpenCodeCarriageOrigin(t, originAddr, originHost)
	auditor := newOpenCodeCarriageAuditor(t, originHost, origin.port)

	snapshot := sandboxpolicy.EmptySnapshot()
	// Host-open, not filtered: the filtered postures at this seam either give
	// the sandbox no route to any proxy (isolated) or refuse the proxy engine
	// outright, which is the finding this arm exists beside.
	snapshot.Effective.NetworkAccess = sandboxpolicy.NetworkAccessInternet
	// HOME must name a directory that EXISTS INSIDE the constructed root. The
	// filtered launch path sets it into the harness state root for this reason;
	// the host-open path does not, so an inherited HOME under the runner's temp
	// tree would be absent inside the sandbox and the server fails on its first
	// write with nothing but an opaque 500 to show for it. The workspace is
	// bound read-write by construction, so a directory under it is always there.
	agentHome := filepath.Join(cwd, "agent-home")
	require.NoError(t, os.MkdirAll(agentHome, 0o700))
	snapshot.Effective.Environment = append(
		append([]sandboxpolicy.EnvironmentEntry{{Name: "HOME", Value: agentHome}},
			origin.environment...),
		openCodeCarriageEnvironment(auditor.endpoint, carriage)...)

	agentID := db.NewAgentID()
	allocation, err := allocatePrivateOpenCodeState(agentID)
	require.NoError(t, err)
	// Whatever goes wrong, the server's own log is what says why. The launch
	// helper dumps it on a failed START; a failure at session creation or at the
	// prompt would otherwise leave only an opaque HTTP 500.
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		logOpenCodeLayerSmokeServerLogs(t,
			filepath.Join(allocation.StateRoot, "data", "opencode", "log"))
	})
	spec, err := openCodeTclaudeLayerLaunchSpec(
		string(sandboxpolicy.ImplementationTclaudeLayer), cwd, nil, &snapshot, agentID)
	require.NoError(t, err)
	require.NotNil(t, spec)
	permissionJSON, err := openCodePermissionJSONForLaunch(
		cwd,
		harness.OpenCodeSandboxTclaudeLayer,
		harness.OpenCodeApprovalDeny,
		harness.OpenCodeToolsAllow,
		&snapshot,
	)
	require.NoError(t, err)
	launch, err := startOpenCodeRuntime(
		openCodeCarriageSessionID, cwd, "OpenCode carriage arm", "", permissionJSON,
		string(sandboxpolicy.ImplementationTclaudeLayer), spec)
	require.NoError(t, err)
	t.Cleanup(func() { _ = stopOpenCodeRuntime(openCodeCarriageSessionID) })

	require.NoError(t, sendOpenCodePrompt(
		launch, cwd, "reply with carriage-ok", openCodeCarriageModel, ""))

	// One bounded wait for EITHER outcome, so a cooperating launch is not timed
	// out waiting for direct traffic and an ignoring one is not timed out
	// waiting for a decision. Whichever arrives is the answer.
	require.Eventuallyf(t, func() bool {
		return auditor.decisionCount() > 0 || origin.modelRequests.Load() > 0
	}, 90*time.Second, 100*time.Millisecond,
		"the OpenCode server made no model request over either route, so this case measured nothing")
	// The losing route is given a moment to show up too: asserting "the proxy
	// saw nothing" one millisecond after the origin was hit would race a
	// carriage that is simply slower, and would report "not carried" for a
	// launch that carried.
	time.Sleep(2 * time.Second)

	decisions := auditor.snapshot()
	direct := origin.modelRequests.Load()
	// "Carried" is about the MODEL ORIGIN specifically. A server may also offer
	// the proxy destinations of its own (metadata, updates); those are logged
	// and are not the answer to the carriage question, and the authored allow
	// row refuses them, which is the proxy behaving correctly rather than a
	// defect.
	originDecisions := make([]openCodeCarriageDecision, 0, len(decisions))
	for _, decision := range decisions {
		if decision.target.Kind == sandboxproxy.TargetKindName &&
			decision.target.Name == originHost &&
			decision.target.Port == origin.port {
			originDecisions = append(originDecisions, decision)
			continue
		}
		t.Logf("carriage auditor saw a non-origin destination over %s: %+v -> %s",
			decision.carriage, decision.target, decision.decision.Verdict)
	}
	carried := len(originDecisions) > 0

	if carriage == "" {
		// The control. Both halves are required, and each fails a different
		// way of the arm being worthless.
		assert.Empty(t, originDecisions,
			"a launch offered no proxy environment must not reach the proxy")
		assert.Positivef(t, direct,
			"the origin must be reachable directly, or a carriage case recording no direct traffic proves nothing")
		return fmt.Sprintf(
			"direct control: origin decisions at the proxy=%d, direct origin requests=%d",
			len(originDecisions), direct)
	}

	if !carried {
		// Not carried. This is a REPORTABLE RESULT, not a failure — but it is
		// only meaningful if the request happened at all and went straight to
		// the origin, which is what is asserted here.
		assert.Positivef(t, direct,
			"%s was not carried and the origin saw no direct request either: this case measured nothing",
			carriage)
		return fmt.Sprintf(
			"%s NOT CARRIED: the server ignored the %s carriage and reached the origin directly (direct requests=%d)",
			carriage, carriage, direct)
	}

	// Carried. Assert the discriminating facts rather than "something reached
	// the proxy": the origin was offered over THE CARRIAGE UNDER TEST and no
	// other, and the authored allow row authorized it. The target being a name
	// rather than a literal is already established by the filter above, and it
	// matters — a client that resolved the name itself and offered a literal
	// would not be exercising what authored host rows are evaluated against.
	for _, decision := range originDecisions {
		assert.Equalf(t, carriage, decision.carriage,
			"a launch offered only %s must not reach the proxy over %s",
			carriage, decision.carriage)
		assert.Equal(t, sandboxproxy.VerdictAllowed, decision.decision.Verdict,
			"the authored allow row covers this origin; any other verdict is a fixture defect")
	}
	// Cooperation is only proven if the carried request actually completed:
	// a decision with no answered model request would be a connection the
	// harness opened and abandoned.
	assert.Positivef(t, direct,
		"%s reached the proxy but no model request completed at the origin", carriage)
	return fmt.Sprintf(
		"%s CARRIED: %d proxy decision(s) for %s:%d, all allowed, model request completed",
		carriage, len(originDecisions), originHost, origin.port)
}

// openCodeCarriageEnvironment offers exactly ONE carriage to the launch, taken
// from the launcher's own definition rather than written out here. The
// exemption variables ride along with either carriage because a real floor
// always sets them: without NO_PROXY="" a harness may apply its own default
// exemptions and skip the proxy for reasons that have nothing to do with
// whether it can carry.
//
// An empty carriage means the control case, which is offered nothing at all.
func openCodeCarriageEnvironment(
	endpoint string,
	carriage sandboxproxy.Carriage,
) []sandboxpolicy.EnvironmentEntry {
	if carriage == "" {
		return nil
	}
	var offered []sandboxpolicy.EnvironmentEntry
	for _, entry := range session.ProxyNetworkCarriage(endpoint) {
		if entry.Carriage != carriage && entry.Carriage != "" {
			continue
		}
		offered = append(offered, sandboxpolicy.EnvironmentEntry{
			Name: entry.Name, Value: entry.Value,
		})
	}
	return offered
}

// openCodeCarriageOrigin is the model provider fixture. It records how many
// model requests actually completed at the destination, which is the half of
// the discrimination the proxy cannot report.
type openCodeCarriageOrigin struct {
	port          int
	environment   []sandboxpolicy.EnvironmentEntry
	modelRequests atomic.Int32
}

func newOpenCodeCarriageOrigin(
	t *testing.T,
	addr, host string,
) *openCodeCarriageOrigin {
	t.Helper()
	origin := &openCodeCarriageOrigin{}
	// Bound to the fixture address rather than loopback, so the destination is
	// one a client will actually offer to a proxy.
	listener, err := net.Listen("tcp", net.JoinHostPort(addr, "0"))
	require.NoError(t, err)
	origin.port = listener.Addr().(*net.TCPAddr).Port
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(
		writer http.ResponseWriter, request *http.Request,
	) {
		origin.modelRequests.Add(1)
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(writer,
			"data: {\"id\":\"chatcmpl-carriage\",\"object\":\"chat.completion.chunk\","+
				"\"created\":1,\"model\":\"test-model\",\"choices\":[{\"index\":0,"+
				"\"delta\":{\"role\":\"assistant\",\"content\":\"carriage-ok\"},"+
				"\"finish_reason\":null}]}\n\n")
		_, _ = io.WriteString(writer,
			"data: {\"id\":\"chatcmpl-carriage\",\"object\":\"chat.completion.chunk\","+
				"\"created\":1,\"model\":\"test-model\",\"choices\":[{\"index\":0,"+
				"\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = io.WriteString(writer, "data: [DONE]\n\n")
	})
	mux.HandleFunc("/models.json", func(
		writer http.ResponseWriter, _ *http.Request,
	) {
		http.Error(writer, "model metadata fetch must stay disabled",
			http.StatusTeapot)
	})
	server := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })

	baseURL := fmt.Sprintf("http://%s:%d/v1", host, origin.port)
	config := map[string]any{
		"enabled_providers": []string{"test"},
		"provider": map[string]any{
			"test": map[string]any{
				"npm":       "@ai-sdk/openai-compatible",
				"whitelist": []string{"test-model"},
				"models": map[string]any{
					"test-model": map[string]any{
						"id":   "test-model",
						"name": "Carriage arm model",
						"limit": map[string]int{
							"context": 100_000,
							"output":  10_000,
						},
					},
				},
				"options": map[string]string{
					"baseURL": baseURL,
					// Deliberately invalid, and never anything else: no real
					// credential belongs in a smoke, and the origin is a
					// fixture that authenticates nothing.
					"apiKey": "invalid-smoke-key",
				},
			},
		},
	}
	configJSON, err := json.Marshal(config)
	require.NoError(t, err)
	origin.environment = []sandboxpolicy.EnvironmentEntry{
		{Name: "OPENCODE_CONFIG_CONTENT", Value: string(configJSON)},
		{
			Name:  "OPENCODE_MODELS_URL",
			Value: fmt.Sprintf("http://%s:%d/models.json", host, origin.port),
		},
		// OpenCode 1.18.6's provider-source isolation inputs, the same set the
		// FILTERED launch path applies (openCodeServerEnvironment). The
		// host-open posture this arm must use does not apply them, and without
		// them the server reaches for model metadata, project config and
		// updates of its own accord. That traffic is not what is being
		// measured, and under a carriage it would arrive at the proxy as
		// destinations the authored allow row never covered — noise in the
		// record at best, and internet traffic from a smoke at worst.
		{Name: "OPENCODE_CONFIG", Value: ""},
		{Name: "OPENCODE_CONFIG_DIR", Value: ""},
		{Name: "OPENCODE_DISABLE_PROJECT_CONFIG", Value: "1"},
		{Name: "OPENCODE_PURE", Value: "1"},
		{Name: "OPENCODE_DISABLE_MODELS_FETCH", Value: "1"},
		{Name: "OPENCODE_DISABLE_AUTOUPDATE", Value: "1"},
		{Name: "OPENCODE_AUTH_CONTENT", Value: "{}"},
	}
	return origin
}

// openCodeCarriageAuditor is the real filtering proxy, used here as the
// auditor. It is the production server with the production evaluator: nothing
// about the decision it records is re-derived for this test.
type openCodeCarriageAuditor struct {
	endpoint string

	mu        sync.Mutex
	decisions []openCodeCarriageDecision
}

type openCodeCarriageDecision struct {
	carriage sandboxproxy.Carriage
	target   sandboxproxy.Target
	decision sandboxproxy.Decision
}

func newOpenCodeCarriageAuditor(
	t *testing.T,
	originHost string,
	originPort int,
) *openCodeCarriageAuditor {
	t.Helper()
	auditor := &openCodeCarriageAuditor{}
	// The origin is authorized BY NAME, so a carried request is decided on the
	// identity the client states — the same thing an authored host row is
	// evaluated against under the real floor.
	rules := sandboxpolicy.NetworkRules{
		Mode: sandboxpolicy.AccessModeList,
		Allow: []sandboxpolicy.NetworkAllowEntry{
			{Host: originHost, Ports: []int{originPort}},
		},
	}
	server, err := sandboxproxy.New(sandboxproxy.Config{
		Rules: rules,
		OnDecision: func(
			carriage sandboxproxy.Carriage,
			target sandboxproxy.Target,
			decision sandboxproxy.Decision,
		) {
			auditor.mu.Lock()
			defer auditor.mu.Unlock()
			auditor.decisions = append(auditor.decisions,
				openCodeCarriageDecision{
					carriage: carriage, target: target, decision: decision,
				})
		},
		OnError: func(carriage sandboxproxy.Carriage, err error) {
			// Logged, never asserted on: a client that speaks one carriage at
			// a proxy expecting the other produces one of these, and that is
			// itself part of the answer rather than a defect.
			t.Logf("carriage auditor %s transport error: %v", carriage, err)
		},
	})
	require.NoError(t, err)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	auditor.endpoint = net.JoinHostPort("127.0.0.1",
		strconv.Itoa(listener.Addr().(*net.TCPAddr).Port))
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	return auditor
}

func (a *openCodeCarriageAuditor) snapshot() []openCodeCarriageDecision {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]openCodeCarriageDecision(nil), a.decisions...)
}

func (a *openCodeCarriageAuditor) decisionCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.decisions)
}

// requireOpenCodeCarriageEnv refuses a degraded run rather than skipping the
// part of the arm the missing fixture would have covered.
func requireOpenCodeCarriageEnv(t *testing.T, name string) string {
	t.Helper()
	value := strings.TrimSpace(os.Getenv(name))
	require.NotEmptyf(t, value,
		"%s is required; the carriage arm needs the flow's origin fixture", name)
	return value
}
