//go:build linux

package session

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/sandboxproxy"
)

// The M2.6 END-TO-END posture smokes: authored policy in, real launch, real
// enforcement observed, for the four postures the §1.3 deployment table
// distinguishes.
//
// The per-milestone smokes each prove their own piece — the floor, the policy
// engine, harness cooperation, tool egress. What none of them proves is that
// the ASSEMBLED posture behaves as authored, and in particular that the three
// postures which must deploy NO proxy do not deploy one. That is what this file
// is for, and it is why it runs in its own CI shard
// (scripts/proxy-posture-e2e/).
//
// # What every scenario asserts, from one predicate
//
// Whether a launch deploys a filtering proxy is asked ONCE, of
// TclaudeLayerDeploysProxy over the engine DeployedNetworkEngineForRules
// resolved from the authored policy. Every other claim in this file is compared
// against that answer rather than against a per-test expectation:
//
//   - the launcher's own supervisor flag (in runProxyEngineLaunch);
//   - the presence of a proxy PROCESS on the host, watched while the sandbox
//     runs;
//   - the presence of a proxy LISTENER in the sandbox's own network namespace;
//   - the proxy discovery variables the launcher injects;
//   - the proxy's decision record;
//   - and the MECHANISM the preview surface predicts.
//
// The last one is the disclosure-honesty check at system level: preview and
// launch are asserted to agree in the SAME run, so a prediction naming a
// mechanism the launch does not run fails here rather than in front of an
// operator.
//
// # Runner fixture contract
//
// The shard supplies the same live fixture shape the other proxy smokes use,
// plus one RFC1918 address, through these variables:
//
//	TCLAUDE_FILTERED_ALLOWED_ADDR       a reserved-space address with listeners
//	TCLAUDE_FILTERED_ADJACENT_ADDR      a second one OUTSIDE the allowed prefix
//	TCLAUDE_FILTERED_ALLOWED_PREFIX     a CIDR covering ALLOWED_ADDR only
//	TCLAUDE_FILTERED_ALLOWED_PORT       a port those addresses accept
//	TCLAUDE_FILTERED_DENIED_PORT        a second live port the policy narrows away
//	TCLAUDE_POSTURE_E2E_PRIVATE_ADDR    an RFC1918 address with the same listeners
//
// and maps these names in the HOST's /etc/hosts, because the proxy resolves
// names host-side:
//
//	allowed.posture.tclaude.test   -> ALLOWED_ADDR
//	denied.posture.tclaude.test    -> ALLOWED_ADDR
//	private.posture.tclaude.test   -> PRIVATE_ADDR
//
// The denied name resolves to the SAME address as the allowed one on purpose:
// what separates them is authored identity, not reachability, so its refusal is
// the policy answering rather than a side effect of pointing nowhere.
const (
	postureE2EEnv       = "TCLAUDE_PROXY_POSTURE_E2E"
	postureE2EHelperEnv = "TCLAUDE_PROXY_POSTURE_E2E_HELPER"

	postureE2EPrivateAddrEnv = "TCLAUDE_POSTURE_E2E_PRIVATE_ADDR"
	postureE2EHostAllowedEnv = "TCLAUDE_POSTURE_E2E_HOST_ALLOWED_PORT"
	postureE2EHostDeniedEnv  = "TCLAUDE_POSTURE_E2E_HOST_DENIED_PORT"

	postureE2EAllowedHost = "allowed.posture.tclaude.test"
	postureE2EDeniedHost  = "denied.posture.tclaude.test"
	postureE2EPrivateHost = "private.posture.tclaude.test"

	postureE2EDialTimeout = 3 * time.Second
)

// postureE2EFixture is the runner-supplied fixture plus the halves the
// launching test owns.
type postureE2EFixture struct {
	proxySmokeFixture
	// PrivateAddr is RFC1918 rather than the benchmarking space the other
	// addresses live in. The private-destination blocker treats both alike, but
	// §4.4 is a ruling about PRIVATE space, and a scenario that only ever
	// exercised 198.18/15 would be evidence about a range an operator does not
	// think of that way.
	PrivateAddr string
}

func postureE2ERunnerFixture(t *testing.T) postureE2EFixture {
	t.Helper()
	return postureE2EFixture{
		proxySmokeFixture: proxySmokeRunnerFixture(t),
		PrivateAddr:       requireFilteredSmokeEnv(t, postureE2EPrivateAddrEnv),
	}
}

// postureE2EFixtureFromEnv is what the in-sandbox helper reads: every value,
// including the host-loopback ports the launching test chose.
func postureE2EFixtureFromEnv(t *testing.T) postureE2EFixture {
	t.Helper()
	fixture := postureE2ERunnerFixture(t)
	fixture.HostAllowed = requireFilteredSmokePort(t, postureE2EHostAllowedEnv)
	fixture.HostDenied = requireFilteredSmokePort(t, postureE2EHostDeniedEnv)
	return fixture
}

// ---------------------------------------------------------------------------
// Scenario 1: a discriminating allowlist. A proxy deploys and enforces.
// ---------------------------------------------------------------------------

// postureE2EDiscriminatingRules is the authored policy scenario 1 launches
// under: a list with a name, a CIDR and a deny row overlapping an allow.
func postureE2EDiscriminatingRules(
	fixture postureE2EFixture,
) sandboxpolicy.NetworkRules {
	return sandboxpolicy.NetworkRules{
		Mode:   sandboxpolicy.AccessModeList,
		Engine: sandboxpolicy.NetworkEngineProxy,
		Allow: []sandboxpolicy.NetworkAllowEntry{
			{Host: postureE2EAllowedHost, Ports: []int{fixture.AllowedPort}},
			// Authored so the deny row below has an overlapping allow to beat:
			// a deny that wins over nothing proves only that nothing was
			// allowed.
			{Host: postureE2EDeniedHost, Ports: []int{fixture.AllowedPort}},
			// Clears the private-destination blocker for the names that resolve
			// into the fixture's reserved space, and is the literal-target rule.
			{CIDR: fixture.AllowedPrefix, Ports: []int{fixture.AllowedPort}},
		},
		Deny: []sandboxpolicy.NetworkAllowEntry{
			{Host: postureE2EDeniedHost},
		},
	}
}

// TestProxyPostureE2EDiscriminatingAllowlist is the assembled posture at its
// strongest: an authored allowlist reaches a real launch, the proxy carries the
// authored destination over BOTH carriages, refuses the denied one with the
// discriminating verdict, and every route that does not go through the proxy is
// gone.
func TestProxyPostureE2EDiscriminatingAllowlist(t *testing.T) {
	postureE2ERequireShard(t)
	fixture, launch := runProxyPostureE2ELaunch(t, postureE2EScenario{
		Key:   "discriminating",
		Rules: postureE2EDiscriminatingRules,
		Markers: []string{
			"posture-e2e: discriminating/authored destination over http: carried",
			"posture-e2e: discriminating/authored destination over socks5: carried",
			"posture-e2e: discriminating/denied destination over http: refused",
			"posture-e2e: discriminating/denied destination over socks5: refused",
			"posture-e2e: discriminating/direct TCP outside the proxy: refused",
			"posture-e2e: discriminating/UDP outside the proxy: refused",
			"posture-e2e: discriminating/ICMP outside the proxy: refused",
			"posture-e2e: discriminating/the proxy listener is the only one: verified",
		},
	})

	// The decision record is where a refusal becomes an OBSERVATION rather than
	// an inference, and the verdict is compared exactly: "not allowed" would be
	// satisfied by a refusal for the wrong reason — an unreachable fixture, a
	// policy that authorized nothing — which is the vacuous shape this suite
	// exists to prevent.
	decisions := requireProxyDecisions(t, launch)
	assert.Truef(t,
		postureE2EHasVerdict(decisions, postureE2EAllowedHost,
			fixture.AllowedPort, sandboxproxy.VerdictAllowed),
		"the authored destination must be observed CARRIED at the proxy; records:\n%s",
		strings.Join(launch.Decisions, "\n"))
	assert.Truef(t,
		postureE2EHasVerdict(decisions, postureE2EDeniedHost,
			fixture.AllowedPort, sandboxproxy.VerdictDeniedByRule),
		"the denied destination must be observed refused BY THE DENY ROW; records:\n%s",
		strings.Join(launch.Decisions, "\n"))
}

// ---------------------------------------------------------------------------
// Scenario 2: an open baseline with a deny row. A proxy deploys, and private
// space stays reachable per the §4.4 amended ruling.
// ---------------------------------------------------------------------------

func postureE2EOpenDenyRules(postureE2EFixture) sandboxpolicy.NetworkRules {
	return sandboxpolicy.NetworkRules{
		Mode:   sandboxpolicy.AccessModeOpen,
		Engine: sandboxpolicy.NetworkEngineProxy,
		Deny: []sandboxpolicy.NetworkAllowEntry{
			{Host: postureE2EDeniedHost},
		},
	}
}

// TestProxyPostureE2EOpenBaselineWithDeny executes the amended §4.4 ruling
// end to end: open means open minus the authored denies, so the deny is
// enforced while private space — a literal and a name that resolves there —
// stays reachable, matching what the packet engine does for the same policy.
//
// The one exception is executed too rather than described: host loopback still
// requires an authored loopback row under every baseline, and its refusal
// carries the private-destination verdict.
func TestProxyPostureE2EOpenBaselineWithDeny(t *testing.T) {
	postureE2ERequireShard(t)
	fixture, launch := runProxyPostureE2ELaunch(t, postureE2EScenario{
		Key:   "open-deny",
		Rules: postureE2EOpenDenyRules,
		Markers: []string{
			"posture-e2e: open-deny/undenied destination over http: carried",
			"posture-e2e: open-deny/undenied destination over socks5: carried",
			"posture-e2e: open-deny/denied destination over http: refused",
			"posture-e2e: open-deny/denied destination over socks5: refused",
			"posture-e2e: open-deny/private name under an open baseline: carried",
			"posture-e2e: open-deny/private literal under an open baseline: carried",
			"posture-e2e: open-deny/reserved literal under an open baseline: carried",
			"posture-e2e: open-deny/host loopback without an authored row: refused",
		},
	})

	decisions := requireProxyDecisions(t, launch)
	assert.Truef(t,
		postureE2EHasVerdict(decisions, postureE2EDeniedHost,
			fixture.AllowedPort, sandboxproxy.VerdictDeniedByRule),
		"the deny row must be observed executing under an open baseline; records:\n%s",
		strings.Join(launch.Decisions, "\n"))
	assert.Truef(t,
		postureE2EHasVerdict(decisions, postureE2EPrivateHost,
			fixture.AllowedPort, sandboxproxy.VerdictAllowed),
		"a name resolving into private space must be CARRIED under an open baseline (§4.4 amended); records:\n%s",
		strings.Join(launch.Decisions, "\n"))
	assert.Truef(t,
		postureE2EHasAddressVerdict(decisions, fixture.PrivateAddr,
			fixture.AllowedPort, sandboxproxy.VerdictAllowed),
		"a private-space LITERAL must be carried under an open baseline; records:\n%s",
		strings.Join(launch.Decisions, "\n"))
	// NOT VerdictPrivateDestination, and the difference is the mechanism rather
	// than a detail: the private-destination blocker runs at the RESOLVED-
	// address stage, and this target never reaches it. Evaluate refuses first,
	// because the open baseline's accept branch explicitly excludes loopback —
	// host loopback is reachable only through an authored loopback row, under
	// every baseline — so the honest verdict is "no authored row covers this".
	assert.Truef(t,
		postureE2EHasAddressVerdict(decisions, "127.0.0.1",
			fixture.HostAllowed, sandboxproxy.VerdictNotAuthorized),
		"host loopback must still be refused without an authored loopback row, and by the open baseline's own loopback exclusion; records:\n%s",
		strings.Join(launch.Decisions, "\n"))
}

// ---------------------------------------------------------------------------
// Scenario 3: a loopback-only list. No engine deploys; the floor expresses the
// policy natively.
// ---------------------------------------------------------------------------

func postureE2ELoopbackOnlyRules(
	fixture postureE2EFixture,
) sandboxpolicy.NetworkRules {
	return sandboxpolicy.NetworkRules{
		Mode: sandboxpolicy.AccessModeList,
		// The engine is AUTHORED and the launch must still deploy nothing:
		// selecting an engine for a policy that needs no filtering is a latent
		// choice (§1.3-4), not an instruction to run a process.
		Engine: sandboxpolicy.NetworkEngineProxy,
		Allow: []sandboxpolicy.NetworkAllowEntry{
			{Loopback: true, Ports: []int{fixture.HostAllowed}},
		},
	}
}

// TestProxyPostureE2ELoopbackOnlyDeploysNoProxy is the conditional-deployment
// half of the ticket, and the reason this shard provisions the packet floor at
// all: a loopback-only list is a FILTERED posture that is not discriminating,
// so it deploys no engine and runs the floor the packet gateway builds.
//
// The absence is OBSERVED, not inferred. The same host-side process watch that
// sees a proxy in scenario 1 sees none here, the sandbox's own network
// namespace holds no listening socket at all, and the launcher injects no proxy
// discovery — while the authored loopback port is still reachable and an
// unauthored one is not, so the policy is being enforced by the floor rather
// than not enforced at all.
func TestProxyPostureE2ELoopbackOnlyDeploysNoProxy(t *testing.T) {
	postureE2ERequireShard(t)
	runProxyPostureE2ELaunch(t, postureE2EScenario{
		Key:   "loopback-only",
		Rules: postureE2ELoopbackOnlyRules,
		Markers: []string{
			"posture-e2e: loopback-only/proxy discovery in the sandbox: absent",
			"posture-e2e: loopback-only/listening sockets in the sandbox namespace: only the floor's DNS broker",
			"posture-e2e: loopback-only/authored host-loopback port: carried",
			"posture-e2e: loopback-only/unauthored host-loopback port: refused",
		},
	})
}

// ---------------------------------------------------------------------------
// Scenario 4: allow-all. No floor, no proxy.
// ---------------------------------------------------------------------------

func postureE2EAllowAllRules(postureE2EFixture) sandboxpolicy.NetworkRules {
	return sandboxpolicy.NetworkRules{
		Mode:   sandboxpolicy.AccessModeOpen,
		Engine: sandboxpolicy.NetworkEngineProxy,
	}
}

// TestProxyPostureE2EAllowAllDeploysNoFloor is the other end of the table: an
// open policy with no denies asks for no distinction between destinations, so
// there is nothing to filter and nothing to build. The sandbox reaches the
// fixture DIRECTLY — which is the observation that distinguishes "no floor"
// from "a floor that happened to allow it".
func TestProxyPostureE2EAllowAllDeploysNoFloor(t *testing.T) {
	postureE2ERequireShard(t)
	runProxyPostureE2ELaunch(t, postureE2EScenario{
		Key:   "allow-all",
		Rules: postureE2EAllowAllRules,
		Markers: []string{
			"posture-e2e: allow-all/proxy discovery in the sandbox: absent",
			"posture-e2e: allow-all/direct TCP to the fixture: carried",
			"posture-e2e: allow-all/direct TCP outside any authored rule: carried",
		},
	})
}

// ---------------------------------------------------------------------------
// The shared scenario runner: one launch, one deployment answer, parity.
// ---------------------------------------------------------------------------

// postureE2EScenario is one authored posture and what its launch must be
// observed doing inside the sandbox.
type postureE2EScenario struct {
	// Key selects the in-sandbox arm and prefixes its markers.
	Key string
	// Rules builds the authored policy. It is a builder rather than a value
	// because the launching test chooses the host-loopback ports, and a policy
	// authored before they existed would narrow to a port nothing listens on —
	// a refusal for a fabricated reason. Every scenario authors engine: proxy,
	// so the four differ only in the policy, which is precisely the variable
	// the conditional-deployment ruling turns on.
	Rules func(postureE2EFixture) sandboxpolicy.NetworkRules
	// Markers are the observations the in-sandbox arm must report. Each one is
	// an executed check, not a description of one.
	Markers []string
}

// runProxyPostureE2ELaunch builds one real launch for a scenario and asserts
// everything that is the same question for all four: what the deployment
// predicate says, what the host and the sandbox observed, and whether the
// preview surface predicted the mechanism that actually ran.
func runProxyPostureE2ELaunch(
	t *testing.T,
	scenario postureE2EScenario,
) (postureE2EFixture, proxyEngineLaunchResult) {
	t.Helper()
	fixture := postureE2ERunnerFixture(t)
	// The launching test owns both host-loopback halves: an allowed port an
	// authored loopback row can reach, and a LIVE denied port, so a refusal
	// there is the policy answering rather than nothing listening.
	fixture.HostAllowed = proxySmokeLoopbackServer(t)
	fixture.HostDenied = proxySmokeLoopbackServer(t)
	rules := scenario.Rules(fixture)

	deploysProxy := postureE2EDeploysProxy(t, rules)
	observer := &postureE2EProxyProcessObserver{}

	helperBinaryName := "posture-e2e-helper"
	launch := runProxyEngineLaunch(t, proxyEngineLaunchInput{
		Rules: rules,
		ExtraEnv: append(postureE2EHelperLaunchEnv(fixture),
			postureE2EHelperEnv+"="+scenario.Key),
		Command: func(workspace string) string {
			return clcommon.ShellQuoteArg(
				filepath.Join(workspace, helperBinaryName)) +
				" -test.run=^TestProxyPostureE2EHelper$ -test.v"
		},
		WorkspaceBinaries: map[string]string{helperBinaryName: os.Args[0]},
		WhileRunning:      observer.watch,
	})

	// The launch derives posture and engine from the axes the launcher plans,
	// while the assertions above derive them from the authored rules. Both call
	// the production functions, but they are different seams — so they are
	// compared rather than assumed equal. If planning ever normalized or widened
	// a policy, this says so plainly instead of surfacing as a scenario that
	// mysteriously expected the wrong deployment.
	require.Equalf(t, deploysProxy, launch.DeploysProxy,
		"the authored policy and the planned launch disagree about deployment "+
			"(planned posture %v, engine %v)",
		launch.Posture, launch.Engine)

	for _, marker := range scenario.Markers {
		assert.Containsf(t, launch.Output, marker,
			"the %s scenario must OBSERVE each check executing", scenario.Key)
	}

	// The host-side watch. Its anti-vacuous half is asserted first: an observer
	// that never sampled would report "no proxy process" for every scenario,
	// including the ones that run one.
	assert.Positivef(t, observer.Samples(),
		"the host-side proxy-process watch completed no scan of the process table, so its verdict is worthless")
	assert.Equalf(t, deploysProxy, observer.SawProxy(),
		"a filtering proxy process was %s while the %s sandbox ran",
		postureE2EPresence(observer.SawProxy()), scenario.Key)

	// The proxy's own decision record: present exactly when a proxy ran. Every
	// scenario that deploys one asks it questions, so an empty record there is
	// a broken audit trail rather than a quiet launch.
	assert.Equalf(t, deploysProxy, len(launch.Decisions) > 0,
		"the filtering proxy's decision record was %s for the %s launch",
		postureE2EPresence(len(launch.Decisions) > 0), scenario.Key)

	postureE2EAssertPreviewParity(t, rules, deploysProxy)
	return fixture, launch
}

// postureE2EDeploysProxy answers the deployment question ONCE, through the
// production predicate over the production engine resolution. Every assertion
// in this file compares against this value rather than against a per-scenario
// expectation, so a scenario cannot be written to expect a deployment the
// launcher would not perform.
func postureE2EDeploysProxy(
	t *testing.T,
	rules sandboxpolicy.NetworkRules,
) bool {
	t.Helper()
	posture, err := sandboxpolicy.NetworkPostureForRules(rules)
	require.NoError(t, err)
	engine, err := sandboxpolicy.DeployedNetworkEngineForRules(rules)
	require.NoError(t, err)
	return TclaudeLayerDeploysProxy(posture, engine)
}

// postureE2EAssertPreviewParity is the disclosure-honesty check at system
// level: the preview surface an operator reads before launching must name the
// mechanism this launch actually ran.
//
// The platform is passed explicitly rather than taken from runtime.GOOS. That
// is the durable shape for M3: a Darwin arm adds a platform row here instead of
// rewriting the assertion, and a prediction whose behavior depends on the host
// it runs on cannot be asserted at all.
func postureE2EAssertPreviewParity(
	t *testing.T,
	rules sandboxpolicy.NetworkRules,
	deploysProxy bool,
) {
	t.Helper()
	snapshot := sandboxpolicy.EmptySnapshot()
	snapshot.Effective.Network = &rules
	axes, err := sandboxpolicy.PlannedEffectiveAccessAxes(snapshot.Effective)
	require.NoError(t, err)
	predicted, err := harness.PredictAccessEnforcement(
		harness.MustGet(harness.DefaultName),
		sandboxpolicy.ImplementationTclaudeLayer,
		axes, "", "linux")
	require.NoError(t, err)

	assert.Equalf(t, deploysProxy,
		predicted.Mechanism == harness.ProxyEngineLinuxMechanism,
		"the preview predicted mechanism %q for a launch that %s a filtering proxy",
		predicted.Mechanism, map[bool]string{true: "deployed", false: "did not deploy"}[deploysProxy])
	if deploysProxy {
		// The observed behavior of these scenarios is a policy being enforced,
		// so the predicted rating must say so. A prediction of "unenforced" for
		// a launch this file just watched enforce is the same disclosure bug in
		// the other direction.
		assert.Equalf(t, harness.EnforceFull, predicted.NetworkList,
			"the preview predicts %q for a launch observed enforcing its list",
			predicted.NetworkList)
	}
}

func postureE2EPresence(present bool) string {
	if present {
		return "PRESENT"
	}
	return "absent"
}

// postureE2ERequireShard gates every top-level smoke on the shard that supplies
// the fixture. The gate is the shard's, not the proxy smokes', so one shard
// cannot silently start running the other's flows against a fixture built for
// something else.
func postureE2ERequireShard(t *testing.T) {
	t.Helper()
	if os.Getenv(postureE2EEnv) != "1" {
		t.Skip("set TCLAUDE_PROXY_POSTURE_E2E=1 on the executing Linux CI boundary")
	}
}

// postureE2EHelperEnv passes the live fixture to the in-sandbox helper.
func postureE2EHelperLaunchEnv(fixture postureE2EFixture) []string {
	return append(
		proxySmokeHelperEnv(nil, fixture.proxySmokeFixture),
		postureE2EPrivateAddrEnv+"="+fixture.PrivateAddr,
		postureE2EHostAllowedEnv+"="+strconv.Itoa(fixture.HostAllowed),
		postureE2EHostDeniedEnv+"="+strconv.Itoa(fixture.HostDenied),
	)
}

// ---------------------------------------------------------------------------
// Host-side observation: is a filtering proxy process running?
// ---------------------------------------------------------------------------

// postureE2EProxyProcessObserver watches the host's process table while a
// sandbox runs.
//
// This is the only assertion in the suite that can say a proxy is ABSENT rather
// than merely unused: from inside the sandbox the two are indistinguishable,
// and the launcher's own flag says what was ASKED for rather than what ran.
//
// A process counts only if it is the tclaude binary this shard built AND
// carries the supervisor flag: the /bin/sh that wraps the launch has the flag
// in its command line too, and counting it would report a proxy for a launch
// whose supervisor failed to start.
type postureE2EProxyProcessObserver struct {
	samples int
	saw     bool
}

func (o *postureE2EProxyProcessObserver) Samples() int { return o.samples }
func (o *postureE2EProxyProcessObserver) SawProxy() bool {
	return o.saw
}

func (o *postureE2EProxyProcessObserver) watch(
	_ *testing.T,
	done <-chan struct{},
) {
	binary := strings.TrimSpace(os.Getenv(filteredGatewayTclaudeBinaryEnv))
	resolved, err := filepath.Abs(binary)
	if err != nil {
		resolved = binary
	}
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		// Only a COMPLETED scan counts. Incrementing unconditionally would make
		// the caller's anti-vacuity assertion unfalsifiable — it would hold even
		// if every /proc read had failed, which is precisely the state in which
		// "no proxy process" means "we never looked".
		found, scanned := postureE2EProxySupervisorRunning(resolved)
		if scanned {
			o.samples++
		}
		if found {
			o.saw = true
		}
		select {
		case <-done:
			// One last sample after the command exited is deliberately NOT
			// taken: the supervisor's lifetime is the sandbox's, so a race
			// against teardown could only ever lose a proxy, never invent one.
			return
		case <-ticker.C:
		}
	}
}

// postureE2EProxySupervisorRunning reports whether any live process is this
// shard's tclaude running as the proxy supervisor, and whether the process
// table could be read at all. The second return is what separates "no proxy is
// running" from "this never looked".
//
// argv[0] is compared, not just the command line: the /bin/sh that wraps every
// launch carries the supervisor flag in ITS command line too, and counting that
// would report a running proxy for a launch whose supervisor never started.
func postureE2EProxySupervisorRunning(binary string) (found, scanned bool) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false, false
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, err := strconv.Atoi(entry.Name()); err != nil {
			continue
		}
		raw, err := os.ReadFile(
			filepath.Join("/proc", entry.Name(), "cmdline"))
		if err != nil {
			continue
		}
		// /proc/<pid>/cmdline is NUL-separated, so argv[0] is the first field.
		argv := strings.Split(string(raw), "\x00")
		if len(argv) == 0 || argv[0] != binary {
			continue
		}
		if strings.Contains(string(raw), "--proxy-network-policy") {
			return true, true
		}
	}
	return false, true
}

// ---------------------------------------------------------------------------
// Decision-record helpers
// ---------------------------------------------------------------------------

// postureE2EHasVerdict requires the EXACT verdict for a name target rather than
// "not allowed": a refusal for the wrong reason — an unreachable fixture, a
// policy that authorized nothing — would otherwise read as the policy working.
func postureE2EHasVerdict(
	decisions []proxyDecisionRecord,
	host string,
	port int,
	verdict sandboxproxy.Verdict,
) bool {
	for _, decision := range decisions {
		if decision.Host == host && decision.Port == port &&
			decision.Verdict == string(verdict) {
			return true
		}
	}
	return false
}

// postureE2EHasAddressVerdict is postureE2EHasVerdict for a literal target,
// which the record carries in its address field rather than its host field.
func postureE2EHasAddressVerdict(
	decisions []proxyDecisionRecord,
	address string,
	port int,
	verdict sandboxproxy.Verdict,
) bool {
	for _, decision := range decisions {
		if decision.Address == address && decision.Port == port &&
			decision.Verdict == string(verdict) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// The in-sandbox helper
// ---------------------------------------------------------------------------

// TestProxyPostureE2EHelper runs INSIDE the sandbox and executes one scenario's
// checks. Every check is an executed round trip or an executed refusal, and it
// reports each one so the outer boundary can require the observation rather
// than trust an exit status.
func TestProxyPostureE2EHelper(t *testing.T) {
	scenario := os.Getenv(postureE2EHelperEnv)
	if scenario == "" {
		t.Skip("proxy posture end-to-end smoke helper")
	}
	fixture := postureE2EFixtureFromEnv(t)
	switch scenario {
	case "discriminating":
		postureE2EDiscriminatingHelper(t, fixture)
	case "open-deny":
		postureE2EOpenDenyHelper(t, fixture)
	case "loopback-only":
		postureE2ELoopbackOnlyHelper(t, fixture)
	case "allow-all":
		postureE2EAllowAllHelper(t, fixture)
	default:
		t.Fatalf("unknown posture scenario %q", scenario)
	}
}

func postureE2EDiscriminatingHelper(t *testing.T, fixture postureE2EFixture) {
	endpoint := proxySmokeProxyEndpoint(t)
	allowed := net.JoinHostPort(
		postureE2EAllowedHost, strconv.Itoa(fixture.AllowedPort))
	denied := net.JoinHostPort(
		postureE2EDeniedHost, strconv.Itoa(fixture.AllowedPort))

	postureE2ECarriages(t, "discriminating", "authored destination",
		endpoint, allowed, false, true)
	postureE2ECarriages(t, "discriminating", "denied destination",
		endpoint, denied, false, false)

	// The floor: everything that does not go through the proxy is gone. Each
	// one is executed, and the refusal itself is what is reported.
	direct := net.JoinHostPort(
		fixture.AllowedAddr, strconv.Itoa(fixture.AllowedPort))
	conn, err := net.DialTimeout("tcp", direct, postureE2EDialTimeout)
	if err == nil {
		_ = conn.Close()
		t.Fatalf("direct TCP to %s succeeded inside the floor", direct)
	}
	fmt.Printf(
		"posture-e2e: discriminating/direct TCP outside the proxy: refused (%v)\n",
		err)

	udpRefusal, err := proxySmokeSendUDP(direct)
	require.NoError(t, err)
	require.NotNil(t, udpRefusal, "a UDP datagram left the sandbox to %s", direct)
	fmt.Printf("posture-e2e: discriminating/UDP outside the proxy: refused (%v)\n",
		udpRefusal)

	icmpRefusal, err := proxySmokePingEcho(fixture.AllowedAddr)
	require.NoError(t, err)
	require.NotNil(t, icmpRefusal, "an ICMP echo left the sandbox")
	fmt.Printf("posture-e2e: discriminating/ICMP outside the proxy: refused (%v)\n",
		icmpRefusal)

	// The listener side of "a proxy is deployed": in this namespace exactly one
	// socket listens, and it is the endpoint the launcher advertised. The
	// scenarios that deploy no proxy run the same read and must find none.
	_, proxyPort, err := net.SplitHostPort(endpoint)
	require.NoError(t, err)
	require.Equal(t, []string{proxyPort}, postureE2EListeningPorts(t),
		"the proxy listener must be the only socket listening in this namespace")
	fmt.Println(
		"posture-e2e: discriminating/the proxy listener is the only one: verified")
}

func postureE2EOpenDenyHelper(t *testing.T, fixture postureE2EFixture) {
	endpoint := proxySmokeProxyEndpoint(t)
	port := strconv.Itoa(fixture.AllowedPort)

	postureE2ECarriages(t, "open-deny", "undenied destination", endpoint,
		net.JoinHostPort(postureE2EAllowedHost, port), false, true)
	postureE2ECarriages(t, "open-deny", "denied destination", endpoint,
		net.JoinHostPort(postureE2EDeniedHost, port), false, false)

	// §4.4 as amended: under an open baseline the private-destination blocker
	// does not apply, so both a name that resolves into private space and the
	// literal itself stay reachable.
	postureE2EOneCarriage(t, "open-deny", "private name under an open baseline",
		endpoint, net.JoinHostPort(postureE2EPrivateHost, port), true)
	postureE2EOneCarriage(t, "open-deny", "private literal under an open baseline",
		endpoint, net.JoinHostPort(fixture.PrivateAddr, port), true)
	postureE2EOneCarriage(t, "open-deny", "reserved literal under an open baseline",
		endpoint, net.JoinHostPort(fixture.AdjacentAddr, port), true)

	// The stated exception: the host itself always needs an authored loopback
	// row, under every baseline, and this policy has none.
	postureE2EOneCarriage(t, "open-deny",
		"host loopback without an authored row", endpoint,
		net.JoinHostPort("127.0.0.1", strconv.Itoa(fixture.HostAllowed)),
		false)
}

func postureE2ELoopbackOnlyHelper(t *testing.T, fixture postureE2EFixture) {
	postureE2ERequireNoProxyDiscovery(t, "loopback-only")

	// The only socket listening in this namespace is the PACKET floor's own DNS
	// broker. A deployed proxy's listener would live exactly here — the
	// bootstrap creates it INSIDE the namespace — so an exact set is the
	// absence assertion the ticket asks for, made where the thing would be.
	//
	// Exact rather than "no proxy port": the proxy's port is ephemeral and
	// unknowable to a sandbox that was never told about one, so the only
	// statement that can exclude it is a complete inventory. That makes this
	// assertion loud if the floor ever grows another listener, which is the
	// right failure for a smoke whose whole subject is what is running.
	require.Equalf(t,
		[]string{strconv.Itoa(filteredNetworkDNSUpstreamPort)},
		postureE2EListeningPorts(t),
		"the packet floor's DNS broker must be the ONLY listener in a namespace with no filtering proxy")
	fmt.Println(
		"posture-e2e: loopback-only/listening sockets in the sandbox namespace: only the floor's DNS broker")

	// The floor is enforcing the authored policy natively: the authored host
	// loopback port answers and the unauthored one does not. Without both, an
	// absent proxy would be indistinguishable from an absent policy.
	allowed := net.JoinHostPort(
		sandboxpolicy.FilteredNetworkHostLoopbackName,
		strconv.Itoa(fixture.HostAllowed))
	require.NoErrorf(t, postureE2ETCPRoundTrip(allowed),
		"the authored host-loopback port must be reachable through the floor")
	fmt.Println("posture-e2e: loopback-only/authored host-loopback port: carried")

	denied := net.JoinHostPort(
		sandboxpolicy.FilteredNetworkHostLoopbackName,
		strconv.Itoa(fixture.HostDenied))
	// Dial-only, deliberately. A round trip fails for a refused connection AND
	// for a short read or an echo mismatch, so requiring "some error" here would
	// let a fixture problem stand in for the floor refusing — the one refusal in
	// this shard that would otherwise not be distinguished from a broken
	// fixture. The port is LIVE on the host, so a failed connect is the policy.
	require.Errorf(t, postureE2ETCPDial(denied),
		"an unauthored host-loopback port must be refused by the floor")
	fmt.Println("posture-e2e: loopback-only/unauthored host-loopback port: refused")
}

func postureE2EAllowAllHelper(t *testing.T, fixture postureE2EFixture) {
	postureE2ERequireNoProxyDiscovery(t, "allow-all")

	// No floor at all: the fixture is reached DIRECTLY, with no proxy in the
	// path and nothing filtering the route. Reaching an address outside every
	// authored rule is the second half — a floor that happened to allow the
	// first would not allow this one.
	direct := net.JoinHostPort(
		fixture.AllowedAddr, strconv.Itoa(fixture.AllowedPort))
	require.NoErrorf(t, postureE2ETCPRoundTrip(direct),
		"an allow-all policy builds no floor, so the fixture must be reachable directly")
	fmt.Println("posture-e2e: allow-all/direct TCP to the fixture: carried")

	adjacent := net.JoinHostPort(
		fixture.AdjacentAddr, strconv.Itoa(fixture.AllowedPort))
	require.NoErrorf(t, postureE2ETCPRoundTrip(adjacent),
		"an allow-all policy distinguishes no destinations")
	fmt.Println("posture-e2e: allow-all/direct TCP outside any authored rule: carried")
}

// postureE2ERequireNoProxyDiscovery asserts the launcher injected no proxy
// discovery. On a launch that deploys a proxy these variables are the sandbox's
// only route out, and the floor smoke pins their exact values; a launch that
// deploys none must leave them alone, or a harness would be pointed at a proxy
// that does not exist.
func postureE2ERequireNoProxyDiscovery(t *testing.T, scenario string) {
	t.Helper()
	for _, name := range []string{
		"HTTP_PROXY", "http_proxy",
		"HTTPS_PROXY", "https_proxy",
		"ALL_PROXY", "all_proxy",
	} {
		require.Emptyf(t, os.Getenv(name),
			"%s must not be injected on a launch that deploys no proxy", name)
	}
	fmt.Printf("posture-e2e: %s/proxy discovery in the sandbox: absent\n", scenario)
}

// postureE2ECarriages asks one policy question over BOTH carriages and requires
// them to agree before either is compared to the expectation: a drift between
// them is a distinct failure from a policy being wrong.
func postureE2ECarriages(
	t *testing.T,
	scenario, name, endpoint, target string,
	literal, allowed bool,
) {
	t.Helper()
	status, err := proxySmokeHTTPConnect(endpoint, target)
	require.NoError(t, err, "the proxy must answer rather than hang up")
	socksReply, err := proxySmokeSOCKS5Connect(endpoint, target, literal)
	require.NoError(t, err)

	httpCarried := status == 200
	socksCarried := socksReply == 0
	require.Equalf(t, httpCarried, socksCarried,
		"carriages disagreed for %s (HTTP %d, SOCKS5 reply %d)",
		target, status, socksReply)
	require.Equal(t, allowed, httpCarried)
	if !allowed {
		require.Equal(t, 403, status, "a refusal must be a legible policy answer")
		require.Equal(t, byte(2), socksReply,
			"SOCKS5 refusals use connection-not-allowed")
	}
	fmt.Printf("posture-e2e: %s/%s over http: %s\n",
		scenario, name, postureE2EVerdict(allowed))
	fmt.Printf("posture-e2e: %s/%s over socks5: %s\n",
		scenario, name, postureE2EVerdict(allowed))
}

// postureE2EOneCarriage asks a question over the HTTP CONNECT carriage alone.
// Carriage equivalence is proven by the cases above and by the policy smoke's
// whole table; these cases are about the BASELINE, and asking them twice would
// add a second decision record for the same question without a second claim.
func postureE2EOneCarriage(
	t *testing.T,
	scenario, name, endpoint, target string,
	allowed bool,
) {
	t.Helper()
	status, err := proxySmokeHTTPConnect(endpoint, target)
	require.NoError(t, err, "the proxy must answer rather than hang up")
	require.Equalf(t, allowed, status == 200,
		"unexpected proxy status %d for %s", status, target)
	if !allowed {
		require.Equal(t, 403, status, "a refusal must be a legible policy answer")
	}
	fmt.Printf("posture-e2e: %s/%s: %s\n",
		scenario, name, postureE2EVerdict(allowed))
}

func postureE2EVerdict(allowed bool) string {
	if allowed {
		return "carried"
	}
	return "refused"
}

// postureE2ETCPRoundTrip proves a destination genuinely answers rather than
// merely accepting: the fixture listeners echo, so a completed round trip
// separates "carried" from "connected to something".
func postureE2ETCPRoundTrip(target string) error {
	conn, err := net.DialTimeout("tcp", target, postureE2EDialTimeout)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(postureE2EDialTimeout)); err != nil {
		return err
	}
	token := "posture-e2e-" + target
	if _, err := conn.Write([]byte(token)); err != nil {
		return err
	}
	// ReadFull, not Read: a valid echo may arrive in more than one segment, and
	// a short read would be reported as an echo MISMATCH — a fabricated policy
	// failure in the arms where this helper proves a destination is carried.
	reply := make([]byte, len(token))
	if _, err := io.ReadFull(conn, reply); err != nil {
		return err
	}
	if string(reply) != token {
		return fmt.Errorf("echo mismatch from %s: %q", target, reply)
	}
	return nil
}

// postureE2ETCPDial reports whether a connection could be established at all,
// with none of the round trip's other failure modes folded in.
func postureE2ETCPDial(target string) error {
	conn, err := net.DialTimeout("tcp", target, postureE2EDialTimeout)
	if err != nil {
		return err
	}
	return conn.Close()
}

// postureE2EListeningPorts reports every distinct TCP port listening in the
// CALLER's network namespace, read from the kernel rather than from a tool the
// sandbox may not have.
//
// Ports are deduplicated across the v4 and v6 tables on purpose: a single
// listener bound dual-stack appears twice, and the question these smokes ask is
// WHICH ports answer, not how many sockets implement them.
func postureE2EListeningPorts(t *testing.T) []string {
	t.Helper()
	seen := map[string]bool{}
	ports := []string{}
	read := 0
	for _, path := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		file, err := os.Open(path)
		if err != nil {
			continue
		}
		read++
		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			fields := strings.Fields(scanner.Text())
			// sl local_address rem_address st ...; state 0A is TCP_LISTEN.
			if len(fields) < 4 || fields[3] != "0A" {
				continue
			}
			_, hexPort, found := strings.Cut(fields[1], ":")
			if !found {
				continue
			}
			port, err := strconv.ParseUint(hexPort, 16, 32)
			if err != nil {
				continue
			}
			text := strconv.FormatUint(port, 10)
			if seen[text] {
				continue
			}
			seen[text] = true
			ports = append(ports, text)
		}
		require.NoError(t, scanner.Err())
		require.NoError(t, file.Close())
	}
	// An inventory assembled from no table at all is an empty list that means
	// "could not look", and the scenarios that assert absence would read it as
	// "nothing was listening".
	require.Positive(t, read,
		"neither /proc/net/tcp nor /proc/net/tcp6 could be read; an empty listener inventory would not be evidence")
	return ports
}
