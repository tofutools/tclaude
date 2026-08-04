package agentd_test

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/claude/worktree"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Daemon-spawned Copilot panes, driven through the production spawn, resume and
// status paths against a simulator that reproduces the 1.0.77 permission
// measurements committed in PR #1936.
//
// These scenarios exist because the failure they are about is invisible. A
// Copilot pane that hits a permission dialog is running, healthy, and answering
// tmux — and will never do anything again. Nothing in the daemon can tell that
// apart from an agent that is thinking, so the only way to keep a regression
// out is to make the deadlock reachable in a test and then assert it does not
// happen.
//
// The simulator is launched from the REAL spawner's output
// (harness.copilotSpawner.BuildCommand), so a flag respelling fails here rather
// than in production. What is NOT production is the SpawnArgs → SpawnSpec
// mapping in testharness/copilot_sim.go: production assembles that spec deep
// inside session/new.go, so a field production threads and the mapping does not
// would be invisible to these tests.

// newCopilotFlow is newFlow plus the dashboard's popup base URL, which the
// snapshot endpoint's origin check requires before a test may read it.
func newCopilotFlow(t *testing.T) *testharness.Flow {
	t.Helper()
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	return newFlow(t)
}

// spawnCopilot launches a Copilot agent into a group through the production
// spawn API and returns the response plus the pane simulator behind it.
func spawnCopilot(t *testing.T, f *testharness.Flow, group string, body map[string]any) (
	testharness.SpawnResp, *testharness.CopilotSim,
) {
	t.Helper()
	body["harness"] = harness.CopilotName
	resp := f.AsHuman().SpawnWith(group, body)
	require.Equalf(t, http.StatusOK, resp.Code, "copilot spawn body=%s", resp.Raw)
	sim := f.World.Copilots.GetByConvID(resp.ConvID)
	require.NotNil(t, sim, "the spawn should have built a Copilot pane simulator")
	return resp, sim
}

// copilotLaunchOf parses the launch string the production spawner produced.
func copilotLaunchOf(t *testing.T, f *testharness.Flow, convID string) testharness.CopilotLaunch {
	t.Helper()
	cmd, ok := f.World.CopilotLaunchCommand(convID)
	require.Truef(t, ok, "no Copilot launch recorded for %s", convID)
	launch, err := testharness.ParseCopilotLaunch(cmd)
	require.NoErrorf(t, err, "the production spawner produced a launch the CLI would "+
		"reject: %s", cmd)
	return launch
}

// TestCopilotSpawn_LaunchEnrollmentIdentity: the launch-enrollment contract for
// the second harness that has one.
//
// Copilot's conv-id is knowable before the pane starts, so the daemon presets
// it and carries the agent's name and briefing in the launch argv rather than
// typing them into the pane afterwards. Every assertion here is on a spelling
// whose alternative is a silent identity bug: a `--resume <id>` that opens the
// session picker, a `--session-id` that is not a UUID (Copilot creates a
// session for an unmatched id ONLY when it is one), or a `-i` that is not last
// and has its prompt swallowed by a neighbouring option.
func TestCopilotSpawn_LaunchEnrollmentIdentity(t *testing.T) {
	f := newCopilotFlow(t)
	f.HaveGroup("crew")

	const brief = "Investigate the flaky deploy job and report back"
	resp, sim := spawnCopilot(t, f, "crew", map[string]any{
		"trust_dir":       true,
		"name":            "copilot-worker",
		"initial_message": brief,
		"model":           "claude-sonnet-4.5",
	})

	// The daemon knew the id before launch: the row carries the preset id, and
	// the pane was launched under exactly it.
	row, err := db.LoadSession(resp.Label)
	require.NoError(t, err)
	require.NotNil(t, row)
	assert.Equal(t, resp.ConvID, row.ConvID)
	assert.Equal(t, harness.CopilotName, row.Harness)

	launch := copilotLaunchOf(t, f, resp.ConvID)
	assert.Equal(t, resp.ConvID, launch.SessionID,
		"the pane must be launched under the enrolled conversation id")
	assert.Empty(t, launch.ResumeID, "a fresh launch presets an id, it does not resume one")
	assert.Equal(t, "copilot-worker", launch.Name)
	assert.Equal(t, "claude-sonnet-4.5", launch.Model)
	assert.Contains(t, launch.InitialPrompt, "Investigate the flaky deploy job",
		"the briefing rides the launch argv rather than being typed into the pane")

	// The pane agrees: it created its session-state under the enrolled id, and
	// Copilot's own ConvStore — the production cold-read path — resolves the
	// launch name as an operator title.
	assert.Equal(t, resp.ConvID, sim.ConvID)
	copilotHarness, err := harness.Resolve(harness.CopilotName)
	require.NoError(t, err)
	title, err := copilotHarness.Convs.Title(resp.ConvID)
	require.NoError(t, err)
	assert.Equal(t, "copilot-worker", title)

	// Nothing was injected over tmux. That is the whole point of carrying the
	// name and briefing in the argv, and for Copilot it matters more than for
	// Claude Code: a pane parked on a permission dialog would swallow injected
	// text with no error anywhere.
	assert.Empty(t, f.World.Tmux.Sent(),
		"a launch-enrolled Copilot spawn must not send-keys")
}

// TestCopilotSpawn_UntrustedFolderParksThePaneSilently is the negative half of
// the directory-trust contract, and it stays a LIVE assertion rather than a
// leftover now that seeding exists.
//
// `--trust-dir` is opt-in and is never auto-defaulted — editing a config file
// tclaude does not own is a side effect the operator has to ask for — with one
// exception the daemon verifies itself (the sibling-worktree case below). So a
// spawn that does not opt in still reaches this state, and an operator who
// leaves it off needs to see it plainly.
//
// Contract entry `folder-trust`: with a fresh COPILOT_HOME the trust dialog is
// the FIRST gate, before the provider is contacted at all, and no launch flag
// clears it — --allow-all-tools, --allow-all, --allow-all-paths and --add-dir
// were each measured still blocking with zero provider requests. The result is
// a pane that is alive, enrolled, group-visible and permanently inert.
func TestCopilotSpawn_UntrustedFolderParksThePaneSilently(t *testing.T) {
	f := newCopilotFlow(t)
	// No trust_dir: this is what an un-opted-in spawn really does, not a fault
	// injected by the test.
	f.HaveGroup("crew")

	resp, sim := spawnCopilot(t, f, "crew", map[string]any{
		"name":            "copilot-worker",
		"initial_message": "get started",
	})

	blocked, reason := sim.Blocked()
	require.True(t, blocked, "a fresh COPILOT_HOME parks the pane at the trust dialog")
	assert.Contains(t, reason, "Confirm folder trust")
	assert.True(t, sim.IsAlive(),
		"the parked pane is ALIVE — which is exactly why nothing notices")

	// No hook ever fires, because the gate precedes the provider connection:
	// the launch prompt was accepted by the argv and swallowed by the dialog.
	member := copilotMember(t, f, "crew", resp.ConvID)
	assert.Equal(t, "running", member.State.Status,
		"the agent shows only its process status: no live status will ever arrive")

	// Nothing was written under COPILOT_HOME for the conversation, so even the
	// cold read path has nothing to report.
	assert.NoFileExists(t, filepath.Join(
		testharness.CopilotHomeFor(f.World.HomeDir), "session-state", resp.ConvID,
		"workspace.yaml"))

	// And the pane genuinely swallows what tclaude types at it. A modal owns
	// the keyboard, so the daemon's own rename delivery reaches nothing — which
	// is the property that makes a parked pane indistinguishable from a busy
	// one, and the reason a future blocked-state detector cannot be built on
	// "did the injection succeed".
	f.AsHuman().Rename(resp.ConvID, "renamed-while-parked")
	copilotHarness, err := harness.Resolve(harness.CopilotName)
	require.NoError(t, err)
	title, err := copilotHarness.Convs.Title(resp.ConvID)
	require.NoError(t, err)
	assert.Empty(t, title,
		"a rename typed into a trust modal must not reach the conversation")
}

// TestCopilotSpawn_TrustDirSeedsTheStoreSoThePaneRuns is the positive half:
// the same spawn, opted into pre-trust, starts clean.
//
// The seeding runs through PRODUCTION's editor (harness.EnsureDirTrustedForLaunch
// → EnsureCopilotDirTrustedForLaunch), so this asserts the real write, not a
// test-side imitation of it.
//
// What it does NOT assert, since the flow world's COPILOT_HOME is the ambient
// $HOME/.copilot and the launch-aware and ambient spellings therefore coincide:
// that seeding follows a RELOCATED COPILOT_HOME. That property — the one that
// makes Copilot's store different from the other two harnesses', whose stores
// sit at a fixed path in the operator's home — is covered at unit level in
// pkg/testharness, where the simulator's home deliberately is not the default.
//
// Nor does it cover production's CALL SITE. The simulated spawner stands in for
// `tclaude session new` wholesale, so deleting the seeding block in
// session/new.go would leave these tests green; what is exercised here is the
// editor and the daemon's TrustDir resolution.
func TestCopilotSpawn_TrustDirSeedsTheStoreSoThePaneRuns(t *testing.T) {
	f := newCopilotFlow(t)
	f.HaveGroup("crew")

	resp, sim := spawnCopilot(t, f, "crew", map[string]any{
		"trust_dir":       true,
		"name":            "copilot-worker",
		"initial_message": "get started",
	})

	blocked, reason := sim.Blocked()
	require.Falsef(t, blocked, "a pre-trusted launch must not park: %s", reason)

	// The file production wrote really names this launch's cwd, under the home
	// this launch reads.
	row, err := db.LoadSession(resp.Label)
	require.NoError(t, err)
	require.NotNil(t, row)
	config, err := os.ReadFile(filepath.Join(
		testharness.CopilotHomeFor(f.World.HomeDir), harness.CopilotConfigFileName))
	require.NoError(t, err, "the trust store must exist after an opted-in launch")
	assert.Contains(t, string(config), row.Cwd,
		"trustedFolders must carry the launch cwd")

	// And the pane got past the gate: the launch-arg prompt started a turn, so
	// hooks are flowing and the agent reports a live status.
	assert.Equal(t, session.StatusWorking,
		copilotMember(t, f, "crew", resp.ConvID).State.Status)
	assert.FileExists(t, filepath.Join(
		testharness.CopilotHomeFor(f.World.HomeDir), "session-state", resp.ConvID,
		"workspace.yaml"))
}

// TestCopilotSpawn_DefaultSiblingWorktreeIsAutoTrusted covers the one path that
// pre-trusts WITHOUT anyone opting in, through the real resolver
// (defaultSiblingWorktreeTrust → harness.IsDefaultSiblingWorktree) rather than
// a stand-in.
//
// The exemption exists because a worktree child is the unattended case: tclaude
// created the checkout itself and can verify its shape (../<repo>-<branch>), so
// requiring a human to also tick a trust box would mean every worktree child
// stops on the modal and its parent waits forever for an agent that never
// started.
//
// NOTE on the caller: this spawns as the HUMAN. The agent-caller variant — the
// one the Codex and Claude Code suites cover — cannot be written for Copilot
// yet, because Copilot has no arm in normalizeSpawnLineageSandbox
// (spawn_sandbox_guard.go) or in classifyApprovalLineage, so ANY agent-parent →
// Copilot-child spawn is refused with sandbox_restricted before it reaches the
// trust logic at all. That lineage gap belongs to the approval/lineage change,
// not here; when it lands, this test should gain the agentReqProof variant.
func TestCopilotSpawn_DefaultSiblingWorktreeIsAutoTrusted(t *testing.T) {
	f := newCopilotFlow(t)
	f.HaveGroup("crew")

	repo, _ := initRepoOnMain(t)
	worktreeDir, err := worktree.AddWorktreeIn(repo, "agent-child", "main", "")
	require.NoError(t, err)

	// No trust_dir field anywhere in this request.
	resp, sim := spawnCopilot(t, f, "crew", map[string]any{
		"name": "copilot-sibling",
		"cwd":  worktreeDir,
	})

	trusted, ok := f.World.SpawnTrustDir(resp.ConvID)
	require.True(t, ok)
	assert.True(t, trusted,
		"a verified default sibling worktree must be pre-trusted with no opt-in")

	// The consequence, which is the part worth asserting for Copilot: the pane
	// actually started instead of parking on the modal.
	blocked, reason := sim.Blocked()
	assert.Falsef(t, blocked, "an auto-trusted worktree child must not park: %s", reason)
	assert.Equal(t, session.StatusWorking,
		copilotMember(t, f, "crew", resp.ConvID).State.Status)
}

// TestCopilotSpawn_RenderedPostureDecidesWhetherTheAgentDeadlocks is the
// regression test the approval work has to pass.
//
// It runs the SAME scripted turn twice through the production status path: once
// under the posture the daemon renders today, and once under a launch that
// resolves to the measured nonblocking posture. The difference is not cosmetic
// — one agent returns to idle and the other is reported "working" forever,
// which is the state every coordination decision in tclaude is built on.
func TestCopilotSpawn_RenderedPostureDecidesWhetherTheAgentDeadlocks(t *testing.T) {
	t.Run("today's rendered posture blocks on the first tool call", func(t *testing.T) {
		f := newCopilotFlow(t)
		f.HaveGroup("crew")

		resp, sim := spawnCopilot(t, f, "crew", map[string]any{
			"trust_dir":       true,
			"name":            "copilot-worker",
			"initial_message": "clean up the build",
		})

		// The launch-arg prompt started a turn: hooks fired in Copilot's own
		// order (UserPromptSubmit BEFORE SessionStart) and the agent is working.
		assert.Equal(t, session.StatusWorking, copilotMember(t, f, "crew", resp.ConvID).State.Status)

		launch := copilotLaunchOf(t, f, resp.ConvID)
		// The characterization half. Today the Copilot descriptor leaves
		// Approval nil, so ResolveApprovalPolicy yields "" and the spawner
		// emits no permission flags at all. When the approval catalog lands,
		// this assertion is the one to flip — and the rest of this subtest
		// stops being reachable from a daemon spawn, which is the point.
		require.False(t, launch.ToolsAutoApproved(),
			"UPDATE ME when the Copilot ApprovalCatalog lands: the daemon default "+
				"must render a posture that auto-approves tools, at which point this "+
				"subtest's deadlock becomes unreachable from a spawn")

		got := sim.RequestTool(testharness.CopilotToolCall{
			Kind: testharness.CopilotToolShell, Command: "rm -rf ./build"})
		assert.Equal(t, testharness.CopilotToolBlocked, got)

		// The turn cannot end, so the agent never comes back. This is the
		// deadlock, seen from the surface an operator and every coordinating
		// agent actually reads.
		sim.FinishTurn()
		assert.Equal(t, session.StatusWorking,
			copilotMember(t, f, "crew", resp.ConvID).State.Status,
			"a pane parked on a permission dialog reports as busy forever")
		assert.True(t, sim.IsAlive())
	})

	t.Run("the measured nonblocking posture completes the turn", func(t *testing.T) {
		f := newCopilotFlow(t)
		f.HaveGroup("crew")

		resp, _ := spawnCopilot(t, f, "crew", map[string]any{
			"trust_dir":       true,
			"name":            "copilot-worker",
			"initial_message": "clean up the build",
		})
		// Relaunch the same conversation under the posture the contract
		// measured as nonblocking. Substituting the pane rather than the
		// spawner keeps this PR free of the approval production change while
		// still proving, through the daemon's own status path, what that change
		// has to achieve.
		sim := copilotRelaunchWithPosture(t, f, resp, "--allow-all-tools --no-ask-user")

		// No "is it working" assertion before the tool call: the original
		// pane's launch prompt already put this agent in `working`, so it would
		// hold whether or not the relaunch did anything. The Stop-driven return
		// to idle below is the assertion that can only pass if the turn really
		// completed.
		sim.StartTurn("clean up the build")
		assert.Equal(t, testharness.CopilotToolAllowed, sim.RequestTool(testharness.CopilotToolCall{
			Kind: testharness.CopilotToolShell, Command: "rm -rf ./build"}))
		sim.FinishTurn()
		assert.Equal(t, session.StatusIdle,
			copilotMember(t, f, "crew", resp.ConvID).State.Status,
			"Stop returns the agent to idle, which a blocked pane can never do")
	})
}

// TestCopilotSpawn_ResumeKeepsConversationIdentity drives the production resume
// path and checks the two ways a Copilot relaunch can silently lose an agent.
//
// Contract entry `resume-submits-prompt` established that `-i` with
// `--resume=<full-id>` submits into the RESUMED conversation and the
// session-state directory keeps its original UUID. The risk it left standing is
// the id itself: `--resume` also accepts prefixes and session NAMES, so
// anything short of the full UUID can attach a relaunch to a different
// conversation.
func TestCopilotSpawn_ResumeKeepsConversationIdentity(t *testing.T) {
	f := newCopilotFlow(t)
	f.HaveGroup("crew")

	resp, sim := spawnCopilot(t, f, "crew", map[string]any{
		"trust_dir":       true,
		"name":            "copilot-worker",
		"initial_message": "start work",
	})
	sim.FinishTurn()

	stateDir := filepath.Join(testharness.CopilotHomeFor(f.World.HomeDir),
		"session-state", resp.ConvID)
	require.FileExists(t, filepath.Join(stateDir, "events.jsonl"))

	f.AsHuman().Stop(resp.ConvID, false)
	resume := f.Resume(resp.ConvID)
	require.Equalf(t, http.StatusOK, resume.Code, "resume body=%s", resume.Raw)

	relaunch := copilotLaunchOf(t, f, resp.ConvID)
	assert.Equal(t, resp.ConvID, relaunch.ResumeID,
		"a relaunch must name the FULL conversation id: --resume also matches "+
			"prefixes and session names, either of which can attach to a different "+
			"conversation")
	assert.Empty(t, relaunch.SessionID,
		"--resume and --session-id are documented as mutually exclusive")

	// The conversation did not fork: the resume appended to the SAME event log
	// under the SAME directory, and Copilot's ConvStore still reports one.
	events, err := os.ReadFile(filepath.Join(stateDir, "events.jsonl"))
	require.NoError(t, err)
	assert.Contains(t, string(events), `"type":"session.resume"`,
		"a resume APPENDS to the existing log rather than starting a new one")
	assert.Equal(t, 1, strings.Count(string(events), `"type":"session.start"`))

	copilotHarness, err := harness.Resolve(harness.CopilotName)
	require.NoError(t, err)
	entries, err := copilotHarness.Convs.ListConvs("")
	require.NoError(t, err)
	var seen int
	for _, e := range entries {
		if e.SessionID == resp.ConvID {
			seen++
		}
	}
	assert.Equal(t, 1, seen, "the relaunch must not create a second conversation")
}

// copilotRelaunchWithPosture replaces the registered pane with one launched
// under an explicit permission posture, keeping the conversation's identity and
// the daemon's session row.
//
// It exists so this PR can prove what the approval work must achieve without
// containing any of that work's production logic. The launch string is still
// rendered by the production spawner; only the extra flags are appended here.
func copilotRelaunchWithPosture(t *testing.T, f *testharness.Flow,
	resp testharness.SpawnResp, flags string,
) *testharness.CopilotSim {
	t.Helper()
	cmd, ok := f.World.CopilotLaunchCommand(resp.ConvID)
	require.True(t, ok)
	// Append before the trailing `-i <prompt>`, which must stay last.
	idx := strings.LastIndex(cmd, " -i ")
	require.Positive(t, idx, "the launch should carry an -i prompt: %s", cmd)
	posture := cmd[:idx] + " " + flags + cmd[idx:]

	home := testharness.CopilotHomeFor(f.World.HomeDir)
	row, err := db.LoadSession(resp.Label)
	require.NoError(t, err)
	require.NotNil(t, row)
	sim, err := testharness.NewCopilotSim(t, home, row.Cwd, posture)
	require.NoError(t, err)
	sim.SetSessionID(resp.Label)
	testharness.TrustCopilotFolder(t, home, row.Cwd)
	require.NoError(t, sim.Start())
	f.World.Tmux.Register(resp.Label, row.Cwd, sim)
	f.World.Copilots.Set(resp.Label, sim)
	return sim
}

// copilotMember reads one agent's dashboard row — the surface an operator and
// every coordinating agent actually see.
func copilotMember(t *testing.T, f *testharness.Flow, group, convID string) *dashMember {
	t.Helper()
	_ = f
	m := findDashMember(fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest()), group, convID)
	require.NotNilf(t, m, "agent %s missing from group %s", convID, group)
	return m
}
