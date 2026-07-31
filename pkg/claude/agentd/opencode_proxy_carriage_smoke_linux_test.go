//go:build linux

package agentd

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
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
	"golang.org/x/net/proxy"

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
//
// One thing this arm is NOT, said plainly so a reader does not assume it: the
// host-open launch is not the Unix-relay seam. Host-open builds the spec with
// unixRelay=false, so there is no control relay and the transport is plain TCP.
// It is a real agentd bwrap-confined OpenCode server, and it is the closest
// thing to the refused boundary that can reach a proxy at all — but the seam
// the refusal beside it is about is the relay one, and no launch there can be
// measured until that refusal is lifted.
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
	parsedOrigin, err := netip.ParseAddr(originAddr)
	require.NoError(t, err)
	require.Falsef(t, parsedOrigin.IsLoopback(),
		"%s is loopback, and a loopback origin cannot answer the carriage question: clients skip the proxy for it",
		originAddr)

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
			// Emitted HERE as well as in the summary below, because a case that
			// dies on a require never reaches the summary, and a case that ran
			// should leave its record either way.
			t.Log(openCodeCarriageRecordPrefix + record)
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
	// case under test is defined by what the LAUNCH offers, so every ambient
	// one is REMOVED.
	//
	// Not because a blanked value would shadow an offered one — os/exec dedups
	// Cmd.Env keeping the LAST occurrence, and the launch's entries are
	// appended after the ambient ones, so an offered carriage wins either way.
	// It is the variables a case does NOT offer that make this load-bearing,
	// and the direct control most of all: a surviving ambient HTTPS_PROXY there
	// would route the control through a proxy nobody in this test owns, and the
	// falsifiability the whole arm rests on would be gone.
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
	// The per-agent XDG config directory is bound READ-ONLY for a private-state
	// launch, and OpenCode's Config.loadInstanceState writes a .gitignore into
	// it while creating a session — so without this the server answers HTTP 500
	// with EROFS and nothing is ever measured. The read-only bind's SOURCE is
	// the ambient config directory when one exists, so creating it here with
	// the bootstrap payload production already defines is what puts the file
	// inside the sandbox. Darwin has a launch-time equivalent
	// (prepareOpenCodeReadOnlyConfigForPlatform); on Linux that hook is a
	// no-op, which is a gap worth a ticket rather than something this arm
	// should paper over in production code.
	ambientConfig := filepath.Join(home, "config", "opencode")
	require.NoError(t, os.MkdirAll(ambientConfig, 0o700))
	require.NoError(t, os.WriteFile(
		filepath.Join(ambientConfig, openCodeInstallBootstrapFile),
		[]byte(openCodeInstallGitignore), 0o600))

	db.ResetForTest()
	t.Cleanup(db.ResetForTest)
	agentSocket := filepath.Join(home, ".tclaude", "api", "agentd.sock")
	t.Setenv(agentipc.SocketEnv, agentSocket)
	stopAgentd := startOpenCodeLayerSmokeAgentd(t, tclaudeBinary, agentSocket)
	t.Cleanup(stopAgentd)

	origin := newOpenCodeCarriageOrigin(t, originAddr, originHost)
	auditor := newOpenCodeCarriageAuditor(t, originHost, originAddr, origin.port)
	// PRECONDITION, before anything is measured: prove that a client which DOES
	// carry can reach this origin over BOTH carriages of this exact auditor.
	//
	// Without it, "OpenCode did not carry" and "nothing could have carried" are
	// the same observation. That is the failure this suite exists to refuse:
	// the expected answer here is a negative for at least one carriage, and an
	// expected negative is worthless unless a positive was reachable.
	proveOpenCodeCarriagePath(t, auditor, originHost, origin.port)
	auditor.reset()

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
		// Not carried TO THE ORIGIN'S NAME. Two very different things can look
		// like this, and reporting them as one would put a conclusion in the
		// record that the run did not measure:
		//
		//   - the server never spoke to the proxy at all, which is the real
		//     "does not carry" answer;
		//   - the server DID speak to the proxy but stated the origin
		//     differently — as an IP literal, because it resolved the name
		//     itself rather than leaving resolution to the proxy. That is
		//     cooperation, with a caveat that matters: authored host and domain
		//     rows are never matched against a literal.
		if len(decisions) > 0 {
			assert.Empty(t, auditor.transportErrors())
			return fmt.Sprintf(
				"%s CARRIED UNDER A DIFFERENT IDENTITY: %d proxy decision(s), none stating %s:%d by name (first: %+v -> %s); direct origin requests=%d",
				carriage, len(decisions), originHost, origin.port,
				decisions[0].target, decisions[0].decision.Verdict, direct)
		}
		// This is a REPORTABLE RESULT, not a failure — but it is only
		// meaningful if the request happened at all and went straight to the
		// origin, which is what is asserted here.
		assert.Positivef(t, direct,
			"%s was not carried and the origin saw no direct request either: this case measured nothing",
			carriage)
		// A third state hides here, and reporting it as "made no proxy
		// connection" would be false. The client may have CONNECTED to the
		// proxy and failed before stating a target — a handshake the proxy
		// could not parse, which is what speaking the wrong carriage at it
		// looks like. That leaves a transport error and no decision. It is
		// still "not carried", but "tried and could not" is a different fact
		// about the harness than "never tried", and a reader of this record
		// should not have to guess which one happened.
		if errors := auditor.transportErrors(); len(errors) > 0 {
			return fmt.Sprintf(
				"%s NOT CARRIED (attempted): the server reached the proxy but stated no target, and reached the origin directly (direct requests=%d, transport errors=%v)",
				carriage, direct, errors)
		}
		return fmt.Sprintf(
			"%s NOT CARRIED: the server made no proxy connection at all and reached the origin directly (direct requests=%d)",
			carriage, direct)
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

// proveOpenCodeCarriagePath drives one request per carriage through the auditor
// with a client known to cooperate, and requires each to be ALLOWED at the
// proxy and answered by the origin.
//
// It uses /models.json rather than the completions endpoint deliberately: that
// handler counts nothing, so the precondition cannot inflate the direct-request
// count the measurement reads afterwards. The 418 it returns is the origin
// answering, which is all this needs to establish.
func proveOpenCodeCarriagePath(
	t *testing.T,
	auditor *openCodeCarriageAuditor,
	originHost string,
	originPort int,
) {
	t.Helper()
	target := fmt.Sprintf("http://%s:%d/models.json", originHost, originPort)

	proxyURL, err := url.Parse("http://" + auditor.endpoint)
	require.NoError(t, err)
	httpClient := &http.Client{
		Timeout:   15 * time.Second,
		Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)},
	}
	response, err := httpClient.Get(target)
	require.NoError(t, err,
		"the HTTP carriage of this auditor must reach the origin, or a NOT CARRIED result would be unfalsifiable")
	require.NoError(t, response.Body.Close())
	require.Equal(t, http.StatusTeapot, response.StatusCode)

	// socks5h semantics: the dialer is given the NAME, and x/net/proxy sends it
	// to the proxy rather than resolving it here — which is what the authored
	// host row is evaluated against.
	socksDialer, err := proxy.SOCKS5("tcp", auditor.endpoint, nil, proxy.Direct)
	require.NoError(t, err)
	contextDialer, ok := socksDialer.(proxy.ContextDialer)
	require.True(t, ok)
	socksClient := &http.Client{
		Timeout:   15 * time.Second,
		Transport: &http.Transport{DialContext: contextDialer.DialContext},
	}
	response, err = socksClient.Get(target)
	require.NoError(t, err,
		"the SOCKS5 carriage of this auditor must reach the origin, or a NOT CARRIED result would be unfalsifiable")
	require.NoError(t, response.Body.Close())
	require.Equal(t, http.StatusTeapot, response.StatusCode)

	seen := map[sandboxproxy.Carriage]bool{}
	for _, decision := range auditor.snapshot() {
		require.Equalf(t, sandboxproxy.VerdictAllowed, decision.decision.Verdict,
			"the authored rows must authorize this origin: %+v", decision.target)
		seen[decision.carriage] = true
	}
	require.True(t, seen[sandboxproxy.CarriageHTTP],
		"the HTTP precondition request must have been decided at the proxy")
	require.True(t, seen[sandboxproxy.CarriageSOCKS5],
		"the SOCKS5 precondition request must have been decided at the proxy")
	require.Empty(t, auditor.transportErrors())
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
		// OpenCode 1.18.6's provider-source isolation inputs, modelled on the
		// set the FILTERED launch path applies (openCodeServerEnvironment).
		// NOT identical to it, and the difference is stated rather than implied:
		// OPENCODE_CONFIG_DIR is dropped from a profile environment whenever the
		// launch has private harness state (openCodePrivateEnvironmentName), and
		// the filtered path additionally pins HOME into the state root, which
		// this arm does for itself above. The host-open posture applies none of
		// them on its own, and without them the server reaches for model
		// metadata, project config and updates of its own accord. That traffic is not what is being
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
	errors    []string
}

type openCodeCarriageDecision struct {
	carriage sandboxproxy.Carriage
	target   sandboxproxy.Target
	decision sandboxproxy.Decision
}

func newOpenCodeCarriageAuditor(
	t *testing.T,
	originHost, originAddr string,
	originPort int,
) *openCodeCarriageAuditor {
	t.Helper()
	auditor := &openCodeCarriageAuditor{}
	// Two rows, and the second is not optional. The HOST row is what a carried
	// request is decided on — the identity the client states, the same thing an
	// authored host row is evaluated against under the real floor. The CIDR row
	// is what lets the answer to that name be USED: 198.18.0.0/15 is benchmark
	// space, which the evaluator's private-destination blocker refuses in an
	// allowlist posture unless an explicit cidr row covers it. Without it every
	// carried request would be refused with VerdictPrivateDestination, and the
	// arm could record NOT CARRIED and nothing else — an expected negative that
	// a working positive could never have contradicted.
	rules := sandboxpolicy.NetworkRules{
		Mode: sandboxpolicy.AccessModeList,
		Allow: []sandboxpolicy.NetworkAllowEntry{
			{Host: originHost, Ports: []int{originPort}},
			{CIDR: originAddr + "/32", Ports: []int{originPort}},
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
			// RECORDED, not logged from here. This callback runs on a proxy
			// handler goroutine, and Close does not wait for those: a t.Logf
			// landing after the subtest has finished panics the whole run with
			// "Log in goroutine after Test has completed" — a fabricated
			// failure. The test body drains these instead.
			auditor.mu.Lock()
			defer auditor.mu.Unlock()
			auditor.errors = append(auditor.errors,
				fmt.Sprintf("%s: %v", carriage, err))
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

func (a *openCodeCarriageAuditor) transportErrors() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.errors...)
}

// reset drops everything the precondition below produced, so the measurement
// starts from a proxy and an origin that have seen nothing from this test.
func (a *openCodeCarriageAuditor) reset() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.decisions = nil
	a.errors = nil
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
