package testharness

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/harness/copilotfixture"
)

// Tests for the Copilot simulator seam itself.
//
// A simulator that decides whether a security-relevant guard passes has to be
// tested like production code, because a permissive bug in it silently blesses
// every launch the guards were written to refuse. Each case below names the
// permission-contract entry it reproduces
// (pkg/claude/harness/copilotfixture/testdata/1.0.77/permission_contract.json),
// so a reader can check the model against the measurement rather than against
// this file's own claims.

const copilotSimUUID = "3f2a1b0c-9d8e-4f76-8a55-1c2d3e4f5a6b"

// copilotSimHome lays out a COPILOT_HOME and a cwd for a simulated pane.
func copilotSimHome(t *testing.T) (home, cwd string) {
	t.Helper()
	root := t.TempDir()
	return filepath.Join(root, "copilot-home"), t.TempDir()
}

// newTrustedCopilotSim builds a started pane whose folder trust is already
// granted, so a scenario measures the gate it means to.
func newTrustedCopilotSim(t *testing.T, cmd string) (*CopilotSim, string) {
	t.Helper()
	home, cwd := copilotSimHome(t)
	sim, err := NewCopilotSim(t, home, cwd, cmd)
	require.NoError(t, err)
	TrustCopilotFolder(t, home, cwd)
	require.NoError(t, sim.Start())
	blocked, _ := sim.Blocked()
	require.False(t, blocked, "trust was granted before launch")
	return sim, cwd
}

// The harness tag the simulator branches on must be the production one. A
// local constant that drifted would route every Copilot spawn to the Claude
// Code branch and quietly report it as covered.
func TestCopilotSimHarnessNameMatchesProduction(t *testing.T) {
	assert.Equal(t, harness.CopilotName, copilotHarnessName)
}

// The launch grammar. Each row is either a spelling the production spawner
// emits — which must parse — or one the 1.0.77 contract measured as fatal.
func TestParseCopilotLaunchGrammar(t *testing.T) {
	t.Run("spawner output parses", func(t *testing.T) {
		cmd, err := copilotBuildLaunchCommand(copilotLaunchArgs{
			SessionID:     copilotSimUUID,
			Name:          "worker",
			Model:         "claude-sonnet-4.5",
			Effort:        "high",
			InitialPrompt: "hello there",
		})
		require.NoError(t, err)
		launch, err := ParseCopilotLaunch(cmd)
		require.NoError(t, err, "cmd=%s", cmd)
		assert.Equal(t, "copilot", launch.Binary)
		assert.Equal(t, copilotSimUUID, launch.SessionID)
		assert.Equal(t, "worker", launch.Name)
		assert.Equal(t, "claude-sonnet-4.5", launch.Model)
		assert.Equal(t, "high", launch.Effort)
		assert.Equal(t, "hello there", launch.InitialPrompt)
		// The spellings copilot_spawner.go documents flag by flag. Asserting
		// them on the rendered string is what makes a silent respelling — a
		// `--resume <id>` that opens the session picker instead of resuming —
		// fail here rather than in a pane nobody is watching.
		assert.Contains(t, cmd, "--session-id "+copilotSimUUID)
		assert.Contains(t, cmd, "--model=claude-sonnet-4.5")
		assert.True(t, strings.HasSuffix(cmd, "-i 'hello there'"),
			"-i must come last so no option swallows the prompt: %s", cmd)
	})

	t.Run("resume output parses", func(t *testing.T) {
		cmd, err := copilotBuildLaunchCommand(copilotLaunchArgs{ResumeID: copilotSimUUID})
		require.NoError(t, err)
		launch, err := ParseCopilotLaunch(cmd)
		require.NoError(t, err, "cmd=%s", cmd)
		assert.Equal(t, copilotSimUUID, launch.ResumeID)
		assert.Empty(t, launch.SessionID)
		assert.Contains(t, cmd, "--resume="+copilotSimUUID)
	})

	rejected := []struct {
		name, cmd, wants string
	}{
		{"bare resume opens the picker", "copilot --resume " + copilotSimUUID, "session picker"},
		{"resume with session-id", "copilot --resume=" + copilotSimUUID +
			" --session-id " + copilotSimUUID, "cannot be combined"},
		{"non-uuid session id", "copilot --session-id worker-1", "is not a UUID"},
		{"headless -p in a pane", "copilot -p hi", "headless form"},
		{"prompt not last", "copilot -i hi --model=gpt", "must be last"},
		// The exact flag TCL-973's plan proposed as the daemon default.
		// Contract entry `url-access`: rejected at argument parse, exit 1,
		// provider never contacted — it would have killed every Copilot pane.
		{"deny-tool url() is a parse error", "copilot --deny-tool 'url()'",
			"Invalid rule format"},
		{"deny-tool bare star", "copilot --deny-tool '*'", "Invalid rule format"},
		{"unmodelled flag", "copilot --sandbox", "unmodelled argument"},
	}
	for _, tc := range rejected {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseCopilotLaunch(tc.cmd)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wants)
		})
	}

	// The spellings the same contract entry measured as ACCEPTED. They are
	// asserted alongside the rejections because a checker that refused
	// everything would pass the rejection table on its own.
	//
	// Asserted on the pattern checker rather than through the launch parser,
	// because the two answer different questions: 1.0.77 ACCEPTS all five, and
	// the launch parser separately refuses the two whose ENFORCEMENT this
	// simulator cannot reproduce (see the deny-rule subtests below).
	for _, pattern := range []string{"url", "url(*)", "url(example.com)", "shell(*)", "write(/tmp)"} {
		t.Run("1.0.77 accepts "+pattern, func(t *testing.T) {
			assert.NoError(t, copilotCheckRulePattern("--deny-tool", pattern))
		})
	}

	// And the launch parser keeps only the rules the gate model can enforce.
	// A rule it cannot evaluate must be refused, not carried and ignored: an
	// ignored deny models as ALLOWED, and for the domain-scoped URL forms the
	// contract's own evidence says the opposite.
	t.Run("keeps an enforceable shell deny", func(t *testing.T) {
		launch, err := ParseCopilotLaunch("copilot --deny-tool 'shell(echo)'")
		require.NoError(t, err)
		assert.Equal(t, []string{"shell(echo)"}, launch.DenyTools)
	})
	// A rule whose enforcement is MEASURED but not implemented here is refused
	// with a message that says so, rather than the "nobody measured it" one:
	// the evidence is committed, so the reader's next step is code, not a
	// fixture.
	for _, pattern := range []string{"url(github.com)", "write(/tmp)"} {
		t.Run("refuses the measured-but-unimplemented "+pattern, func(t *testing.T) {
			_, err := ParseCopilotLaunch("copilot --deny-tool '" + pattern + "'")
			require.Error(t, err)
			assert.Contains(t, err.Error(), "MEASURED BUT NOT IMPLEMENTED")
		})
	}
}

// Contract entry `ambient-allow-all-env`: parsing is strict, case-sensitive
// equality against the literal "true". Anything else behaves as unset, which
// is why EnvExports must UNSET the variable rather than pin it to a falsy
// value — a future widening of the value parse would defeat the pin.
func TestParseCopilotLaunchAmbientAllowAll(t *testing.T) {
	for value, want := range map[string]bool{
		"true": true, "TRUE": false, "1": false, "false": false, "": false,
	} {
		launch, err := ParseCopilotLaunch(
			"export " + CopilotAllowAllEnvVar + "='" + value + "'; copilot")
		require.NoError(t, err)
		assert.Equalf(t, want, launch.AmbientAllowAll(), "COPILOT_ALLOW_ALL=%q", value)
	}
}

// Contract entry `folder-trust`, the finding that reshapes TCL-973: with a
// fresh COPILOT_HOME the trust dialog is the FIRST gate, no launch flag clears
// it, and the pane parks alive and silent. A detached agent therefore cannot
// be produced by rendering argv alone.
func TestCopilotSimFolderTrustBlocksEveryLaunchFlag(t *testing.T) {
	for _, flags := range []string{
		"", " --allow-all-tools", " --allow-all", " --allow-all-paths",
	} {
		t.Run("flags:"+flags, func(t *testing.T) {
			home, cwd := copilotSimHome(t)
			sim, err := NewCopilotSim(t, home, cwd, "copilot --session-id "+copilotSimUUID+flags)
			require.NoError(t, err)
			require.NoError(t, sim.Start())

			blocked, reason := sim.Blocked()
			assert.True(t, blocked, "no launch flag clears the folder-trust gate")
			assert.Contains(t, reason, "Confirm folder trust")
			assert.True(t, sim.IsAlive(),
				"the pane is ALIVE and will never do anything: that is the deadlock")
			// Nothing was written, because the gate precedes the provider
			// connection entirely.
			assert.NoFileExists(t,
				filepath.Join(home, "session-state", copilotSimUUID, "workspace.yaml"))
		})
	}

	t.Run("add-dir does not clear it either", func(t *testing.T) {
		home, cwd := copilotSimHome(t)
		sim, err := NewCopilotSim(t, home, cwd,
			"copilot --session-id "+copilotSimUUID+" --add-dir "+cwd)
		require.NoError(t, err)
		require.NoError(t, sim.Start())
		blocked, _ := sim.Blocked()
		assert.True(t, blocked)
	})

	t.Run("a pre-launch config write clears it", func(t *testing.T) {
		home, cwd := copilotSimHome(t)
		sim, err := NewCopilotSim(t, home, cwd, "copilot --session-id "+copilotSimUUID)
		require.NoError(t, err)
		TrustCopilotFolder(t, home, cwd)
		require.NoError(t, sim.Start())
		blocked, _ := sim.Blocked()
		assert.False(t, blocked)
		assert.FileExists(t,
			filepath.Join(home, "session-state", copilotSimUUID, "workspace.yaml"))
	})

	t.Run("the ambient variable clears it, which is the hazard", func(t *testing.T) {
		home, cwd := copilotSimHome(t)
		sim, err := NewCopilotSim(t, home, cwd,
			"export "+CopilotAllowAllEnvVar+"=true; copilot --session-id "+copilotSimUUID)
		require.NoError(t, err)
		require.NoError(t, sim.Start())
		blocked, _ := sim.Blocked()
		assert.False(t, blocked,
			"COPILOT_ALLOW_ALL is strictly stronger than the flag it documents: "+
				"it clears trust as well, promoting the session with no record")
	})
}

// Contract entry `default-interactive-blocking`, plus the property the whole
// simulator exists for: a blocked pane emits NO Stop. tclaude's status machine
// returns an agent to idle on Stop, so a simulator that emitted one from a
// parked pane would report a permanently deadlocked agent as free.
func TestCopilotSimToolApprovalGate(t *testing.T) {
	t.Run("an unsafe command blocks with no flags", func(t *testing.T) {
		sim, _ := newTrustedCopilotSim(t, "copilot --session-id "+copilotSimUUID)
		sim.StartTurn("delete the file")
		got := sim.RequestTool(CopilotToolCall{
			Kind: CopilotToolShell, Command: "rm -f ./victim"})
		assert.Equal(t, CopilotToolBlocked, got)

		blocked, reason := sim.Blocked()
		assert.True(t, blocked)
		assert.Contains(t, reason, "rm -f ./victim")
		sim.FinishTurn()
		assert.True(t, sim.IsAlive(), "a blocked pane stays alive forever")
	})

	t.Run("--allow-all-tools completes the same call", func(t *testing.T) {
		sim, _ := newTrustedCopilotSim(t,
			"copilot --session-id "+copilotSimUUID+" --allow-all-tools")
		sim.StartTurn("delete the file")
		got := sim.RequestTool(CopilotToolCall{
			Kind: CopilotToolShell, Command: "rm -f ./victim"})
		assert.Equal(t, CopilotToolAllowed, got)
		blocked, _ := sim.Blocked()
		assert.False(t, blocked)
	})

	// The scope limit the contract states in its own finding: Copilot
	// auto-approves commands it classifies as trivially safe, `echo` was
	// measured doing so, and the allowlist is NOT enumerated. The simulator
	// therefore makes this a fact the caller asserts about its own scripted
	// command rather than a table it invents.
	t.Run("a caller-asserted safe command runs with no flags", func(t *testing.T) {
		sim, _ := newTrustedCopilotSim(t, "copilot --session-id "+copilotSimUUID)
		sim.StartTurn("say hi")
		got := sim.RequestTool(CopilotToolCall{
			Kind: CopilotToolShell, Command: "echo hi", AutoApproved: true})
		assert.Equal(t, CopilotToolAllowed, got)
	})
}

// Contract entry `no-ask-user`: the tool is advertised by default and the flag
// removes it. Present with nobody attached, it is the second deadlock source.
func TestCopilotSimAskUserGate(t *testing.T) {
	sim, _ := newTrustedCopilotSim(t,
		"copilot --session-id "+copilotSimUUID+" --allow-all-tools")
	sim.StartTurn("what should I do?")
	assert.Equal(t, CopilotToolBlocked, sim.RequestTool(CopilotToolCall{Kind: CopilotToolAskUser}),
		"--allow-all-tools does not close the ask_user deadlock; only --no-ask-user does")
}

// Contract entry `out-of-cwd-paths`. The internal control from the measurement
// is reproduced: the SAME call is allowed inside a granted root and blocked
// outside it, so the block is path-driven rather than command-risk-driven.
func TestCopilotSimPathGate(t *testing.T) {
	// The tool axis is opened so only the path axis can block, and the temp
	// grant is dropped so the cases below cannot pass by accident: a test
	// directory may or may not sit under the system temp dir depending on how
	// the runner sets TMPDIR and GOTMPDIR, and a scenario whose result flips
	// with the environment proves nothing about Copilot. The temp grant gets
	// its own subtest against os.TempDir() directly.
	base := "copilot --session-id " + copilotSimUUID + " --allow-all-tools --disallow-temp-dir"

	t.Run("inside cwd is granted", func(t *testing.T) {
		sim, cwd := newTrustedCopilotSim(t, base)
		sim.StartTurn("read it")
		assert.Equal(t, CopilotToolAllowed, sim.RequestTool(CopilotToolCall{
			Kind: CopilotToolShell, Command: "cat f", Path: filepath.Join(cwd, "f")}))
	})

	t.Run("outside every root blocks", func(t *testing.T) {
		sim, _ := newTrustedCopilotSim(t, base)
		outside := filepath.Join(t.TempDir(), "elsewhere", "secret")
		sim.StartTurn("read it")
		assert.Equal(t, CopilotToolBlocked, sim.RequestTool(CopilotToolCall{
			Kind: CopilotToolShell, Command: "cat secret", Path: outside}))
		_, reason := sim.Blocked()
		assert.Contains(t, reason, "Allow directory access")
	})

	t.Run("--add-dir grants it", func(t *testing.T) {
		outside := t.TempDir()
		sim, _ := newTrustedCopilotSim(t, base+" --add-dir "+outside)
		sim.StartTurn("read it")
		assert.Equal(t, CopilotToolAllowed, sim.RequestTool(CopilotToolCall{
			Kind: CopilotToolShell, Command: "cat secret",
			Path: filepath.Join(outside, "secret")}))
	})

	// The system temp dir is granted by default and --disallow-temp-dir
	// removes that grant. The same call either side of the flag is the
	// measurement's own internal control.
	tempPath := filepath.Join(os.TempDir(), "copilot-sim-scratch")
	t.Run("temp is granted by default", func(t *testing.T) {
		sim, _ := newTrustedCopilotSim(t,
			"copilot --session-id "+copilotSimUUID+" --allow-all-tools")
		sim.StartTurn("read it")
		assert.Equal(t, CopilotToolAllowed, sim.RequestTool(CopilotToolCall{
			Kind: CopilotToolShell, Command: "cat scratch", Path: tempPath}))
	})

	t.Run("--disallow-temp-dir removes it", func(t *testing.T) {
		sim, _ := newTrustedCopilotSim(t, base)
		sim.StartTurn("read it")
		assert.Equal(t, CopilotToolBlocked, sim.RequestTool(CopilotToolCall{
			Kind: CopilotToolShell, Command: "cat scratch", Path: tempPath}))
	})
}

// Contract entry `url-access`, which corrected the plan in the plan's own
// favour: the URL dialog is real and distinct, and --allow-all-tools closes
// it, so the shell path needs no URL deny at all.
func TestCopilotSimURLGateClosesWithAllowAllTools(t *testing.T) {
	t.Run("no flags blocks", func(t *testing.T) {
		sim, _ := newTrustedCopilotSim(t, "copilot --session-id "+copilotSimUUID)
		sim.StartTurn("fetch it")
		assert.Equal(t, CopilotToolBlocked, sim.RequestTool(CopilotToolCall{
			Kind: CopilotToolShell, Command: "curl -s https://github.com",
			URL: "https://github.com"}))
		_, reason := sim.Blocked()
		assert.Contains(t, reason, "attempting to access the following URL")
	})

	t.Run("--allow-all-tools runs it through", func(t *testing.T) {
		sim, _ := newTrustedCopilotSim(t,
			"copilot --session-id "+copilotSimUUID+" --allow-all-tools")
		sim.StartTurn("fetch it")
		assert.Equal(t, CopilotToolAllowed, sim.RequestTool(CopilotToolCall{
			Kind: CopilotToolShell, Command: "curl -s https://github.com",
			URL: "https://github.com"}))
	})
}

// Contract entry `in-pane-allow-all-override`, DISPROVEN in the favourable
// direction: in-pane /allow-all cannot override a launch-time deny. Denial
// precedence holds at runtime, so a launch-time --deny-tool is a real boundary
// rather than a starting posture.
//
// The command choice is load-bearing and copied from the measurement: `echo`
// is auto-approved when no rule mentions it, so a refusal can only come from
// the deny rule.
func TestCopilotSimLaunchDenySurvivesInPaneAllowAll(t *testing.T) {
	sim, _ := newTrustedCopilotSim(t,
		"copilot --session-id "+copilotSimUUID+" --deny-tool 'shell(echo)'")
	sim.Receive("/allow-all")
	sim.Receive("Enter")

	sim.StartTurn("say hi")
	got := sim.RequestTool(CopilotToolCall{
		Kind: CopilotToolShell, Command: "echo hi", AutoApproved: true})
	assert.Equal(t, CopilotToolDenied, got,
		"a launch-time deny survives the in-pane widening")
	blocked, _ := sim.Blocked()
	assert.False(t, blocked,
		"a denial is not a deadlock: the model gets an answer and the turn continues")
}

// The URL deny rules, per SPELLING, as entry `web-fetch-url-access` measured
// them. This is the one place the contract's own warning — parse acceptance is
// not enforcement — has teeth, because the two spellings below parse
// identically and behave oppositely.
func TestCopilotSimURLDenyRulesPerSpelling(t *testing.T) {
	// The BARE KIND is a working blanket deny at the permission layer, and it
	// beats a launch-time --allow-all-tools. A denial is not a deadlock: the
	// model is told, and the turn continues.
	t.Run("bare url denies even under --allow-all-tools", func(t *testing.T) {
		sim, _ := newTrustedCopilotSim(t,
			"copilot --session-id "+copilotSimUUID+" --allow-all-tools --deny-tool url")
		sim.StartTurn("fetch it")
		assert.Equal(t, CopilotToolDenied, sim.RequestTool(CopilotToolCall{
			Kind: CopilotToolWebFetch, URL: "https://probe.invalid/x"}))
		blocked, _ := sim.Blocked()
		assert.False(t, blocked, "a denial lets the turn continue")
	})

	// The wildcard form parses and then matches NOTHING, falling through to the
	// network layer. Modelling it as a working deny would let a tclaude default
	// that does nothing in production look effective in every test — so the
	// simulator must show the URL going through.
	t.Run("url(*) is inert", func(t *testing.T) {
		sim, _ := newTrustedCopilotSim(t,
			"copilot --session-id "+copilotSimUUID+" --allow-all-tools --deny-tool 'url(*)'")
		sim.StartTurn("fetch it")
		assert.Equal(t, CopilotToolAllowed, sim.RequestTool(CopilotToolCall{
			Kind: CopilotToolWebFetch, URL: "https://probe.invalid/x"}))
	})
}

// web_fetch is the THIRD independent deadlock source, and the last one to be
// measured. Entry `web-fetch-url-access` had to drop COPILOT_OFFLINE — which
// removes the tool from the catalog entirely — and replace that hermeticity
// with an egress wall in order to ask the question at all.
func TestCopilotSimWebFetchGate(t *testing.T) {
	call := CopilotToolCall{Kind: CopilotToolWebFetch, URL: "https://probe.invalid/x"}

	t.Run("blocks with no flags", func(t *testing.T) {
		sim, _ := newTrustedCopilotSim(t, "copilot --session-id "+copilotSimUUID)
		sim.StartTurn("fetch it")
		assert.Equal(t, CopilotToolBlocked, sim.RequestTool(call))
		_, reason := sim.Blocked()
		assert.Contains(t, reason, "attempting to access the following URL")
		sim.FinishTurn()
		assert.True(t, sim.IsAlive(), "a parked pane never ends its turn")
	})

	// Both closers were measured independently. --allow-all-urls closing it on
	// its own is what proves the prompt is a URL decision rather than ordinary
	// tool approval.
	for _, flags := range []string{"--allow-all-tools", "--allow-all-tools --no-ask-user", "--allow-all-urls"} {
		t.Run("closed by "+flags, func(t *testing.T) {
			sim, _ := newTrustedCopilotSim(t,
				"copilot --session-id "+copilotSimUUID+" "+flags)
			sim.StartTurn("fetch it")
			assert.Equal(t, CopilotToolAllowed, sim.RequestTool(call))
		})
	}

	// But --allow-all-urls was measured against web_fetch ONLY. The shell path
	// is a different URL consumer, and the two agreeing about --allow-all-tools
	// does not license generalising the other flag across them — so a shell
	// call reaching a URL under that flag must FAIL the test rather than be
	// answered either way.
	//
	// Driven as a real call, not asserted on parsed fields. A field-only
	// version of this subtest passed while the shell gate was mutated to honour
	// --allow-all-urls, which is the definition of a hollow test: it named the
	// right property and could not observe it.
	t.Run("does not license the shell path", func(t *testing.T) {
		sim, rec := newRecordingCopilotSim(t,
			"copilot --session-id "+copilotSimUUID+" --allow-all-urls")
		sim.StartTurn("fetch it")
		sim.RequestTool(CopilotToolCall{
			Kind: CopilotToolShell, Command: "curl -s https://github.com",
			URL: "https://github.com", AutoApproved: true})

		require.NotEmpty(t, rec.fatals,
			"a shell URL call under --allow-all-urls must fail loudly: the flag was "+
				"measured against web_fetch, and nothing establishes it reaches this gate")
		assert.Contains(t, rec.fatals[0], "--allow-all-urls")
		assert.Contains(t, rec.fatals[0], "WEB-FETCH gate only")
	})
}

// The blanket-allow widenings the contract never measured against a given
// gate. Each must fail the test rather than silently opening or closing it —
// this is the mechanism that stops the simulator from answering a question the
// fixtures never asked, so it needs coverage of its own.
func TestCopilotSimUnmeasuredWideningFailsLoudly(t *testing.T) {
	risky := CopilotToolCall{Kind: CopilotToolShell, Command: "rm -f ./victim"}
	for _, tc := range []struct {
		name, flags, wants string
		call               CopilotToolCall
	}{
		{name: "--allow-all on the tool gate", flags: "--allow-all",
			wants: "--allow-all/--yolo", call: risky},
		{name: "--yolo on the tool gate", flags: "--yolo",
			wants: "--allow-all/--yolo", call: risky},
		{name: "ambient promotion on the path gate",
			flags: "--allow-all-tools --disallow-temp-dir", wants: "COPILOT_ALLOW_ALL",
			call: CopilotToolCall{Kind: CopilotToolShell, Command: "cat x",
				Path: filepath.Join(os.TempDir(), "outside-every-grant")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := "copilot --session-id " + copilotSimUUID + " " + tc.flags
			if tc.wants == "COPILOT_ALLOW_ALL" {
				cmd = "export " + CopilotAllowAllEnvVar + "=true; " + cmd
			}
			sim, rec := newRecordingCopilotSim(t, cmd)
			sim.StartTurn("do it")
			sim.RequestTool(tc.call)
			require.NotEmptyf(t, rec.fatals, "the widening guard must fire for %s", tc.flags)
			assert.Contains(t, rec.fatals[0], tc.wants)
		})
	}
}

// In-pane /allow-all after a launch-time URL deny: entry
// `web-fetch-url-access` establishes launch-time precedence only and says
// explicitly that surviving post-launch widening is NOT measured on the URL
// axis. A blanket deny a pane can widen away is a different product from one it
// cannot, so the simulator refuses to say which this is.
func TestCopilotSimURLDenyUnderInPaneWideningFailsLoudly(t *testing.T) {
	sim, rec := newRecordingCopilotSim(t,
		"copilot --session-id "+copilotSimUUID+" --allow-all-tools --deny-tool url")
	sim.Receive("/allow-all")
	sim.Receive("Enter")
	sim.StartTurn("fetch it")
	sim.RequestTool(CopilotToolCall{Kind: CopilotToolWebFetch, URL: "https://probe.invalid/x"})

	require.NotEmpty(t, rec.fatals)
	assert.Contains(t, rec.fatals[0], "NOT measured")
}

// recordingT stands in for *testing.T so a guard's Fatalf can be OBSERVED
// rather than aborting the test that is checking it fires.
type recordingT struct {
	*testing.T
	fatals []string
}

func (r *recordingT) Fatalf(format string, args ...any) {
	r.fatals = append(r.fatals, fmt.Sprintf(format, args...))
}

func (r *recordingT) Errorf(format string, args ...any) {
	r.fatals = append(r.fatals, fmt.Sprintf(format, args...))
}

// newRecordingCopilotSim builds a started, trusted pane whose failures are
// captured instead of fatal.
func newRecordingCopilotSim(t *testing.T, cmd string) (*CopilotSim, *recordingT) {
	t.Helper()
	home, cwd := copilotSimHome(t)
	rec := &recordingT{T: t}
	sim, err := newCopilotSim(rec,
		copilotfixture.LoadHookCapture(t, copilotfixture.HookScenarioClaudeDialect),
		home, cwd, cmd)
	require.NoError(t, err)
	TrustCopilotFolder(t, home, cwd)
	require.NoError(t, sim.Start())
	require.Empty(t, rec.fatals, "the launch itself must not have failed")
	return sim, rec
}

// The session-state files production reads: workspace.yaml carries the launch
// name as an operator title, and /rename rewrites it.
func TestCopilotSimWritesSessionState(t *testing.T) {
	home, cwd := copilotSimHome(t)
	cmd, err := copilotBuildLaunchCommand(copilotLaunchArgs{
		SessionID: copilotSimUUID, Name: "worker", Model: "claude-sonnet-4.5"})
	require.NoError(t, err)
	sim, err := NewCopilotSim(t, home, cwd, cmd)
	require.NoError(t, err)
	TrustCopilotFolder(t, home, cwd)
	require.NoError(t, sim.Start())

	workspace := filepath.Join(home, "session-state", copilotSimUUID, "workspace.yaml")
	body := readFileString(t, workspace)
	assert.Contains(t, body, "id: "+copilotSimUUID)
	assert.Contains(t, body, "cwd: "+cwd)
	assert.Contains(t, body, "name: worker")
	assert.Contains(t, body, "user_named: true",
		"a launch --name is an operator title, not a generated summary")

	sim.Receive("/rename renamed-worker")
	sim.Receive("Enter")
	assert.Contains(t, readFileString(t, workspace), "name: renamed-worker")

	events := readFileString(t,
		filepath.Join(home, "session-state", copilotSimUUID, "events.jsonl"))
	assert.Contains(t, events, `"type":"session.start"`)
	assert.Contains(t, events, `"selectedModel":"claude-sonnet-4.5"`)
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(raw)
}

// A pane parked on a dialog SWALLOWS everything tclaude types at it. The modal
// owns the keyboard, so none of the lifecycle commands reach the session — and
// a simulator that let them through would let a deadlocked pane rename its
// conversation, append to an event log that has no session.start, and answer a
// soft-stop with a clean SessionEnd. The last one is the dangerous one: a
// future "detect and recover a deadlocked pane" guard would test as working
// while doing nothing at all.
func TestCopilotSimParkedPaneSwallowsInPaneCommands(t *testing.T) {
	home, cwd := copilotSimHome(t)
	sim, err := NewCopilotSim(t, home, cwd, "copilot --session-id "+copilotSimUUID)
	require.NoError(t, err)
	require.NoError(t, sim.Start())
	blocked, _ := sim.Blocked()
	require.True(t, blocked, "a fresh COPILOT_HOME parks the pane")

	for _, line := range []string{"/rename renamed-while-parked", "/compact", "/exit"} {
		sim.Receive(line)
		sim.Receive("Enter")
	}

	assert.True(t, sim.IsAlive(), "/exit typed into a modal does not exit the CLI")
	stateDir := filepath.Join(home, "session-state", copilotSimUUID)
	assert.NoFileExists(t, filepath.Join(stateDir, "workspace.yaml"),
		"/rename must not materialise the session state the trust gate prevents")
	assert.NoFileExists(t, filepath.Join(stateDir, "events.jsonl"),
		"/compact must not append to a log the pane never opened")
}

// A relaunch is a NEW PROCESS, so it re-evaluates the gates. Without this, a
// conversation that parked once would report deadlocked forever after its trust
// was seeded — which is precisely the transition directory trust exists to
// make, so a simulator that could not express it would make the fix untestable.
func TestCopilotSimRelaunchAfterSeedingClearsTheBlock(t *testing.T) {
	home, cwd := copilotSimHome(t)
	cmd := "copilot --session-id " + copilotSimUUID
	sim, err := NewCopilotSim(t, home, cwd, cmd)
	require.NoError(t, err)
	require.NoError(t, sim.Start())
	blocked, _ := sim.Blocked()
	require.True(t, blocked)

	// Seed trust and relaunch the same pane, as the resume branch does.
	TrustCopilotFolder(t, home, cwd)
	sim.Shutdown()
	require.NoError(t, sim.Start())

	blocked, reason := sim.Blocked()
	assert.Falsef(t, blocked, "the relaunch must re-evaluate the gate: %s", reason)

	// And the log it writes is a FIRST launch, not a resume: the parked process
	// never reached the provider, so it never wrote a session.start for a
	// session.resume to follow.
	events := readFileString(t, filepath.Join(home, "session-state", copilotSimUUID, "events.jsonl"))
	assert.Contains(t, events, `"type":"session.start"`)
	assert.NotContains(t, events, `"type":"session.resume"`)
}

// `--allow-all` / `--yolo` must NOT be read as tool auto-approval. The contract
// measured them only against the folder-trust modal, where they do nothing;
// their effect on tool approval, paths and URLs was never measured, and the
// flag names are exactly the kind of thing that invites an assumption.
func TestParseCopilotLaunchBlanketAllowIsNotToolApproval(t *testing.T) {
	for _, flag := range []string{"--allow-all", "--yolo"} {
		launch, err := ParseCopilotLaunch("copilot " + flag)
		require.NoError(t, err)
		assert.True(t, launch.BlanketAllow, "%s should be recorded", flag)
		assert.False(t, launch.AllowAllTools,
			"%s was never measured against a tool call", flag)
		assert.False(t, launch.ToolsAutoApproved(),
			"%s must not be mistaken for --allow-all-tools", flag)
	}
}

// Permission flags whose EFFECT nothing measured are refused at parse rather
// than parsed and then ignored. `--deny-url` is the sharpest: the contract
// reports domain-scoped denies as ENFORCED with no prompt, so a simulator that
// accepted and ignored one would model "allowed" exactly where reality denies.
func TestParseCopilotLaunchRefusesUnmodelledPermissionFlags(t *testing.T) {
	for _, arg := range []string{
		"--allow-tool 'shell(rm)'", "--allow-url github.com",
		"--mode plan", "--plan", "--autopilot",
	} {
		t.Run(arg, func(t *testing.T) {
			_, err := ParseCopilotLaunch("copilot " + arg)
			require.Error(t, err, "an unmodelled permission flag must not parse silently")
			assert.Contains(t, err.Error(), "UNMODELLED")
		})
	}

	// And the flags whose behaviour IS measured but which this simulator does
	// not implement are refused with the other message, so a reader is sent to
	// the code rather than to a fixture that already exists.
	for _, arg := range []string{"--deny-url github.com", "--excluded-tools web_fetch"} {
		t.Run(arg, func(t *testing.T) {
			_, err := ParseCopilotLaunch("copilot " + arg)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "MEASURED BUT NOT IMPLEMENTED")
		})
	}
}

// The trust store is JSONC and the CLI writes a managed-file header into it.
// A reader that could not cope would report a correctly-seeded launch as
// parked; the contract's folder-trust entry flags this explicitly.
func TestCopilotSimReadsAJSONCTrustStore(t *testing.T) {
	home, cwd := copilotSimHome(t)
	require.NoError(t, os.MkdirAll(home, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.json"), []byte(
		"// This file is automatically managed.\n"+
			"// Do not edit it by hand.\n"+
			`{"trustedFolders":["`+cwd+`"],"firstLaunchAt":"2026-01-01T00:00:00Z"}`+"\n"), 0o600))

	sim, err := NewCopilotSim(t, home, cwd, "copilot --session-id "+copilotSimUUID)
	require.NoError(t, err)
	require.NoError(t, sim.Start())
	blocked, reason := sim.Blocked()
	assert.Falsef(t, blocked, "a JSONC trust store must still be readable: %s", reason)
}

// Seeding a second directory must not un-trust the first. TrustCopilotFolder
// delegates to production's read-modify-write editor precisely so two agents
// spawned into different directories compose instead of racing.
func TestCopilotSimTrustSeedingComposes(t *testing.T) {
	home, first := copilotSimHome(t)
	second := t.TempDir()
	TrustCopilotFolder(t, home, first)
	TrustCopilotFolder(t, home, second)

	for _, dir := range []string{first, second} {
		sim, err := NewCopilotSim(t, home, dir, "copilot --session-id "+copilotSimUUID)
		require.NoError(t, err)
		require.NoError(t, sim.Start())
		blocked, _ := sim.Blocked()
		assert.Falsef(t, blocked, "%s should still be trusted after the other seeding", dir)
		sim.Shutdown()
	}
}
