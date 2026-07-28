package agentd

import (
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// fakeAliveTmux reports a fixed set of live tmux session names, so the label
// sequence's liveness probe is deterministic and never forks a real tmux.
type fakeAliveTmux struct{ alive map[string]struct{} }

func (f fakeAliveTmux) Command(_ ...string) *exec.Cmd { return exec.Command("true") }

func (f fakeAliveTmux) ListSessions() (map[string]struct{}, error) { return f.alive, nil }

// useFakeTmux installs a live-session snapshot for the duration of the test.
func useFakeTmux(t *testing.T, names ...string) {
	t.Helper()
	alive := map[string]struct{}{}
	for _, n := range names {
		alive[n] = struct{}{}
	}
	prev := clcommon.Default
	clcommon.Default = fakeAliveTmux{alive: alive}
	t.Cleanup(func() { clcommon.Default = prev })
	t.Cleanup(SetTmuxCacheTTLForTest(0))
}

// enableSpawnLabelFromName persists the opt-in flag into the test HOME's
// config.json; spawnLabelBase reads config live, so the next sequence sees it.
func enableSpawnLabelFromName(t *testing.T) {
	t.Helper()
	require.NoError(t, config.Save(&config.Config{
		Agent: &config.AgentConfig{SpawnLabelFromName: true},
	}), "save config")
}

// seedSession writes a session row owning a label, so the sequence's
// taken-check sees it. Status is irrelevant on purpose — see the next test.
func seedSession(t *testing.T, id, status string) {
	t.Helper()
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: id, Cwd: t.TempDir(), Status: status, CreatedAt: time.Now(),
	}), "SaveSession(%s)", id)
}

// The flag is OFF by default, so a spawn keeps minting the historical random
// "spwn-XXXXXX" label no matter what the agent is called. This is the guard
// that the feature is genuinely opt-in.
func TestSpawnLabelSequence_DefaultOffKeepsRandomLabel(t *testing.T) {
	setupTestDB(t)
	useFakeTmux(t)

	next := spawnLabelSequence("code-reviewer")
	for range 3 {
		label := next()
		assert.True(t, strings.HasPrefix(label, "spwn-"),
			"flag off must keep the random label, got %q", label)
		assert.Len(t, label, len("spwn-")+6, "random label shape")
	}
}

// With the flag on, a free name is used verbatim — the whole point of the
// feature: `tclaude session attach code-reviewer`.
func TestSpawnLabelSequence_UsesNameVerbatimWhenFree(t *testing.T) {
	setupTestDB(t)
	enableSpawnLabelFromName(t)
	useFakeTmux(t)

	assert.Equal(t, "code-reviewer", spawnLabelSequence("code-reviewer")())
}

// An unsafe name is coerced to the safe token charset before it becomes a
// label — it lands in a tmux session name and a filesystem path. This holds
// independently of agent.spawn_name_normalize, which governs only whether the
// spawn boundary rejects such a name.
func TestSpawnLabelSequence_NormalizesName(t *testing.T) {
	setupTestDB(t)
	enableSpawnLabelFromName(t)
	useFakeTmux(t)

	cases := map[string]string{
		"  code reviewer!  ": "code-reviewer",
		"[reviewer]":         "reviewer",
		"café":               "caf",
	}
	for in, want := range cases {
		assert.Equalf(t, want, spawnLabelSequence(in)(), "label for name %q", in)
	}
}

// A name with nothing safe left in it (and an empty name) falls back to the
// random token rather than producing an empty or unusable label.
func TestSpawnLabelSequence_UnusableNameFallsBackToRandom(t *testing.T) {
	setupTestDB(t)
	enableSpawnLabelFromName(t)
	useFakeTmux(t)

	for _, name := range []string{"", "   ", "###"} {
		label := spawnLabelSequence(name)()
		assert.Truef(t, strings.HasPrefix(label, "spwn-"),
			"name %q has no usable stem and must fall back to a random label, got %q", name, label)
	}
}

// A name-derived label is disambiguated the way `session new` disambiguates a
// taken tmux name: bare base first, then "-2", "-3", … (never "-1").
func TestSpawnLabelSequence_LadderSkipsTakenLabels(t *testing.T) {
	setupTestDB(t)
	enableSpawnLabelFromName(t)
	useFakeTmux(t)

	seedSession(t, "worker", "running")
	seedSession(t, "worker-2", "running")

	assert.Equal(t, "worker-3", spawnLabelSequence("worker")())
}

// A DEAD session row still blocks its label. A session id owns durable
// per-session history (session_cost_daily, telemetry checkpoints, notify
// state), so reusing an exited namesake's id would conflate two different
// agents' costs under one key — stricter than `session new`, which only
// rejects a LIVE owner because its label is not otherwise reused.
func TestSpawnLabelSequence_ExitedSessionRowStillBlocks(t *testing.T) {
	setupTestDB(t)
	enableSpawnLabelFromName(t)
	useFakeTmux(t)

	seedSession(t, "worker", "exited")

	assert.Equal(t, "worker-2", spawnLabelSequence("worker")())
}

// A live tmux session with no session row of ours still blocks the name:
// otherwise the forked `session new` would silently rename itself to
// "worker-2" (UniqueTmuxSessionName) while the daemon reported "worker".
func TestSpawnLabelSequence_LiveTmuxSessionBlocks(t *testing.T) {
	setupTestDB(t)
	enableSpawnLabelFromName(t)
	useFakeTmux(t, "worker")

	assert.Equal(t, "worker-2", spawnLabelSequence("worker")())
}

// A reserved-but-not-yet-launched spawn holds its label in pending_spawns, so
// a concurrent spawn of the same name must step past it.
func TestSpawnLabelSequence_PendingSpawnBlocks(t *testing.T) {
	setupTestDB(t)
	enableSpawnLabelFromName(t)
	useFakeTmux(t)

	require.NoError(t, db.InsertPendingSpawn(&db.PendingSpawn{Label: "worker", GroupID: 1}),
		"InsertPendingSpawn")

	assert.Equal(t, "worker-2", spawnLabelSequence("worker")())
}

// Successive calls on ONE sequence never repeat — the layered-launch path
// (reserveUniqueSpawnPrivateAttachmentRootWith) calls it in a loop and needs a
// fresh candidate each time, even though nothing has been written to the DB
// between calls.
func TestSpawnLabelSequence_SuccessiveCallsAdvance(t *testing.T) {
	setupTestDB(t)
	enableSpawnLabelFromName(t)
	useFakeTmux(t)

	next := spawnLabelSequence("worker")
	seen := map[string]bool{}
	for range 5 {
		label := next()
		require.Falsef(t, seen[label], "sequence repeated label %q", label)
		seen[label] = true
	}
	assert.True(t, seen["worker"], "the bare name should be the first candidate")
	assert.True(t, seen["worker-2"], "the ladder should follow the bare name")
}

// Two spawns of the same name can be in flight at once — the candidate is
// picked before the forked `session new` writes its row, so the DB probe alone
// would let both land on "worker" and let the loser's row overwrite the
// winner's. The process-wide reservation set closes that window.
func TestSpawnLabelSequence_InFlightLabelIsReserved(t *testing.T) {
	setupTestDB(t)
	enableSpawnLabelFromName(t)
	useFakeTmux(t)

	// Two independent sequences, as two concurrent spawn requests would build.
	// Nothing is written to the DB in between.
	assert.Equal(t, "worker", spawnLabelSequence("worker")())
	assert.Equal(t, "worker-2", spawnLabelSequence("worker")(),
		"a label already handed out to an in-flight spawn must not be reused")
}

// The reservation set is only fed by the name-derived path; with the flag off
// nothing is claimed, so the historical random-label behaviour is untouched.
func TestSpawnLabelSequence_FlagOffClaimsNothing(t *testing.T) {
	setupTestDB(t)
	useFakeTmux(t)

	require.NotEmpty(t, spawnLabelSequence("worker")(), "random label")

	enableSpawnLabelFromName(t)
	assert.Equal(t, "worker", spawnLabelSequence("worker")(),
		"the earlier flag-off spawn must not have reserved the name")
}

// The stem is truncated so the longest suffix the sequence can append still
// clears the same agent.MaxSpawnNameLen (64) gate the name itself passed —
// the label doubles as a tmux session name and a private-attachment dir.
func TestSpawnLabelSequence_LongNameLeavesRoomForSuffix(t *testing.T) {
	setupTestDB(t)
	enableSpawnLabelFromName(t)
	useFakeTmux(t)

	long := strings.Repeat("a", 64)
	label := spawnLabelSequence(long)()
	assert.Len(t, label, 64-spawnLabelSuffixBudget, "stem reserves the suffix budget")
	assert.LessOrEqual(t, len(label)+spawnLabelSuffixBudget, 64,
		"even the longest suffix must fit inside the name cap")
}
