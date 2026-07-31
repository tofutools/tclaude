//go:build linux

package agentd

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/sandboxproxy"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// TestOpenCodeUnixRelayDeploysTheProxyEngine is the structural half of TCL-891,
// and it replaces TestOpenCodeUnixRelayRefusesTheProxyEngine (TCL-889), which
// pinned the refusal this PR lifts.
//
// The fact it pins is the one OpenCode's proxy-engine capability cells will
// eventually rest on: agentd launches OpenCode through the Unix-relay server
// boundary, and that boundary now DEPLOYS the proxy engine — its own
// supervisor, its own fd layout — rather than refusing it because the
// inherited-descriptor contract was written against the packet gateway's.
//
// WHAT THIS DOES NOT DO, because a reader of the lift will wonder: it does not
// activate anything. The cells stay EnforceNone in this tree, and
// TestOpenCodeProxyEngineCellsStayUnenforced below still asserts so — for a
// different reason than before. A launch seam that can deploy an engine is a
// precondition for rating it, not evidence about it; the evidence is the floor
// smoke, and the activation record is made in the PR that cites its green run.
//
// It is pinned here, on the OpenCode path, because the session-package test of
// the same seam (TestTclaudeLayerUnixRelayDeploysTheProxyEngine) builds its
// spec with HarnessName "claude". That proves the renderer deploys; it does not
// prove that the spec OpenCode's OWN builder produces reaches the deployment,
// which is the fact everything downstream rests on.
//
// The launch path under test is production's: openCodeServeProcessExec is the
// function that renders a Unix-relay OpenCode server launch. Only the
// bubblewrap host probe is stubbed — the smoke gate for THAT is the executor
// smoke; what this test must not depend on is whether the machine running it
// happens to have bwrap.
//
// Falsifiability: restore either refusal in sandbox_bwrap_linux.go /
// sandbox_bwrap.go and this test reports an error instead of a rendered argv;
// render the packet policy flag or the packet fd layout for this plan and the
// discriminating assertions below fail.
func TestOpenCodeUnixRelayDeploysTheProxyEngine(t *testing.T) {
	// Own HOME and XDG_DATA_HOME before touching the database or allocating
	// state: both are read out of the environment, and a test that allocated
	// under the developer's real home would write outside its own tree.
	// A SHORT temp root under /tmp, not t.TempDir and not $TMPDIR: the v4
	// control socket path is built under this tree, and the spec builder
	// validates it against the Linux sockaddr limit BEFORE it reaches the
	// engine question. A long temp root therefore fails this test with
	// "control path exceeds Linux sockaddr capacity" — a fabricated failure
	// that says nothing about the refusal under test.
	home, err := os.MkdirTemp("/tmp", "ocp-*")
	require.NoError(t, err)
	home, err = filepath.EvalSymlinks(home)
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	// The isolated boundary binds the canonical agentd socket, and the argv
	// renderer refuses a spec whose socket is absent. Bind a real one so the
	// renderer reaches the engine question it is being asked about.
	socketPath := filepath.Join(home, ".tclaude", "api", "agentd.sock")
	require.NoError(t, os.MkdirAll(filepath.Dir(socketPath), 0o700))
	socket, err := net.Listen("unix", socketPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = socket.Close() })
	t.Setenv(agentipc.SocketEnv, socketPath)
	db.ResetForTest()
	t.Cleanup(db.ResetForTest)

	cwd := t.TempDir()
	agentID := db.NewAgentID()
	_, err = allocatePrivateOpenCodeState(agentID)
	require.NoError(t, err)

	snapshot := openCodeProxyEngineSnapshot()
	// The filtered builder is the one production reaches for a rules-carrying
	// profile; the engine rides the same snapshot into the spec.
	spec, err := buildOpenCodeTclaudeLayerLaunchSpec(
		cwd, nil, &snapshot, agentID, true, true)
	require.NoError(t, err)
	require.NotNil(t, spec)
	require.Equal(t, sandboxpolicy.NetworkEngineProxy, spec.Contract.NetworkEngine,
		"the spec under test must actually carry the proxy engine")

	previousResolve := resolveOpenCodeTclaudeLayer
	resolveOpenCodeTclaudeLayer = func(
		sandboxpolicy.NetworkPosture,
		sandboxpolicy.RootPosture,
		sandboxpolicy.NetworkEngine,
	) (string, harness.LaunchOSSandbox, error) {
		return "/usr/bin/bwrap", harness.LaunchOSSandbox{}, nil
	}
	t.Cleanup(func() { resolveOpenCodeTclaudeLayer = previousResolve })

	runtime := &db.OpenCodeRuntime{
		SessionID: "opencode-proxy-engine-deployment",
		Transport: db.OpenCodeTransportUnixRelay,
	}
	_, launcherArgs, _, _, cleanup, err := openCodeServeProcessExec(
		"/usr/bin/opencode", "41999", runtime, spec)
	cleanup()
	require.NoError(t, err,
		"a proxy-engine OpenCode launch must render, not refuse")

	// The discriminating facts, not merely "it rendered". The supervisor this
	// launch starts must be the PROXY one, and it must never be handed the
	// packet gateway's policy: a launch that started the packet supervisor for
	// a proxy plan would build a floor with pasta and nft prerequisites it does
	// not have, and would enforce a policy the running engine never compiled.
	assert.Contains(t, launcherArgs, "--proxy-network-policy")
	assert.NotContains(t, launcherArgs, "--filtered-network-policy")

	// Both renderers the launch calls agree about the same layout, not just
	// whichever one runs first. Keeping them in step is what stops a launch
	// from telling the in-sandbox relay to name descriptors the supervisor
	// installed somewhere else — the failure the old refusal existed to avoid,
	// which the lift has to keep avoiding rather than merely stop refusing.
	listenerFD, executableFD, fdErr := session.TclaudeLayerUnixRelayServerFDs(*spec)
	require.NoError(t, fdErr)
	assert.Equal(t, 6, listenerFD,
		"the proxy engine contributes two sealed descriptors, so the launcher's pair starts at 6")
	assert.Equal(t, 7, executableFD)
	execArgs, argvErr := session.TclaudeLayerUnixRelayServerExecArgs(
		"/usr/bin/bwrap", *spec, 2, []string{"/usr/bin/opencode", "serve"})
	require.NoError(t, argvErr)
	assert.Contains(t, execArgs, "--proxy-network-policy")
	// The relay command production hands the sandbox names the fds reported
	// above. Asserting the rendered argv carries them is what ties the accessor
	// to the launch rather than leaving them two independently-correct numbers.
	assert.Contains(t, launcherArgs,
		"/proc/self/fd/"+strconv.Itoa(executableFD),
		"the inherited relay must name the executable descriptor the accessor reports")
	assert.Contains(t, launcherArgs, strconv.Itoa(listenerFD),
		"the inherited relay must name the listener descriptor the accessor reports")
}

// TestOpenCodeProxyEngineCellsFollowTheActivationRecord replaces
// TestOpenCodeProxyEngineCellsStayUnenforced, and it moves in the same commit
// as the record it follows — which is the rule, not a convenience.
//
// The history in one line each, because the reason has now changed twice and a
// reader deserves to know which one is current:
//
//   - before TCL-891, unenforced because the launch seam REFUSED this engine
//     and there was nothing to rate;
//   - during TCL-891's first PR, unenforced because the seam could deploy it
//     but no ACTIVATION RECORD had been made;
//   - now, enforced because the record exists, made from the green named run
//     of the floor smoke that PR added.
//
// ON SHOWING THAT THE PRE-FLIP VALUE DIFFERS, and where that is honestly
// possible. The concern is real: an assertion that only checks the enforced
// value would pass unchanged on a tree where the ratings branch had been made
// unconditional — the same shape as the deny-name over-claim TCL-884 caught.
//
// The tempting in-test comparison is the Darwin row, and it is a TRAP. Darwin's
// cells are EnforceNone for a reason that DOMINATES the record lookup (the
// proxy-cells branch is not reached there at all), so requiring linux != darwin
// would pass whether or not the record gates anything, and would advertise a
// guarantee this test does not have. Verified rather than assumed: with the
// platform gate removed from proxyEngineActivated, the Darwin rating is still
// EnforceNone.
//
// So the coupling is pinned where it is real — on the record and the predicate,
// in TestProxyEngineActivationIsScopedToItsEvidence, whose two halves are each
// falsifiability-verified — and the revert check for THIS test was done by hand
// and recorded in the PR: with the OpenCode row removed from the record, every
// assertion below fails. The Darwin read is kept, but only for the claim it can
// actually support: that the not-activated sentence still reaches an operator
// somewhere.
//
// Ratings are asked of the REAL evaluator in BOTH baselines below rather than
// read off the activation map, because the map having a row and the enforcement
// actually being deliverable are two different facts, and only the second is
// what an operator relies on.
//
// Platform is passed explicitly rather than taken from runtime.GOOS so each
// assertion states which platform it is about — GOOS=darwin go vet cannot see
// into a runtime.GOOS branch (TCL-884 handoff §3.4).
func TestOpenCodeProxyEngineCellsFollowTheActivationRecord(t *testing.T) {
	snapshot := openCodeProxyEngineSnapshot()
	axes, err := sandboxpolicy.PlannedEffectiveAccessAxes(snapshot.Effective)
	require.NoError(t, err)

	predicted, err := harness.PredictAccessEnforcement(
		harness.MustGet(harness.OpenCodeName),
		sandboxpolicy.ImplementationTclaudeLayer, axes, "", "linux",
	)
	require.NoError(t, err)
	assert.Equal(t, harness.EnforceFull, predicted.NetworkList,
		"OpenCode's proxy-engine allow cells are backed by a green named smoke")
	assert.Equal(t, harness.EnforceFull, predicted.NetworkPorts)
	assert.NotEmpty(t, predicted.NetworkSelectors)
	assert.Contains(t,
		harness.ProxyEngineActivationSmokes(harness.OpenCodeName),
		"TestOpenCodeProxyFloorCooperation",
		"the row must name the smoke that actually measured OpenCode behind the floor")

	// Darwin, read for the ONE claim it can support: the not-activated sentence
	// is still reachable for this harness somewhere, so the assertion below
	// that it has retired on Linux is a change rather than a disappearance.
	// Deliberately NOT used as proof that the rating is gated on evidence — see
	// the header.
	unactivated, err := harness.PredictAccessEnforcement(
		harness.MustGet(harness.OpenCodeName),
		sandboxpolicy.ImplementationTclaudeLayer, axes, "", "darwin",
	)
	require.NoError(t, err)
	assert.Equal(t, harness.EnforceNone, unactivated.NetworkList)

	// §5.3, both halves. The not-activated sentence retires; the carriage
	// sentence stays, because activation changes what is ENFORCED and not what
	// the engine CARRIES.
	assert.NotContains(t, predicted.NetworkEngineDetail,
		harness.ProxyEngineNotActivatedNotice,
		"an activated harness must not still say its cells are unactivated")
	assert.Contains(t, predicted.NetworkEngineDetail,
		harness.ProxyEngineCarriageNotice)
	assert.Contains(t, unactivated.NetworkEngineDetail,
		harness.ProxyEngineNotActivatedNotice,
		"the unactivated platform must still disclose that it is not activated")

	// The per-harness carriage fact this ticket turns on, and it is asserted
	// HERE rather than only in the harness package because this is the OpenCode
	// launch path's own test: OpenCode 1.18.6 carries over HTTP CONNECT and
	// ignores ALL_PROXY, so SOCKS-dependent destinations are BLOCKED by the
	// floor rather than filtered by the policy. Saying so is what stops the
	// activated row reading as a broader claim than the measurement supports.
	assert.Contains(t, predicted.NetworkEngineDetail,
		harness.ProxyEngineOpenCodeCarriageNotice)
}

// TestOpenCodeProxyEngineRatingsMatchTheEvaluator asserts the flipped cells
// against the REAL evaluator — the same one the running proxy compiles — in
// BOTH baselines, per the standard adopted after TCL-884's over-claim.
//
// The over-claim survived review because every proxy assertion ran against a
// `mode: list` policy, the shape where the rating is least wrong. Asking both
// baselines is what makes a rating an observation of the mechanism rather than
// a second opinion about it.
func TestOpenCodeProxyEngineRatingsMatchTheEvaluator(t *testing.T) {
	const denied = "denied.example.com"
	const deniedAddr = "93.184.216.34"

	for _, baseline := range []struct {
		name  string
		rules sandboxpolicy.NetworkRules
	}{
		{
			// Open baseline: the literal is simply allowed, so a name deny is
			// escaped by asking for the address.
			name: "open",
			rules: sandboxpolicy.NetworkRules{
				Mode:   sandboxpolicy.AccessModeOpen,
				Deny:   []sandboxpolicy.NetworkAllowEntry{{Host: denied}},
				Engine: sandboxpolicy.NetworkEngineProxy,
			},
		},
		{
			// Allowlist baseline with a covering cidr row: the literal is
			// allowed by that row, so the deny is escaped with no DNS trickery
			// at all.
			name: "list",
			rules: sandboxpolicy.NetworkRules{
				Mode: sandboxpolicy.AccessModeList,
				Allow: []sandboxpolicy.NetworkAllowEntry{
					{CIDR: "93.184.216.0/24", Ports: []int{443}},
				},
				Deny:   []sandboxpolicy.NetworkAllowEntry{{Host: denied}},
				Engine: sandboxpolicy.NetworkEngineProxy,
			},
		},
	} {
		t.Run(baseline.name, func(t *testing.T) {
			evaluator, err := sandboxproxy.NewEvaluator(baseline.rules)
			require.NoError(t, err)

			byName, err := sandboxproxy.ParseTarget(denied, 443)
			require.NoError(t, err)
			require.False(t, evaluator.Evaluate(byName).Allowed(),
				"the deny must hold for the name it was authored against")
			byLiteral, err := sandboxproxy.ParseTarget(deniedAddr, 443)
			require.NoError(t, err)
			escaped := evaluator.Evaluate(byLiteral).Allowed()

			predicted, err := harness.PredictAccessEnforcement(
				harness.MustGet(harness.OpenCodeName),
				sandboxpolicy.ImplementationTclaudeLayer,
				sandboxpolicy.ResolvedAxes{Network: baseline.rules}, "", "linux",
			)
			require.NoError(t, err)
			capability, ok := openCodeNetworkDenySelector(
				predicted, sandboxpolicy.NetworkSelectorHost)
			require.True(t, ok)
			if escaped {
				assert.NotEqual(t, harness.EnforceFull, capability.Level,
					"the evaluator carried %s:443 past a deny on %s, so OpenCode's cell must not claim full enforcement",
					deniedAddr, denied)
				assert.NotEmpty(t, capability.Detail,
					"a partial rating must say what it does not cover")
			}

			// Deny loopback is Full and CORRECT, evaluator-asserted since
			// TCL-888 and deliberately not touched here: every loopback
			// spelling is folded into one identity before matching, so there is
			// no by-address restatement to walk past the deny.
			loopbackRules := baseline.rules
			loopbackRules.Deny = []sandboxpolicy.NetworkAllowEntry{{Loopback: true}}
			loopbackEvaluator, err := sandboxproxy.NewEvaluator(loopbackRules)
			require.NoError(t, err)
			for _, spelling := range []string{"127.0.0.1", "localhost", "::1"} {
				target, parseErr := sandboxproxy.ParseTarget(spelling, 443)
				require.NoError(t, parseErr)
				assert.Falsef(t, loopbackEvaluator.Evaluate(target).Allowed(),
					"loopback spelling %s must not escape a loopback deny", spelling)
			}
			loopbackPredicted, err := harness.PredictAccessEnforcement(
				harness.MustGet(harness.OpenCodeName),
				sandboxpolicy.ImplementationTclaudeLayer,
				sandboxpolicy.ResolvedAxes{Network: loopbackRules}, "", "linux",
			)
			require.NoError(t, err)
			loopback, ok := openCodeNetworkDenySelector(
				loopbackPredicted, sandboxpolicy.NetworkSelectorLoopback)
			require.True(t, ok)
			assert.Equal(t, harness.EnforceFull, loopback.Level,
				"no loopback spelling escapes the deny, so Full is the honest rating")
		})
	}
}

func openCodeNetworkDenySelector(
	predicted harness.PredictedAccessEnforcement,
	selector sandboxpolicy.NetworkSelectorKind,
) (harness.NetworkSelectorCapability, bool) {
	for _, capability := range predicted.NetworkDenySelectors {
		if capability.Selector == string(selector) {
			return capability, true
		}
	}
	return harness.NetworkSelectorCapability{}, false
}

// openCodeProxyEngineSnapshot authors the one profile both tests above are
// about: a discriminating rule set that selects the proxy engine explicitly.
func openCodeProxyEngineSnapshot() sandboxpolicy.Snapshot {
	snapshot := sandboxpolicy.EmptySnapshot()
	snapshot.Effective.Network = &sandboxpolicy.NetworkRules{
		Mode:   sandboxpolicy.AccessModeList,
		Engine: sandboxpolicy.NetworkEngineProxy,
		Allow: []sandboxpolicy.NetworkAllowEntry{
			{Domain: "example.com", Ports: []int{443}},
		},
	}
	return snapshot
}

// TestOpenCodeLayerResolveProbesTheFloorTheLaunchBuilds pins the half of the
// lift that is easiest to get subtly wrong: which engine the host-capability
// probe is asked about.
//
// The launch contract carries the AUTHORED engine, and the plan deploys the
// RESOLVED one. They are the same for a discriminating policy and DIFFERENT for
// a non-discriminating one — a filtered posture whose allow rows the proxy
// engine cannot discriminate on deploys no engine at all, even though the
// profile authored `engine: proxy`. Probing the contract's answer there would
// verify the proxy engine's floor (bubblewrap and pidfds) for a launch that is
// about to build the packet gateway's (pasta, nft, a user namespace), and the
// launch would then fail deep inside the supervisor rather than at the
// prerequisite check that exists to catch it.
//
// Falsifiability: pass spec.Contract.NetworkEngine instead of the deployed
// engine at either resolve site and the non-discriminating case below reports
// the isolated floor.
func TestOpenCodeLayerResolveProbesTheFloorTheLaunchBuilds(t *testing.T) {
	for _, tc := range []struct {
		name        string
		rules       sandboxpolicy.NetworkRules
		wantEngine  sandboxpolicy.NetworkEngine
		wantPosture sandboxpolicy.NetworkPosture
	}{
		{
			name: "discriminating-proxy",
			rules: sandboxpolicy.NetworkRules{
				Mode:   sandboxpolicy.AccessModeList,
				Engine: sandboxpolicy.NetworkEngineProxy,
				Allow: []sandboxpolicy.NetworkAllowEntry{
					{Domain: "example.com", Ports: []int{443}},
				},
			},
			wantEngine: sandboxpolicy.NetworkEngineProxy,
			// The proxy engine's floor IS the isolated posture's construction.
			wantPosture: sandboxpolicy.NetworkIsolatedWithAgentd,
		},
		{
			name: "authored-proxy-but-non-discriminating",
			rules: sandboxpolicy.NetworkRules{
				Mode:   sandboxpolicy.AccessModeList,
				Engine: sandboxpolicy.NetworkEngineProxy,
				Allow: []sandboxpolicy.NetworkAllowEntry{
					{Loopback: true, Ports: []int{8080}},
				},
			},
			// No engine deploys, so the packet gateway's floor is what gets
			// built and what must be probed.
			wantEngine:  sandboxpolicy.NetworkEngineUnset,
			wantPosture: sandboxpolicy.NetworkFiltered,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, err := os.MkdirTemp("/tmp", "ocr-*")
			require.NoError(t, err)
			home, err = filepath.EvalSymlinks(home)
			require.NoError(t, err)
			t.Cleanup(func() { _ = os.RemoveAll(home) })
			t.Setenv("HOME", home)
			t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
			socketPath := filepath.Join(home, ".tclaude", "api", "agentd.sock")
			require.NoError(t, os.MkdirAll(filepath.Dir(socketPath), 0o700))
			socket, err := net.Listen("unix", socketPath)
			require.NoError(t, err)
			t.Cleanup(func() { _ = socket.Close() })
			t.Setenv(agentipc.SocketEnv, socketPath)
			db.ResetForTest()
			t.Cleanup(db.ResetForTest)

			snapshot := sandboxpolicy.EmptySnapshot()
			rules := tc.rules
			snapshot.Effective.Network = &rules
			agentID := db.NewAgentID()
			_, err = allocatePrivateOpenCodeState(agentID)
			require.NoError(t, err)
			spec, err := buildOpenCodeTclaudeLayerLaunchSpec(
				t.TempDir(), nil, &snapshot, agentID, true, true)
			require.NoError(t, err)
			require.NotNil(t, spec)

			var probedPostures []sandboxpolicy.NetworkPosture
			var probedEngines []sandboxpolicy.NetworkEngine
			previousResolve := resolveOpenCodeTclaudeLayer
			resolveOpenCodeTclaudeLayer = func(
				posture sandboxpolicy.NetworkPosture,
				root sandboxpolicy.RootPosture,
				engine sandboxpolicy.NetworkEngine,
			) (string, harness.LaunchOSSandbox, error) {
				probedEngines = append(probedEngines, engine)
				// The floor mapping is production's, applied here rather than
				// re-implemented, so this records the posture that would
				// actually have been verified.
				probedPostures = append(probedPostures,
					session.TclaudeLayerFloorPosture(posture, engine))
				return "/usr/bin/bwrap", harness.LaunchOSSandbox{}, nil
			}
			t.Cleanup(func() { resolveOpenCodeTclaudeLayer = previousResolve })

			runtime := &db.OpenCodeRuntime{
				SessionID: "opencode-resolve-" + tc.name,
				Transport: db.OpenCodeTransportUnixRelay,
			}
			_, _, _, _, cleanup, err := openCodeServeProcessExec(
				"/usr/bin/opencode", "41998", runtime, spec)
			cleanup()
			require.NoError(t, err)
			require.NotEmpty(t, probedEngines,
				"the launch must verify the host before rendering")
			assert.Equal(t, tc.wantEngine, probedEngines[0],
				"the probe must follow the DEPLOYED engine, not the authored one")
			assert.Equal(t, tc.wantPosture, probedPostures[0],
				"the probe must verify the floor this launch actually builds")
		})
	}
}
