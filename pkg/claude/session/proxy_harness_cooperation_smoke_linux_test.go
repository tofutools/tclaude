//go:build linux

package session

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// §8.1 test 4 for the proxy filtering engine: do the pinned harnesses actually
// work through the proxy floor, and is every destination they reach one the
// policy authorized?
//
// # Where the auditing happens, and why it moved
//
// TestPinnedFilteredModelEndpointEvidence puts a CONNECT auditor in front of an
// unsandboxed harness and reads what the harness asked for. That works there
// because the harness talks to the auditor directly.
//
// Under this posture the tclaude proxy IS the CONNECT auditor, and it is a
// strictly better one: the floor is an empty network namespace, so the proxy is
// the only exit, and every connection the harness attempts must present itself
// for a decision. The auditing record is therefore the proxy's own decision log
// — which is why the supervisor runs at debug level here — and its completeness
// is a property of the floor rather than of the harness cooperating. "The
// harness made no connection the auditor did not see" is not asserted by
// counting; it is the floor's guarantee, and the floor smoke proves it
// separately by executing the denials.
//
// # Credential rule (categorical)
//
// Real credentials are NEVER put in CI. Every harness here runs on deliberately
// invalid credentials and proves carriage at the proxy: the request reaching
// the right origin through the proxy is the evidence, and the turn stopping
// before it becomes billable is by design rather than a budget concession.
//
// # Runner fixture contract
//
// In addition to the floor/policy smoke fixture, the job must map each pinned
// harness's model origin in the HOST's /etc/hosts to TCLAUDE_FILTERED_ALLOWED_ADDR
// and run a listener there on port 443:
//
//	api.anthropic.com -> TCLAUDE_FILTERED_ALLOWED_ADDR
//	api.openai.com    -> TCLAUDE_FILTERED_ALLOWED_ADDR
//
// and a listener on port 443 at that address. 443 is mandatory, not a
// parameter: the pinned harnesses pick it themselves.
//
// The mapping is what keeps this smoke offline: no packet reaches a real model
// provider, so an invalid credential cannot even be presented to one. The
// listener has to be live so that an ALLOWED verdict is followed by a real
// upstream connection — otherwise a green run could not tell "policy allowed
// it" from "policy allowed it and the tunnel was actually carried".
const (
	// proxyCooperationOriginPort is 443 and is NOT read from the job. The
	// pinned harnesses choose that port themselves — nothing here overrides a
	// base URL — so a policy authorizing any other port would refuse their
	// CONNECT for want of a matching row, and the smoke would fail for a reason
	// that has nothing to do with the harness. Pinning it here keeps the
	// fixture contract and the authored policy from drifting apart.
	proxyCooperationOriginPort = 443

	proxyCooperationProbeEnv    = "TCLAUDE_FILTERED_PROXY_COOPERATION_PROBE"
	proxyCooperationProbeMarker = "proxy-cooperation-probe"

	// proxyCooperationUndeclaredHost is a name no scenario authorizes. It is
	// probed on purpose so a green run has an OBSERVED refusal in it: a policy
	// that happened to be asked nothing undeclared would otherwise satisfy
	// "every undeclared origin was refused" vacuously.
	proxyCooperationUndeclaredHost = "undeclared.proxy.tclaude.test"
)

// proxyCooperationScenario is one pinned harness and the origins it is allowed
// to reach. The expected set is the ENTIRE authorization: anything else the
// harness attempts is refused by the policy and observed as such.
type proxyCooperationScenario struct {
	name    string
	binary  string
	version string
	args    []string
	env     map[string]string
	origins []string
	prepare func(t *testing.T, home string) map[string]string
}

// TestPinnedProxyHarnessCooperation runs each pinned harness inside a real
// proxy-engine launch and asserts, from the proxy's own record, that it reached
// the origin it is supposed to reach and nothing else.
func TestPinnedProxyHarnessCooperation(t *testing.T) {
	if os.Getenv(proxySmokeEnv) != "1" {
		t.Skip("set TCLAUDE_FILTERED_PROXY_SMOKE=1 on the executing Linux CI boundary")
	}
	fixture := proxySmokeRunnerFixture(t)
	originPort := proxyCooperationOriginPort

	claude := requirePinnedFilteredHarness(t, "claude", filteredClaudePinnedVersion)
	codex := requirePinnedFilteredHarness(t, "codex", filteredCodexPinnedVersion)

	carriageByHarness := map[string][]string{}
	for _, scenario := range []proxyCooperationScenario{
		{
			name:    "claude",
			binary:  claude,
			version: filteredClaudePinnedVersion,
			args: []string{
				"--print", "--model", "sonnet", "Reply with exactly ok.",
			},
			env: map[string]string{
				"ANTHROPIC_API_KEY": "invalid-ci-evidence-key",
			},
			origins: []string{"api.anthropic.com"},
		},
		{
			name:    "codex",
			binary:  codex,
			version: filteredCodexPinnedVersion,
			args: append(filteredCodexEndpointEvidenceArgs(),
				"exec", "--skip-git-repo-check", "--model", "gpt-5.4",
				"Reply with exactly ok."),
			origins: []string{"api.openai.com"},
			prepare: func(t *testing.T, home string) map[string]string {
				t.Helper()
				codexHome := filepath.Join(home, ".codex")
				require.NoError(t, os.MkdirAll(codexHome, 0o700))
				require.NoError(t, os.WriteFile(
					filepath.Join(codexHome, "auth.json"),
					[]byte(`{"auth_mode":"apikey","OPENAI_API_KEY":"invalid-ci-evidence-key"}`),
					0o600))
				return map[string]string{"CODEX_HOME": codexHome}
			},
		},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			decisions := runProxyCooperationScenario(
				t, scenario, fixture, originPort)
			require.NotEmpty(t, decisions,
				"the proxy recorded no decision at all; the harness never reached it")

			declared := append([]string(nil), scenario.origins...)
			declared = append(declared, proxySmokeAllowedHost)

			// 1. The expected model origin was observed AT THE PROXY, allowed,
			//    and actually carried.
			for _, origin := range scenario.origins {
				assert.Truef(t,
					proxyDecisionsInclude(decisions, origin, originPort, "allowed"),
					"%s did not reach its model origin %s through the proxy; decisions:\n%s",
					scenario.name, origin, formatProxyDecisions(decisions))
			}

			// 2. Every origin the harness asked for that the policy did not
			//    declare was refused. This reads the whole record rather than
			//    a list of names guessed in advance, so an origin nobody
			//    anticipated cannot slip past by not being asserted about.
			for _, decision := range decisions {
				if slices.Contains(declared, decision.Host) {
					continue
				}
				// A LITERAL target has no Host — it records an Address — and
				// treating that empty Host as an undeclared name would fail the
				// test for the wrong reason on any harness that connects by IP.
				// Literals are authorized by the fixture CIDR row, so an allowed
				// one is inside the authored policy, not outside it.
				if decision.Kind == "literal" {
					continue
				}
				assert.NotEqualf(t, "allowed", decision.Verdict,
					"%s reached undeclared origin %s through the proxy",
					scenario.name, decision.Destination())
			}

			// 3. Anti-vacuous: at least one refusal actually executed. The
			//    deliberate undeclared probe below guarantees there is one to
			//    find, so its absence means the record is incomplete rather
			//    than that the harness behaved.
			assert.Truef(t, proxyDecisionsIncludeRefusal(
				decisions, proxyCooperationUndeclaredHost),
				"the deliberate undeclared probe was not refused-and-recorded; decisions:\n%s",
				formatProxyDecisions(decisions))

			// 4. The §6 ALL_PROXY question, answered empirically instead of by
			//    string inspection: which carriages did this harness's launch
			//    actually use? Recorded rather than asserted — a harness that
			//    never uses SOCKS is a capability fact about that harness, not
			//    a failure.
			carriages := proxyDecisionCarriages(decisions, scenario.origins)
			carriageByHarness[scenario.name] = carriages
			fmt.Printf(
				"proxy-cooperation: %s %s: model origins %s carried over %v\n",
				scenario.name, scenario.version,
				strings.Join(scenario.origins, ","), carriages)
		})
	}

	require.Len(t, carriageByHarness, 2,
		"every declared harness scenario must have produced a carriage record")
}

// runProxyCooperationScenario launches one harness inside the floor, preceded
// by a probe that exercises both carriages against a destination no policy
// authorizes, and returns the proxy's decision record for the whole launch.
func runProxyCooperationScenario(
	t *testing.T,
	scenario proxyCooperationScenario,
	fixture proxySmokeFixture,
	originPort int,
) []proxyDecisionRecord {
	t.Helper()
	rules := proxyCooperationRules(scenario.origins, fixture, originPort)
	probeBinary := "proxy-cooperation-probe"

	// The harness's HOME is the sandbox home the launch builds, so any fixture
	// the scenario writes has to land there before the launch is constructed.
	// runProxyEngineLaunch owns that directory, so the scenario's own
	// preparation is threaded through the environment it returns.
	env := append([]string(nil), os.Environ()...)
	env = append(env, proxySmokeHelperEnv(nil, fixture)...)
	env = append(env,
		proxyCooperationProbeEnv+"=1",
	)
	for name, value := range scenario.env {
		env = append(env, name+"="+value)
	}

	launch := runProxyEngineLaunch(t, proxyEngineLaunchInput{
		Rules:             rules,
		ExtraEnv:          env,
		WorkspaceBinaries: map[string]string{probeBinary: os.Args[0]},
		PrepareHome:       scenario.prepare,
		Timeout:           180 * time.Second,
		// The harness runs on invalid credentials and is EXPECTED to exit
		// non-zero. This is the only launch in the set that tolerates it.
		AllowExitError: true,
		Command: func(workspace string) string {
			// The probe runs first and unconditionally, then the harness runs
			// whatever its own exit status. Both are inside the same floor,
			// under the same launcher-owned proxy discovery.
			return clcommon.ShellQuoteArg(filepath.Join(workspace, probeBinary)) +
				" -test.run=^TestProxyCooperationProbeHelper$ -test.v; " +
				clcommon.ShellQuoteArg(scenario.binary) + " " +
				strings.Join(shellQuoteAll(scenario.args), " ")
		},
	})
	t.Logf("proxy cooperation launch output:\n%s", launch.Output)
	return requireProxyDecisions(t, launch)
}

// proxyCooperationRules is the authored policy for one scenario: exactly the
// harness's own model origins, plus the fixture CIDR that clears their resolved
// addresses through the private-destination blocker.
//
// The CIDR row is not a second authorization of the origins. The fixture
// address space is reserved (RFC 2544 benchmarking), and §4.4's blocker refuses
// a name that resolves into reserved space unless an authored CIDR row covers
// it. Without this row every scenario would be refused for the wrong reason and
// the smoke would prove nothing about the harness.
func proxyCooperationRules(
	origins []string,
	fixture proxySmokeFixture,
	originPort int,
) sandboxpolicy.NetworkRules {
	allow := []sandboxpolicy.NetworkAllowEntry{
		{CIDR: fixture.AllowedPrefix, Ports: []int{originPort}},
	}
	for _, origin := range origins {
		allow = append(allow, sandboxpolicy.NetworkAllowEntry{
			Host: origin, Ports: []int{originPort},
		})
	}
	// The floor smoke's allowed host rides along so the probe has a declared
	// destination to compare its undeclared one against, over both carriages.
	allow = append(allow, sandboxpolicy.NetworkAllowEntry{
		Host: proxySmokeAllowedHost, Ports: []int{fixture.AllowedPort},
	})
	return sandboxpolicy.NetworkRules{
		Mode:   sandboxpolicy.AccessModeList,
		Allow:  allow,
		Engine: sandboxpolicy.NetworkEngineProxy,
	}
}

// TestProxyCooperationProbeHelper runs INSIDE the sandbox, before the harness.
// It asks one declared and one undeclared question over each carriage, so the
// launch's decision record is guaranteed to contain an executed refusal on both
// carriages rather than only whatever the harness happened to do.
func TestProxyCooperationProbeHelper(t *testing.T) {
	if os.Getenv(proxyCooperationProbeEnv) != "1" {
		t.Skip("proxy cooperation probe helper")
	}
	fixture := proxySmokeRunnerFixture(t)
	endpoint := proxySmokeProxyEndpoint(t)

	declared := net.JoinHostPort(
		proxySmokeAllowedHost, strconv.Itoa(fixture.AllowedPort))
	undeclared := net.JoinHostPort(
		proxyCooperationUndeclaredHost, strconv.Itoa(fixture.AllowedPort))

	status, err := proxySmokeHTTPConnect(endpoint, declared)
	require.NoError(t, err)
	require.Equal(t, 200, status, "a declared destination must be carried")
	fmt.Printf("%s: declared/http: carried\n", proxyCooperationProbeMarker)

	status, err = proxySmokeHTTPConnect(endpoint, undeclared)
	require.NoError(t, err, "the proxy must answer rather than hang up")
	require.Equal(t, 403, status, "an undeclared destination must be refused")
	fmt.Printf("%s: undeclared/http: refused\n", proxyCooperationProbeMarker)

	reply, err := proxySmokeSOCKS5Connect(endpoint, undeclared, false)
	require.NoError(t, err)
	require.Equal(t, byte(2), reply,
		"SOCKS5 refusals use connection-not-allowed")
	fmt.Printf("%s: undeclared/socks5: refused\n", proxyCooperationProbeMarker)
}

// proxyDecisionRecord is one line of the proxy's own account of this launch.
type proxyDecisionRecord struct {
	Message  string `json:"msg"`
	Carriage string `json:"carriage"`
	Kind     string `json:"target_kind"`
	Host     string `json:"host"`
	Address  string `json:"address"`
	Port     int    `json:"port"`
	Verdict  string `json:"verdict"`
}

// Destination renders the record the way a reader thinks of it.
func (r proxyDecisionRecord) Destination() string {
	host := r.Host
	if host == "" {
		host = r.Address
	}
	return net.JoinHostPort(host, strconv.Itoa(r.Port))
}

func parseProxyDecisions(t *testing.T, lines []string) []proxyDecisionRecord {
	t.Helper()
	records := make([]proxyDecisionRecord, 0, len(lines))
	for _, line := range lines {
		var record proxyDecisionRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			// A malformed line is a broken contract, not noise to skip: the
			// assertions below read this record and would silently weaken.
			require.NoErrorf(t, err, "unparseable proxy decision record: %s", line)
		}
		records = append(records, record)
	}
	return records
}

func proxyDecisionsInclude(
	decisions []proxyDecisionRecord,
	host string,
	port int,
	verdict string,
) bool {
	for _, decision := range decisions {
		if decision.Host == host && decision.Port == port &&
			decision.Verdict == verdict {
			return true
		}
	}
	return false
}

func proxyDecisionsIncludeRefusal(
	decisions []proxyDecisionRecord,
	host string,
) bool {
	for _, decision := range decisions {
		if decision.Host == host && decision.Verdict != "allowed" {
			return true
		}
	}
	return false
}

// proxyDecisionCarriages reports which carriages actually delivered this
// harness's own model traffic. It reads only the model origins, so the probe's
// deliberate SOCKS5 request cannot be mistaken for the harness using SOCKS.
func proxyDecisionCarriages(
	decisions []proxyDecisionRecord,
	origins []string,
) []string {
	seen := map[string]struct{}{}
	for _, decision := range decisions {
		if slices.Contains(origins, decision.Host) {
			seen[decision.Carriage] = struct{}{}
		}
	}
	carriages := make([]string, 0, len(seen))
	for carriage := range seen {
		carriages = append(carriages, carriage)
	}
	sort.Strings(carriages)
	return carriages
}

func formatProxyDecisions(decisions []proxyDecisionRecord) string {
	lines := make([]string, 0, len(decisions))
	for _, decision := range decisions {
		lines = append(lines, fmt.Sprintf("  %s %s -> %s",
			decision.Carriage, decision.Destination(), decision.Verdict))
	}
	return strings.Join(lines, "\n")
}

func shellQuoteAll(args []string) []string {
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = clcommon.ShellQuoteArg(arg)
	}
	return quoted
}
