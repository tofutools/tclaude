package testharness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/harness"
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
	// asserted alongside the rejections because a parser that refused
	// everything would pass the rejection table on its own.
	for _, pattern := range []string{"url", "url(*)", "url(example.com)", "shell(*)", "write(/tmp)"} {
		t.Run("accepts "+pattern, func(t *testing.T) {
			launch, err := ParseCopilotLaunch("copilot --deny-tool '" + pattern + "'")
			require.NoError(t, err)
			assert.Equal(t, []string{pattern}, launch.DenyTools)
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

// A URL deny is NOT read as a working deny, because the contract records that
// parse acceptance and runtime enforcement come apart for exactly those
// spellings. Modelling `url(*)` as effective would let a tclaude default that
// does nothing in production pass every test.
func TestCopilotSimWildcardURLDenyIsNotModelledAsEffective(t *testing.T) {
	sim, _ := newTrustedCopilotSim(t,
		"copilot --session-id "+copilotSimUUID+" --allow-all-tools --deny-tool 'url(*)'")
	sim.StartTurn("fetch it")
	assert.Equal(t, CopilotToolAllowed, sim.RequestTool(CopilotToolCall{
		Kind: CopilotToolShell, Command: "curl -s https://github.com",
		URL: "https://github.com"}),
		"url(*) parses and matches nothing at runtime; the simulator must not "+
			"pretend otherwise")
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
