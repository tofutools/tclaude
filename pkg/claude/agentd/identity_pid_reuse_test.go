package agentd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// TCL-771. The DIRECT CLI identity path — convIDForPID steps 2 and 3, which
// run before the tclaude-layer walk TCL-761 repaired — resolved a reused pid
// with the plain "most recently updated row wins" query. A pid is not unique
// over a machine's lifetime and session rows are never pruned, so a long-dead
// session's row can shadow a live agent's pane pid; the answer becomes
// peer.ConvID, which classify() turns into classAgent, so the caller is
// authorized as whichever conversation won that guess.
//
// These drive the resolver and the production whoami handler, not the repair
// helper, so they pin what a CLI caller is actually told it is.

const (
	pidReuseCLIPeerPID  = 6101 // the `tclaude agent` CLI process over the socket
	pidReuseCLIHarness  = 6050 // the harness ancestor the walk stops at
	pidReuseCLISharedSh = 6040 // the pane sh — one pid, two rows

	pidReuseCLILiveConv = "6a000000-0000-4000-8000-000000000001"
	pidReuseCLIDeadConv = "6d000000-0000-4000-8000-000000000002"

	pidReuseCLILiveLabel = "spwn-cli-pidreuse-live"
	pidReuseCLIDeadLabel = "spwn-cli-pidreuse-dead"
)

// stubTmux reports a fixed alive set and never runs a real tmux.
type stubTmux struct{ alive map[string]struct{} }

func (s stubTmux) Command(...string) *exec.Cmd { return exec.Command("true") }

func (s stubTmux) ListSessions() (map[string]struct{}, error) { return s.alive, nil }

// haveLiveTmuxSessions installs the alive set the liveness probe reads, with
// the shared cache made transparent so each test sees its own fixture.
func haveLiveTmuxSessions(t *testing.T, names ...string) {
	t.Helper()
	alive := map[string]struct{}{}
	for _, n := range names {
		alive[n] = struct{}{}
	}
	prev := clcommon.Default
	clcommon.Default = stubTmux{alive: alive}
	t.Cleanup(func() { clcommon.Default = prev })
	t.Cleanup(SetTmuxCacheTTLForTest(0))
}

// stampSessionUpdatedAt pins a row's updated_at so the ORDER BY the resolver
// reads is deterministic: SaveSession always stamps time.Now(), and two saves
// in one test can land close enough that RFC3339Nano's variable-width fraction
// does not sort the way wall-clock did.
func stampSessionUpdatedAt(t *testing.T, sessionID string, at time.Time) {
	t.Helper()
	handle, err := db.Open()
	require.NoError(t, err)
	res, err := handle.Exec(`UPDATE sessions SET updated_at = ? WHERE id = ?`,
		at.UTC().Truncate(time.Second).Format(time.RFC3339Nano), sessionID)
	require.NoError(t, err)
	affected, err := res.RowsAffected()
	require.NoError(t, err)
	require.Equal(t, int64(1), affected, "expected to stamp exactly one row (%s)", sessionID)
}

// haveCLIPidReuseRows stands up the collision: a live agent and a dead
// session sharing a pane pid, with the DEAD one updated more recently — the
// shape that makes the plain query pick the corpse. The proc tree is the
// pane-sh one (the harness runs as the recorded pane pid's direct child), so
// resolution goes through convIDForPID step 3.
func haveCLIPidReuseRows(t *testing.T, liveTmux, deadTmux string) {
	t.Helper()
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: pidReuseCLILiveLabel, PID: pidReuseCLISharedSh, ConvID: pidReuseCLILiveConv,
		TmuxSession: liveTmux, Harness: "claude", Status: "working",
	}))
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: pidReuseCLIDeadLabel, PID: pidReuseCLISharedSh, ConvID: pidReuseCLIDeadConv,
		TmuxSession: deadTmux, Harness: "claude", Status: "working",
	}))
	now := time.Now()
	stampSessionUpdatedAt(t, pidReuseCLILiveLabel, now.Add(-2*time.Minute))
	stampSessionUpdatedAt(t, pidReuseCLIDeadLabel, now.Add(-1*time.Minute))

	fakeProcTree{
		name: map[int]string{
			pidReuseCLIPeerPID:  "tclaude",
			pidReuseCLIHarness:  "node",
			pidReuseCLISharedSh: "sh",
		},
		parent: map[int]int{
			pidReuseCLIPeerPID: pidReuseCLIHarness,
			pidReuseCLIHarness: pidReuseCLISharedSh,
		},
	}.install(t)
}

// TestConvIDForPID_LiveRowWinsAReusedPidOnDirectCLIIdentity is the headline
// fix: with a corpse shadowing the live agent's pane pid, the CLI caller must
// be identified as ITSELF — not as the conversation that merely holds the
// freshest row at that number.
func TestConvIDForPID_LiveRowWinsAReusedPidOnDirectCLIIdentity(t *testing.T) {
	setupTestDB(t)
	haveCLIPidReuseRows(t, "tmux-cli-live", "tmux-cli-dead")
	// Only the live agent's pane exists; the dead session left a row behind.
	haveLiveTmuxSessions(t, "tmux-cli-live")

	// The fixture has to actually reproduce the shadowing, or everything
	// below passes for the wrong reason.
	shadow, err := db.FindSessionByPID(pidReuseCLISharedSh)
	require.NoError(t, err)
	require.NotNil(t, shadow)
	require.Equal(t, pidReuseCLIDeadLabel, shadow.ID,
		"fixture must reproduce the corpse shadowing the live row")

	gotConv, hasAncestor := convIDForPID(pidReuseCLIPeerPID)
	require.True(t, hasAncestor, "the harness ancestor must be recognised")
	assert.Equal(t, pidReuseCLILiveConv, gotConv,
		"a live agent must not be given a dead session's conv-id because they share a pid")

	// The authorization chokepoint every handler keys on.
	require.Equal(t, classAgent, classify(&peer{
		PID: pidReuseCLIPeerPID, ConvID: gotConv, HasClaudeAncestor: hasAncestor,
	}))

	// And what the caller is actually told it is, through the production
	// handler rather than the resolver alone.
	enrollCallerOnce(gotConv)
	t.Cleanup(func() { enrolledCallers.Delete(gotConv) })
	req := httptest.NewRequest(http.MethodGet, "/v1/whoami", nil)
	req = req.WithContext(context.WithValue(req.Context(), peerKey{}, &peer{
		PID: pidReuseCLIPeerPID, ConvID: gotConv, HasClaudeAncestor: hasAncestor,
	}))
	rec := httptest.NewRecorder()
	buildMux().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "whoami body=%s", rec.Body.String())
	var identity whoamiResp
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &identity))
	assert.False(t, identity.IsHuman)
	assert.Equal(t, pidReuseCLILiveConv, identity.ConvID,
		"whoami must report the caller's own conversation, not the corpse's")
}

// TestConvIDForPID_NothingAliveResolvesExactlyAsBefore is the other side of
// the ruling: liveness is a repair of a demonstrably dead incumbent, never a
// filter and never a re-ranking. With nothing provably alive there is no
// evidence to act on, so resolution must be byte-for-byte what the plain
// most-recently-updated query produced — resolving nothing here would refuse
// a caller the old code identified.
func TestConvIDForPID_NothingAliveResolvesExactlyAsBefore(t *testing.T) {
	setupTestDB(t)
	haveCLIPidReuseRows(t, "tmux-cli-live", "tmux-cli-dead")
	// Both panes gone — and the same assertion covers an unreachable tmux,
	// which every consumer of this probe treats as an empty alive set.
	haveLiveTmuxSessions(t)

	baseline, err := db.FindSessionByPID(pidReuseCLISharedSh)
	require.NoError(t, err)
	require.NotNil(t, baseline)
	require.Equal(t, pidReuseCLIDeadLabel, baseline.ID)

	gotConv, hasAncestor := convIDForPID(pidReuseCLIPeerPID)
	assert.True(t, hasAncestor)
	assert.Equal(t, baseline.ConvID, gotConv,
		"with nothing alive, resolution must be unchanged from FindSessionByPID")
	assert.Equal(t, classAgent, classify(&peer{
		PID: pidReuseCLIPeerPID, ConvID: gotConv, HasClaudeAncestor: hasAncestor,
	}), "and the caller stays as placeable as it was before the repair")
}

// TestConvIDForPID_ALiveRowWithNoConvIDDoesNotUnresolveTheCaller is the
// invariant's sharp edge. A spawn row is written with an EMPTY conv-id and
// stays that way until the first hook establishes one, so a live sibling that
// cannot answer the question is a routine state, not a curiosity. Letting it
// displace a dead incumbent that CAN answer would turn a misidentification
// into a refusal — the caller resolves to "", convIDForPID reports an
// unidentified ancestor, and classify() drops it to classAgentUnknown.
func TestConvIDForPID_ALiveRowWithNoConvIDDoesNotUnresolveTheCaller(t *testing.T) {
	setupTestDB(t)
	haveCLIPidReuseRows(t, "tmux-cli-live", "tmux-cli-dead")
	// The live row has not established its conv-id yet.
	live, err := db.LoadSession(pidReuseCLILiveLabel)
	require.NoError(t, err)
	require.NotNil(t, live)
	live.ConvID = ""
	require.NoError(t, db.SaveSession(live))
	stampSessionUpdatedAt(t, pidReuseCLILiveLabel, time.Now().Add(-2*time.Minute))
	haveLiveTmuxSessions(t, "tmux-cli-live")

	gotConv, hasAncestor := convIDForPID(pidReuseCLIPeerPID)
	assert.True(t, hasAncestor)
	assert.Equal(t, pidReuseCLIDeadConv, gotConv,
		"a live row with no conv-id must not displace an incumbent that has one: "+
			"the repair may improve which conversation is named, never whether one is")
	assert.Equal(t, classAgent, classify(&peer{
		PID: pidReuseCLIPeerPID, ConvID: gotConv, HasClaudeAncestor: hasAncestor,
	}), "the caller stays placeable, exactly as it was before the repair")
}

// TestConvIDForPID_ALiveRowDoesNotAnswerForAnIncumbentWithNoConvID is the
// mirror, and it is the property for which the accept=(ConvID != "") variant
// was rejected: resolving where the old code resolved nothing. It also
// preserves step order — a step-2/3 miss must keep falling through to the
// probes that carry stronger proof (the layer walk's recorded-implementation
// check, OpenCode's endpoint ownership) rather than being short-circuited by
// a row that merely happens to be alive.
func TestConvIDForPID_ALiveRowDoesNotAnswerForAnIncumbentWithNoConvID(t *testing.T) {
	setupTestDB(t)
	haveCLIPidReuseRows(t, "tmux-cli-live", "tmux-cli-dead")
	// This time the INCUMBENT — the freshest row, whose pane is gone — is the
	// one still waiting on its conv-id.
	dead, err := db.LoadSession(pidReuseCLIDeadLabel)
	require.NoError(t, err)
	require.NotNil(t, dead)
	dead.ConvID = ""
	require.NoError(t, db.SaveSession(dead))
	stampSessionUpdatedAt(t, pidReuseCLIDeadLabel, time.Now().Add(-1*time.Minute))
	haveLiveTmuxSessions(t, "tmux-cli-live")

	baseline, err := db.FindSessionByPID(pidReuseCLISharedSh)
	require.NoError(t, err)
	require.Equal(t, pidReuseCLIDeadLabel, baseline.ID, "fixture: the conv-less row is the incumbent")

	gotConv, _ := convIDForPID(pidReuseCLIPeerPID)
	assert.Empty(t, gotConv,
		"the old code resolved nothing here; a live sibling's conv-id must not be "+
			"substituted for an answer the incumbent never gave")
}

// TestConvIDForPID_ANamelessIncumbentKeepsAReusedPid guards the one case
// where the preference could quietly become a re-ranking. A row with no
// recorded tmux session — what auto-registration writes for a harness not
// started under tmux — can be shown neither alive nor dead. Demoting it below
// an older row that merely HAS a live name would resolve differently from
// FindSessionByPID with no evidence that the older row is right, and on this
// path that means handing a caller another conversation's identity.
func TestConvIDForPID_ANamelessIncumbentKeepsAReusedPid(t *testing.T) {
	setupTestDB(t)
	// The incumbent (freshest) is the nameless one; the older row has a live
	// tmux session. Reusing the fixture with the names swapped in.
	haveCLIPidReuseRows(t, "tmux-cli-live", "")
	haveLiveTmuxSessions(t, "tmux-cli-live")

	incumbent, err := db.FindSessionByPID(pidReuseCLISharedSh)
	require.NoError(t, err)
	require.NotNil(t, incumbent)
	require.Equal(t, pidReuseCLIDeadLabel, incumbent.ID, "fixture: the nameless row is the incumbent")

	gotConv, hasAncestor := convIDForPID(pidReuseCLIPeerPID)
	assert.True(t, hasAncestor)
	assert.Equal(t, pidReuseCLIDeadConv, gotConv,
		"absence of a tmux name is not evidence of death; the incumbent keeps the pid")
}
