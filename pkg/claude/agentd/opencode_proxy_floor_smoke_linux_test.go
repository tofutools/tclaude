//go:build linux

package agentd

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/proxy"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/sandboxproxy"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/common"
)

// WHAT THIS ARM CLAIMS (TCL-891, §6.2), AND HOW IT DIFFERS FROM FLOW 30.
//
// THIS IS FLOOR EVIDENCE. It launches the real OpenCode server through the real
// agentd-owned Unix-relay boundary, behind the REAL proxy floor: an empty
// network namespace, both carriages offered by the production launcher, and the
// supervisor's own filtering proxy as the only exit. That is the guarantee
// TCL-889's cooperation arm could not claim and said so at length — it ran
// host-open, because the boundary refused to deploy the proxy engine at all.
// TCL-891 lifted that refusal, so this arm exists.
//
// The difference is not a matter of degree. Under host-open the arm's claim was
// "of the two places this traffic could go, both are watched". Here the claim is
// structural: the namespace has no route anywhere, so every connection the
// server attempts MUST present itself to the proxy for a decision, and "no
// connection the auditor did not see" is a property of the floor rather than
// something counted. A destination the policy refuses has nowhere else to go.
//
// Flow 30 is NOT superseded and is deliberately kept. It offers ONE carriage per
// launch, which is the only construction that can answer "does this harness
// ignore ALL_PROXY" — here both carriages are offered at once, exactly as
// production does, so a harness that carried over HTTP tells you nothing about
// whether it would have used SOCKS.
//
// # What is production here, rather than re-derived for the test
//
//   - the floor is the real one: session.TclaudeLayerFloorPosture's empty
//     namespace, the real proxy bootstrap, the real descriptor handoff;
//   - the proxy is the supervisor's own sandboxproxy server with the real
//     evaluator, and the audit record is its own decision log — turned on the
//     way an operator turns it on, with log_level debug in the tclaude config,
//     not through a seam invented for this test;
//   - the carriage environment is whatever proxyNetworkSandboxEnv injects, not
//     a list written out here;
//   - the launch is the real agentd OpenCode server launch over the Unix-relay
//     control plane, driven by a real model request over its real HTTP API.
//
// # Credential rule (categorical)
//
// The model origin is a fixture reachable only through this smoke's own network
// fixture, and the credentials are deliberately invalid. No packet can reach a
// real provider, and no real credential is ever put in CI.
//
// # What a green run of this arm does NOT establish
//
// It does not activate any capability cell by itself. The activation record and
// the ratings that follow it are made in a separate PR that cites this arm's
// green named run; a smoke and a rating are different artifacts and the rule is
// that the second one names the first.
const (
	openCodeFloorSmokeEnv = "TCLAUDE_OPENCODE_PROXY_FLOOR_SMOKE"
	// The origin must NOT be loopback: clients commonly refuse to send a
	// loopback destination through a proxy at all, which would report "the
	// server ignored the floor" for a destination no cooperating client would
	// have proxied either. The flow provides a fixture address and names that
	// resolve to it.
	openCodeFloorOriginAddrEnv = "TCLAUDE_OPENCODE_FLOOR_ORIGIN_ADDR"
	openCodeFloorOriginHostEnv = "TCLAUDE_OPENCODE_FLOOR_ORIGIN_HOST"
	// Two refusal fixtures, because they exercise DIFFERENT verdicts and a
	// smoke that only had one would leave the other path unobserved:
	// the undeclared name is refused for want of any authorizing row
	// (not_authorized); the denied name has an authorizing row that an explicit
	// deny row beats (denied_by_rule).
	openCodeFloorUndeclaredHostEnv = "TCLAUDE_OPENCODE_FLOOR_UNDECLARED_HOST"
	openCodeFloorDeniedHostEnv     = "TCLAUDE_OPENCODE_FLOOR_DENIED_HOST"
	// The probe's own DECLARED destination, and it is a separate name from the
	// model origin on purpose. If the probe asked about the origin's name, its
	// two allowed decisions would land in the same set the arm reads to answer
	// "did OPENCODE carry its model traffic" — and every downstream conclusion
	// would then be about the probe. Worst of all, the floor-leaked assertion
	// would become unfalsifiable: it fires when a model request completed with
	// no proxy decision naming the origin, and the probe would have guaranteed
	// such a decision exists before OpenCode was even prompted.
	openCodeFloorDeclaredHostEnv = "TCLAUDE_OPENCODE_FLOOR_DECLARED_HOST"

	// The probe's own environment, read inside the floor.
	openCodeFloorProbeEnv       = "TCLAUDE_OPENCODE_FLOOR_PROBE"
	openCodeFloorProbeTargetEnv = "TCLAUDE_OPENCODE_FLOOR_PROBE_TARGETS"
	openCodeFloorProbeMarkerEnv = "TCLAUDE_OPENCODE_FLOOR_PROBE_MARKERS"

	// The per-run record the flow greps out of the log. It is the deliverable:
	// the §6.2 answer for OpenCode behind a real floor, in a few lines.
	openCodeFloorRecordPrefix = "opencode-proxy-floor: "
	// The in-floor probe's own marker line. Distinct from the record prefix
	// above so the flow can count refusals without also matching the summary.
	openCodeFloorMarkerPrefix = "opencode-proxy-floor-probe: "
	openCodeFloorModel        = "test/test-model"
	openCodeFloorSessionID    = "opencode-floor-smoke"
	openCodeFloorMarkerFile   = "floor-probe-markers.txt"
	openCodeFloorProbeBinary  = "floor-probe"
	openCodeFloorWrapperName  = "opencode"
)

// TestOpenCodeProxyFloorCooperation is the floor arm.
func TestOpenCodeProxyFloorCooperation(t *testing.T) {
	if os.Getenv(openCodeFloorSmokeEnv) != "1" {
		t.Skipf("set %s=1; this arm launches a real OpenCode server behind a real proxy floor",
			openCodeFloorSmokeEnv)
	}
	tclaudeBinary := strings.TrimSpace(os.Getenv(openCodeLayerSmokeTclaudeEnv))
	require.NotEmpty(t, tclaudeBinary, openCodeLayerSmokeTclaudeEnv)
	tclaudeBinary, err := filepath.Abs(tclaudeBinary)
	require.NoError(t, err)
	// Resolved BEFORE the workspace is put on PATH, so the wrapper below execs
	// the real pinned binary rather than finding itself.
	realOpenCode, err := harness.OpenCodeExecutable()
	require.NoError(t, err)
	// THE LAUNCHER AND THE SUPERVISOR ARE THE REAL tclaude BINARY, not this
	// test binary. openCodeRelayExecutable defaults to os.Executable, which
	// under `go test` is the compiled agentd test binary — and only tclaude's
	// main understands the positional launch mode this seam execs. A test
	// binary would ignore it, re-run this package's tests with the smoke
	// environment still set, and the launch would die at its authority
	// handshake with nothing to say about the floor. The existing relay
	// executor smoke makes the same override for the same reason.
	previousRelayExecutable := openCodeRelayExecutable
	openCodeRelayExecutable = func() (string, error) { return tclaudeBinary, nil }
	t.Cleanup(func() { openCodeRelayExecutable = previousRelayExecutable })

	originAddr := requireOpenCodeFloorEnv(t, openCodeFloorOriginAddrEnv)
	originHost := requireOpenCodeFloorEnv(t, openCodeFloorOriginHostEnv)
	undeclaredHost := requireOpenCodeFloorEnv(t, openCodeFloorUndeclaredHostEnv)
	deniedHost := requireOpenCodeFloorEnv(t, openCodeFloorDeniedHostEnv)
	declaredHost := requireOpenCodeFloorEnv(t, openCodeFloorDeclaredHostEnv)
	require.NotEqual(t, originHost, declaredHost,
		"the probe's declared destination must not be the model origin's name, or its decisions would be read as OpenCode's")
	parsedOrigin, err := netip.ParseAddr(originAddr)
	require.NoError(t, err)
	require.Falsef(t, parsedOrigin.IsLoopback(),
		"%s is loopback, and a loopback origin cannot answer the carriage question: clients skip the proxy for it",
		originAddr)

	// Short temp root under /tmp: the OpenCode control socket path is built
	// beneath it and a long one overruns the Linux sockaddr limit.
	home, err := os.MkdirTemp("/tmp", "ocf-*")
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
	// floor's own injection is what is under test, so every ambient one is
	// REMOVED — an inherited HTTPS_PROXY reaching the supervisor would put this
	// smoke's traffic behind a proxy nobody here owns.
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
	// TCL-892 workaround, test-owned exactly as #1787's arm made it: the
	// per-agent XDG config directory is bound READ-ONLY for a private-state
	// launch, and OpenCode's Config.loadInstanceState writes a .gitignore into
	// it while creating a session — so without this the server answers HTTP 500
	// with EROFS and nothing is ever measured. Darwin pre-creates the file at
	// launch time (prepareOpenCodeReadOnlyConfigForPlatform); on Linux that
	// hook is a no-op, which is TCL-892 and deliberately not fixed here.
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
	markerPath := filepath.Join(cwd, openCodeFloorMarkerFile)
	installOpenCodeFloorProbeWrapper(t, cwd, realOpenCode, markerPath,
		openCodeFloorProbeTargets{
			Declared:   net.JoinHostPort(declaredHost, strconv.Itoa(origin.port)),
			Undeclared: net.JoinHostPort(undeclaredHost, strconv.Itoa(origin.port)),
			Denied:     net.JoinHostPort(deniedHost, strconv.Itoa(origin.port)),
		})

	snapshot := openCodeFloorSnapshot(
		originAddr, originHost, declaredHost, deniedHost, origin.port)
	snapshot.Effective.Environment = append(
		append([]sandboxpolicy.EnvironmentEntry(nil), origin.environment...),
		sandboxpolicy.EnvironmentEntry{Name: openCodeFloorProbeEnv, Value: "1"})

	agentID := db.NewAgentID()
	allocation, err := allocatePrivateOpenCodeState(agentID)
	require.NoError(t, err)
	// THE AUDIT RECORD IS TURNED ON THE PRODUCTION WAY, IN THE HOME THE
	// SUPERVISOR ACTUALLY RESOLVES. The filtering proxy logs every decision at
	// debug level and a refusal has no other observable trace at all — the
	// client sees a status it may swallow, and the empty namespace produces no
	// packet to capture. An operator turns that record on with this config key,
	// so the smoke does too rather than inventing a seam for a launch
	// production does not perform.
	//
	// WHICH HOME is the part that is easy to get wrong and silently lose the
	// entire audit trail. A filtered launch has openCodeServerEnvironment pin
	// HOME into the harness state root, and the launcher execs the supervisor
	// with that environment — so the supervisor reads its config and writes its
	// log under the state root's filtered home, NOT under this test's HOME.
	supervisorHome := filepath.Join(
		allocation.StateRoot, openCodeFilteredHomeBase)
	writeOpenCodeFloorDebugConfig(t, supervisorHome)
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		logOpenCodeLayerSmokeServerLogs(t,
			filepath.Join(allocation.StateRoot, "data", "opencode", "log"))
		for _, line := range readOpenCodeFloorMarkers(markerPath) {
			t.Log(openCodeFloorMarkerPrefix + line)
		}
	})

	spec, err := openCodeTclaudeLayerLaunchSpec(
		string(sandboxpolicy.ImplementationTclaudeLayer), cwd, nil, &snapshot, agentID)
	require.NoError(t, err)
	require.NotNil(t, spec)
	// The launch under test must be the one this arm claims to measure: the
	// filtered posture, the PROXY engine, and the Unix-relay transport. Asserted
	// from the spec the builder produced rather than from what was authored, so
	// a policy that silently resolved to another engine cannot be reported as
	// floor evidence.
	require.Equal(t, sandboxpolicy.NetworkEngineProxy, spec.Contract.NetworkEngine,
		"this arm is only floor evidence if the launch actually deploys the proxy engine")
	listenerFD, executableFD, err := session.TclaudeLayerUnixRelayServerFDs(*spec)
	require.NoError(t, err,
		"the lifted inherited-descriptor contract must answer for a proxy plan")
	t.Logf("floor launch: proxy engine, inherited relay fds %d/%d",
		listenerFD, executableFD)

	permissionJSON, err := openCodePermissionJSONForLaunch(
		cwd,
		harness.OpenCodeSandboxTclaudeLayer,
		harness.OpenCodeApprovalDeny,
		harness.OpenCodeToolsAllow,
		&snapshot,
	)
	require.NoError(t, err)
	launch, err := startOpenCodeRuntime(
		openCodeFloorSessionID, cwd, "OpenCode floor arm", "", permissionJSON,
		string(sandboxpolicy.ImplementationTclaudeLayer), spec)
	require.NoError(t, err)
	t.Cleanup(func() { _ = stopOpenCodeRuntime(openCodeFloorSessionID) })

	// THE PROBE RAN, and its markers say so per carriage. Required before
	// anything else is read: a wrapper that silently failed to probe would
	// leave a decision record containing only whatever OpenCode did, and the
	// refusal evidence below would be vacuously satisfied by its absence.
	require.Eventuallyf(t, func() bool {
		return len(readOpenCodeFloorMarkers(markerPath)) >=
			len(openCodeFloorExpectedMarkers())+1
	}, 120*time.Second, 250*time.Millisecond,
		"the in-floor probe did not record all of its markers; got:\n%s",
		strings.Join(readOpenCodeFloorMarkers(markerPath), "\n"))
	markers := readOpenCodeFloorMarkers(markerPath)
	// Emitted UNCONDITIONALLY, not only on failure: the flow's checker counts
	// these lines, and evidence that only appears when something went wrong is
	// evidence a green run cannot be read from.
	// Once, not twice. Under `go test -v` a t.Log already reaches the flow's
	// log, and the flow COUNTS these lines — a second copy from fmt.Println
	// would double every count the operator reads.
	for _, marker := range markers {
		t.Log(openCodeFloorMarkerPrefix + marker)
	}
	assert.Contains(t, markers, "wrapper: started",
		"the measurement wrapper must record that it ran at all")
	for _, expected := range openCodeFloorExpectedMarkers() {
		assert.Containsf(t, markers, expected,
			"the in-floor probe did not record %q; markers:\n%s",
			expected, strings.Join(markers, "\n"))
	}

	require.NoError(t, sendOpenCodePrompt(
		launch, cwd, "reply with floor-ok", openCodeFloorModel, ""))

	// One bounded wait for EITHER outcome. Under this floor a model request
	// that went nowhere is still a fact the record must show, so the wait ends
	// on the first evidence of either and the assertions read what happened.
	require.Eventuallyf(t, func() bool {
		return len(openCodeFloorOriginDecisions(
			readOpenCodeFloorDecisions(t, supervisorHome),
			originHost, origin.port)) > 0 ||
			origin.modelRequests.Load() > 0
	}, 120*time.Second, 250*time.Millisecond,
		"the OpenCode server made no model request over any route, so this run measured nothing")
	// The losing route is given a moment too: asserting "the origin was never
	// reached directly" one millisecond after a decision would race a slower
	// path and report a conclusion this run did not measure.
	time.Sleep(3 * time.Second)

	decisions := readOpenCodeFloorDecisions(t, supervisorHome)
	require.NotEmptyf(t, decisions,
		"the proxy recorded no decision at all; either the floor never started or its log is not where this arm reads it (%s)",
		filepath.Join(supervisorHome, ".tclaude", "data", "output.log"))

	records := make([]string, 0, 4)

	// 1. THE STRUCTURAL CLAIM, stated as an assertion rather than as prose.
	//    Every destination in the record is one the authored policy covers, or
	//    it is refused. This reads the WHOLE record rather than a list of names
	//    guessed in advance, so a destination nobody anticipated cannot slip
	//    past by not being asserted about.
	declared := []string{originHost, declaredHost}
	for _, decision := range decisions {
		if decision.Kind == "literal" {
			// A literal target records an Address and no Host, so the name
			// comparison below cannot judge it. It is ASSERTED rather than
			// skipped: the only literal the authored policy covers is the
			// fixture address, through the one /32 cidr row, so an allowed
			// literal naming anything else is exactly the leak this assertion
			// exists to catch.
			if decision.Verdict == "allowed" {
				assert.Equalf(t, originAddr, decision.Address,
					"the floor's proxy allowed a literal destination outside the one authored cidr row: %s",
					decision.Destination())
			}
			continue
		}
		if slices.Contains(declared, decision.Host) {
			continue
		}
		assert.NotEqualf(t, "allowed", decision.Verdict,
			"an undeclared origin %s was allowed through the floor's proxy",
			decision.Destination())
	}

	// 2. REFUSALS OBSERVED EXECUTING, over BOTH carriages, with DISCRIMINATING
	//    verdicts. Not "was not allowed": the exact verdict, because
	//    not_authorized and denied_by_rule are different policy paths and a
	//    smoke that accepted either would not notice one of them breaking.
	for _, carriage := range []string{
		string(sandboxproxy.CarriageHTTP), string(sandboxproxy.CarriageSOCKS5),
	} {
		assert.Truef(t, openCodeFloorDecisionsInclude(
			decisions, carriage, undeclaredHost, origin.port, "not_authorized"),
			"the undeclared probe was not refused-and-recorded over %s; decisions:\n%s",
			carriage, formatOpenCodeFloorDecisions(decisions))
		assert.Truef(t, openCodeFloorDecisionsInclude(
			decisions, carriage, deniedHost, origin.port, "denied_by_rule"),
			"the deny-row probe was not refused-and-recorded over %s; decisions:\n%s",
			carriage, formatOpenCodeFloorDecisions(decisions))
		// Anti-vacuous in the other direction: the same carriage carried a
		// DECLARED destination in the same launch, so a refusal is the policy
		// answering rather than the carriage being broken.
		assert.Truef(t, openCodeFloorDecisionsInclude(
			decisions, carriage, declaredHost, origin.port, "allowed"),
			"the declared destination was not carried over %s, so its refusals prove nothing; decisions:\n%s",
			carriage, formatOpenCodeFloorDecisions(decisions))
	}
	records = append(records, fmt.Sprintf(
		"undeclared and deny-row destinations refused over both carriages, declared destination carried over both (%d decisions total)",
		len(decisions)))

	// 3. THE §6.2 CARRIAGE ANSWER for OpenCode's OWN model traffic, kept in the
	//    three states TCL-889's review established. They are different facts
	//    about the harness and collapsing them would put a conclusion in the
	//    record that the run did not measure.
	originDecisions := openCodeFloorOriginDecisions(
		decisions, originHost, origin.port)
	// Nothing but OpenCode's own traffic can be in this set: the probe asks
	// about a different declared name, so every decision here names the model
	// origin because the SERVER named it.
	direct := origin.modelRequests.Load()
	carriages := map[string]bool{}
	for _, decision := range originDecisions {
		carriages[decision.Carriage] = true
	}
	switch {
	case direct > 0 && len(originDecisions) > 0:
		records = append(records, fmt.Sprintf(
			"CARRIED: %d model request(s) completed at the origin, reached through the proxy over %v",
			direct, sortedOpenCodeFloorCarriages(carriages)))
	case len(originDecisions) > 0:
		records = append(records, fmt.Sprintf(
			"CARRIED BUT NOT COMPLETED: the origin was offered to the proxy over %v and allowed, but no model request completed",
			sortedOpenCodeFloorCarriages(carriages)))
	case len(openCodeFloorTransportErrors(t, supervisorHome)) > 0:
		records = append(records, fmt.Sprintf(
			"NOT CARRIED (attempted): the server reached the proxy but stated no target; transport errors=%v",
			openCodeFloorTransportErrors(t, supervisorHome)))
	default:
		records = append(records,
			"NOT CARRIED (never tried): the server made no proxy connection naming the model origin")
	}

	// 4. THE FLOOR'S OWN GUARANTEE, which is what makes this arm floor evidence
	//    rather than cooperation evidence: a model request that COMPLETED at
	//    the origin must have gone through the proxy, because the namespace has
	//    no other route. A completion with no matching decision would mean the
	//    floor leaked, and is the one outcome here that is a hard failure
	//    rather than a recorded result.
	if direct > 0 {
		assert.NotEmptyf(t, originDecisions,
			"a model request completed at the origin with no proxy decision naming it: the empty-namespace floor leaked; decisions:\n%s",
			formatOpenCodeFloorDecisions(decisions))
	}

	for _, record := range records {
		t.Log(openCodeFloorRecordPrefix + record)
	}
}

// openCodeFloorSnapshot authors the policy this arm measures against.
//
// Four rows and each is load-bearing:
//
//   - the origin HOST row is what a carried request is decided on — the
//     identity the client states, which is what an authored host row is
//     evaluated against under a real floor;
//   - the CIDR row is what lets the ANSWER to that name be used. The fixture
//     lives in benchmark space (198.18.0.0/15), which the evaluator's
//     private-destination blocker refuses in an allowlist posture unless an
//     explicit cidr row covers it. Without it every carried request would be
//     refused as private_destination and the arm could record nothing else;
//   - the probe's declared host is authored allowed and is a DIFFERENT name
//     from the origin, so the probe's own carried requests never enter the set
//     the arm reads to answer what OpenCode did;
//   - the denied host is authored ALLOWED and then DENIED, so the deny row has
//     an overlapping allow to beat. A deny row with nothing to beat produces
//     not_authorized, which is the other test's verdict, not this one's;
//   - the undeclared name is deliberately absent from all of them.
func openCodeFloorSnapshot(
	originAddr, originHost, declaredHost, deniedHost string,
	originPort int,
) sandboxpolicy.Snapshot {
	snapshot := sandboxpolicy.EmptySnapshot()
	snapshot.Effective.Network = &sandboxpolicy.NetworkRules{
		Mode:   sandboxpolicy.AccessModeList,
		Engine: sandboxpolicy.NetworkEngineProxy,
		Allow: []sandboxpolicy.NetworkAllowEntry{
			{Host: originHost, Ports: []int{originPort}},
			{CIDR: originAddr + "/32", Ports: []int{originPort}},
			{Host: declaredHost, Ports: []int{originPort}},
			{Host: deniedHost, Ports: []int{originPort}},
		},
		Deny: []sandboxpolicy.NetworkAllowEntry{
			{Host: deniedHost, Ports: []int{originPort}},
		},
	}
	return snapshot
}

// openCodeFloorProbeTargets is what the in-floor probe is asked about.
type openCodeFloorProbeTargets struct {
	Declared   string
	Undeclared string
	Denied     string
}

// installOpenCodeFloorProbeWrapper puts TEST-OWNED MEASUREMENT SCAFFOLDING on
// PATH under the name the production launch resolves.
//
// IT IS NOT A PRODUCTION SHIM AND MUST NEVER BECOME ONE. It exists for one
// reason: this seam execs a binary rather than a shell line, so there is no
// other place to run a cooperating-client probe INSIDE the floor. The wrapper
// starts that probe and then EXECS the real pinned OpenCode binary, so the
// server everything downstream measures is the real one, in the same network
// namespace, with the same proxy environment.
//
// The probe runs in the BACKGROUND, and that is deliberate rather than lazy:
// agentd bounds OpenCode's startup, and a synchronous probe would spend that
// budget and turn a slow runner into a launch failure that says nothing about
// the floor. What stops a backgrounded probe from failing silently is the
// marker file — the wrapper records that it ran before it forks, the probe
// records each carriage's outcome, and the arm requires every one of them.
func installOpenCodeFloorProbeWrapper(
	t *testing.T,
	workspace, realOpenCode, markerPath string,
	targets openCodeFloorProbeTargets,
) {
	t.Helper()
	probePath := filepath.Join(workspace, openCodeFloorProbeBinary)
	copyOpenCodeFloorTestBinary(t, os.Args[0], probePath)
	encoded, err := json.Marshal(targets)
	require.NoError(t, err)

	wrapperPath := filepath.Join(workspace, openCodeFloorWrapperName)
	script := "#!/bin/sh\n" +
		"# TEST-OWNED MEASUREMENT SCAFFOLDING (TCL-891 floor arm), not a\n" +
		"# production shim. It records that it ran, starts one in-floor probe of\n" +
		"# the filtering proxy over both carriages, and then EXECS the real\n" +
		"# pinned OpenCode binary — so the server measured afterwards is the real\n" +
		"# one, in this same namespace, with this same proxy environment.\n" +
		"printf 'wrapper: started\\n' >>" + clcommon.ShellQuoteArg(markerPath) + "\n" +
		clcommon.ShellQuoteArg(probePath) +
		" -test.run '^TestOpenCodeProxyFloorProbeHelper$' -test.v >/dev/null 2>&1 &\n" +
		"exec " + clcommon.ShellQuoteArg(realOpenCode) + " \"$@\"\n"
	require.NoError(t, os.WriteFile(wrapperPath, []byte(script), 0o700))

	// PATH is prepended AFTER the real binary was resolved, so the wrapper
	// cannot find itself.
	t.Setenv("PATH", workspace+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(openCodeFloorProbeTargetEnv, string(encoded))
	t.Setenv(openCodeFloorProbeMarkerEnv, markerPath)
	resolved, err := harness.OpenCodeExecutable()
	require.NoError(t, err)
	require.Equal(t, wrapperPath, resolved,
		"the launch must resolve the measurement wrapper, or the probe never runs inside the floor")
}

// TestOpenCodeProxyFloorProbeHelper RUNS INSIDE THE FLOOR, started by the
// wrapper above and sharing the OpenCode server's network namespace and proxy
// environment.
//
// It asks one declared and two refused questions over EACH carriage, so the
// launch's decision record is guaranteed to contain executed refusals on both
// carriages with both refusal verdicts — rather than only whatever OpenCode
// itself happened to do, which TCL-889 measured to be HTTP and nothing else.
//
// The proxy endpoints are read from the environment THE FLOOR INJECTED, not
// from a value passed in: a probe given the endpoint directly would prove the
// proxy works while saying nothing about whether the launcher's carriage
// injection reached the process.
func TestOpenCodeProxyFloorProbeHelper(t *testing.T) {
	if os.Getenv(openCodeFloorProbeEnv) != "1" {
		t.Skip("OpenCode proxy floor probe helper")
	}
	markerPath := strings.TrimSpace(os.Getenv(openCodeFloorProbeMarkerEnv))
	require.NotEmpty(t, markerPath)
	var targets openCodeFloorProbeTargets
	require.NoError(t, json.Unmarshal(
		[]byte(os.Getenv(openCodeFloorProbeTargetEnv)), &targets))

	httpProxy := strings.TrimSpace(os.Getenv("HTTPS_PROXY"))
	require.NotEmptyf(t, httpProxy,
		"the floor must have injected HTTPS_PROXY; without it this probe measures nothing")
	allProxy := strings.TrimSpace(os.Getenv("ALL_PROXY"))
	require.NotEmpty(t, allProxy, "the floor must have injected ALL_PROXY")
	socksEndpoint, err := url.Parse(allProxy)
	require.NoError(t, err)

	record := func(name, carriage, outcome string) {
		file, err := os.OpenFile(markerPath,
			os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		require.NoError(t, err)
		_, err = fmt.Fprintf(file, "%s\n",
			openCodeFloorMarker(name, carriage, outcome))
		require.NoError(t, err)
		require.NoError(t, file.Close())
	}

	proxyURL, err := url.Parse(httpProxy)
	require.NoError(t, err)
	httpClient := &http.Client{
		Timeout:   20 * time.Second,
		Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)},
	}
	socksDialer, err := proxy.SOCKS5("tcp", socksEndpoint.Host, nil, proxy.Direct)
	require.NoError(t, err)
	contextDialer, ok := socksDialer.(proxy.ContextDialer)
	require.True(t, ok)
	socksClient := &http.Client{
		Timeout:   20 * time.Second,
		Transport: &http.Transport{DialContext: contextDialer.DialContext},
	}

	for _, probe := range []struct {
		name     string
		target   string
		declared bool
	}{
		{name: "declared", target: targets.Declared, declared: true},
		{name: "undeclared", target: targets.Undeclared},
		{name: "denied", target: targets.Denied},
	} {
		// The origin answers /models.json with 418 and counts nothing, so the
		// probe cannot inflate the model-request count the arm reads.
		target := "http://" + probe.target + "/models.json"

		// HTTP carriage. A refused request is answered by the proxy with 403
		// rather than hung up on, so both outcomes come back as a status.
		outcome := "unreachable"
		if response, err := httpClient.Get(target); err == nil {
			_ = response.Body.Close()
			switch {
			case probe.declared && response.StatusCode == http.StatusTeapot:
				outcome = "carried"
			case !probe.declared && response.StatusCode == http.StatusForbidden:
				outcome = "refused"
			default:
				outcome = "unexpected-status-" + strconv.Itoa(response.StatusCode)
			}
		}
		record(probe.name, string(sandboxproxy.CarriageHTTP), outcome)

		// SOCKS5 carriage. A refusal is a dial error here rather than a status,
		// which is the protocol's own shape and not a weaker observation: the
		// proxy's decision log carries the verdict either way, and the arm
		// asserts on that.
		outcome = "unreachable"
		if response, err := socksClient.Get(target); err == nil {
			_ = response.Body.Close()
			if probe.declared && response.StatusCode == http.StatusTeapot {
				outcome = "carried"
			} else {
				outcome = "unexpected-status-" + strconv.Itoa(response.StatusCode)
			}
		} else if !probe.declared {
			outcome = "refused"
		}
		record(probe.name, string(sandboxproxy.CarriageSOCKS5), outcome)
	}
}

func openCodeFloorMarker(name, carriage, outcome string) string {
	return fmt.Sprintf("probe: %s/%s: %s", name, carriage, outcome)
}

// openCodeFloorExpectedMarkers is the complete set the arm requires. Written
// out rather than derived from the probe's loop so that a probe which stopped
// asking one of these questions is a failure here rather than a smaller run
// that still passes.
func openCodeFloorExpectedMarkers() []string {
	http := string(sandboxproxy.CarriageHTTP)
	socks := string(sandboxproxy.CarriageSOCKS5)
	return []string{
		openCodeFloorMarker("declared", http, "carried"),
		openCodeFloorMarker("declared", socks, "carried"),
		openCodeFloorMarker("undeclared", http, "refused"),
		openCodeFloorMarker("undeclared", socks, "refused"),
		openCodeFloorMarker("denied", http, "refused"),
		openCodeFloorMarker("denied", socks, "refused"),
	}
}

func readOpenCodeFloorMarkers(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	lines := []string{}
	for _, line := range strings.Split(string(data), "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			lines = append(lines, trimmed)
		}
	}
	return lines
}

// writeOpenCodeFloorDebugConfig turns on the proxy's decision record through
// the config key an operator would use.
func writeOpenCodeFloorDebugConfig(t *testing.T, home string) {
	t.Helper()
	dir := filepath.Join(home, ".tclaude", "data")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.json"),
		[]byte(`{"log_level":"debug"}`), 0o600))
}

// openCodeFloorDecision is one line of the filtering proxy's own account of
// this launch.
//
// The reader is deliberately LOCAL to this package rather than shared with the
// session-package smokes that read the same records. Those helpers live in
// _test files, which cannot be imported, and hoisting them into a real package
// would churn four evidence-bearing smokes in a PR that already changes a
// launch capability. The duplication is fail-safe in the direction that
// matters: a reader that drifted would find nothing and every assertion below
// would fail, never pass.
type openCodeFloorDecision struct {
	Message  string `json:"msg"`
	Carriage string `json:"carriage"`
	Kind     string `json:"target_kind"`
	Host     string `json:"host"`
	Address  string `json:"address"`
	Port     int    `json:"port"`
	Verdict  string `json:"verdict"`
}

func (d openCodeFloorDecision) Destination() string {
	host := d.Host
	if host == "" {
		host = d.Address
	}
	return net.JoinHostPort(host, strconv.Itoa(d.Port))
}

// readOpenCodeFloorDecisions returns the filtering proxy's record for this
// launch. This is where a REFUSAL becomes observable at all.
func readOpenCodeFloorDecisions(t *testing.T, home string) []openCodeFloorDecision {
	t.Helper()
	records := []openCodeFloorDecision{}
	for _, line := range readOpenCodeFloorLogLines(
		home, session.ProxyNetworkDecisionMessage) {
		var record openCodeFloorDecision
		// A malformed line is a broken contract, not noise to skip: the
		// assertions read this record and would silently weaken.
		require.NoErrorf(t, json.Unmarshal([]byte(line), &record),
			"unparseable proxy decision record: %s", line)
		records = append(records, record)
	}
	return records
}

func openCodeFloorTransportErrors(t *testing.T, home string) []string {
	t.Helper()
	return readOpenCodeFloorLogLines(home, session.ProxyNetworkErrorMessage)
}

// readOpenCodeFloorLogLines reads the supervisor's log.
//
// The home is passed in rather than taken from the accessor, and that is the
// whole correctness of this reader: common.OutputLogPath() resolves against THIS
// PROCESS's home, while the supervisor runs with HOME pinned into the harness
// state root. It is kept as a last fallback only so a future launch that does
// not repin HOME still reads somewhere sensible. The ~/.tclaude/output.log
// spelling is read too because the log has moved once already.
func readOpenCodeFloorLogLines(home, message string) []string {
	candidates := []string{
		filepath.Join(home, ".tclaude", "data", "output.log"),
		filepath.Join(home, ".tclaude", "output.log"),
		common.OutputLogPath(),
	}
	lines := []string{}
	seen := map[string]struct{}{}
	for _, path := range candidates {
		if path == "" {
			continue
		}
		if _, already := seen[path]; already {
			continue
		}
		seen[path] = struct{}{}
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			if strings.Contains(line, message) {
				lines = append(lines, line)
			}
		}
		if len(lines) > 0 {
			return lines
		}
	}
	return lines
}

func openCodeFloorDecisionsInclude(
	decisions []openCodeFloorDecision,
	carriage, host string,
	port int,
	verdict string,
) bool {
	for _, decision := range decisions {
		if decision.Carriage == carriage && decision.Host == host &&
			decision.Port == port && decision.Verdict == verdict {
			return true
		}
	}
	return false
}

func openCodeFloorOriginDecisions(
	decisions []openCodeFloorDecision,
	originHost string,
	originPort int,
) []openCodeFloorDecision {
	out := []openCodeFloorDecision{}
	for _, decision := range decisions {
		if decision.Kind == "name" && decision.Host == originHost &&
			decision.Port == originPort && decision.Verdict == "allowed" {
			out = append(out, decision)
		}
	}
	return out
}

func sortedOpenCodeFloorCarriages(seen map[string]bool) []string {
	out := make([]string, 0, len(seen))
	for carriage := range seen {
		out = append(out, carriage)
	}
	slices.Sort(out)
	return out
}

func formatOpenCodeFloorDecisions(decisions []openCodeFloorDecision) string {
	var builder strings.Builder
	for _, decision := range decisions {
		fmt.Fprintf(&builder, "  %s %s -> %s\n",
			decision.Carriage, decision.Destination(), decision.Verdict)
	}
	return builder.String()
}

// copyOpenCodeFloorTestBinary copies this test binary into the workspace so it
// can run inside the floor. The workspace is bound read-write by the launch, so
// it is the one place an in-sandbox helper can come from.
func copyOpenCodeFloorTestBinary(t *testing.T, source, destination string) {
	t.Helper()
	source, err := filepath.Abs(source)
	require.NoError(t, err)
	data, err := os.ReadFile(source)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(destination, data, 0o700))
	// Proven executable here rather than discovered inside the sandbox, where
	// the failure would surface as a launch that simply never probed.
	require.NoError(t, exec.Command(destination, "-test.run", "^$").Run())
}

// requireOpenCodeFloorEnv refuses a degraded run rather than skipping the part
// of the arm the missing fixture would have covered.
func requireOpenCodeFloorEnv(t *testing.T, name string) string {
	t.Helper()
	value := strings.TrimSpace(os.Getenv(name))
	require.NotEmptyf(t, value,
		"%s is required; the floor arm needs the flow's network fixture", name)
	return value
}
