package agentd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/tmuxtest"
)

// aliveTmux reports every session as alive with a live pane.
type aliveTmux struct{}

func (aliveTmux) Command(args ...string) *exec.Cmd {
	if len(args) > 0 && args[0] == "display-message" {
		return exec.Command("echo", "0")
	}
	return exec.Command("true")
}

// Run and AttachArgv make the exec-shaped fake above a clcommon.Tmux.
func (x aliveTmux) Run(ctx context.Context, args []string) ([]byte, error) {
	return tmuxtest.Run(ctx, x.Command(args...))
}

func (x aliveTmux) AttachArgv(args []string) ([]string, error) {
	return tmuxtest.Argv(x.Command(args...))
}

func (aliveTmux) ListSessions() (map[string]struct{}, error) { return map[string]struct{}{}, nil }

// The spawn-focus tab redials by label, and labels and tmux names are reused
// when a new agent gets the same name. A tab bound to the agent it was opened
// for must refuse a pane that now belongs to another agent instead of
// attaching to it.
func TestSpawnFocusSession_RefusesAPaneOwnedByAnotherAgent(t *testing.T) {
	setupTestDB(t)
	prev := clcommon.Default
	clcommon.Default = aliveTmux{}
	t.Cleanup(func() { clcommon.Default = prev })

	oldAgent, _, err := db.EnsureAgentForConv("conv-old", "test")
	require.NoError(t, err)
	newAgent, _, err := db.EnsureAgentForConv("conv-new", "test")
	require.NoError(t, err)
	require.NotEqual(t, oldAgent, newAgent)

	// The label "worker" now runs the NEW agent.
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "worker", TmuxSession: "worker", ConvID: "conv-new", Status: "idle",
	}))
	r := httptest.NewRequest(http.MethodGet, "/api/spawn-focus-ws/worker", nil)

	sess, reused := waitForDashboardSpawnFocusSession(r, "worker", oldAgent)
	assert.Nil(t, sess, "a stale tab must not attach to the agent that reused its label")
	assert.True(t, reused)

	sess, reused = waitForDashboardSpawnFocusSession(r, "worker", newAgent)
	require.NotNil(t, sess, "the agent's own tab still attaches")
	assert.False(t, reused)

	sess, reused = waitForDashboardSpawnFocusSession(r, "worker", "")
	require.NotNil(t, sess, "an unbound (legacy) path keeps the label-only behaviour")
	assert.False(t, reused)
}

// Before the conv-id lands, the pending spawn's reserved agent id owns the
// label: the agent's own tab attaches and another agent's stale tab does not.
func TestSpawnFocusSession_PendingReservationNamesTheOwner(t *testing.T) {
	setupTestDB(t)
	prev := clcommon.Default
	clcommon.Default = aliveTmux{}
	t.Cleanup(func() { clcommon.Default = prev })

	staleAgent, _, err := db.EnsureAgentForConv("conv-stale", "test")
	require.NoError(t, err)
	const reserved = "agt_00000000000000000000000000reserved"
	require.NoError(t, db.InsertPendingSpawn(&db.PendingSpawn{Label: "codexer", AgentID: reserved}))
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "codexer", TmuxSession: "codexer", Status: "idle",
	}))
	r := httptest.NewRequest(http.MethodGet, "/api/spawn-focus-ws/codexer", nil)

	sess, reused := waitForDashboardSpawnFocusSession(r, "codexer", reserved)
	require.NotNil(t, sess)
	assert.False(t, reused)

	sess, reused = waitForDashboardSpawnFocusSession(r, "codexer", staleAgent)
	assert.Nil(t, sess)
	assert.True(t, reused)
}

// A pane whose owner is not known yet is waited on, not trusted, and the wait
// ends with the request.
func TestSpawnFocusSession_UnknownOwnerIsNotTrusted(t *testing.T) {
	setupTestDB(t)
	prev := clcommon.Default
	clcommon.Default = aliveTmux{}
	t.Cleanup(func() { clcommon.Default = prev })

	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "anon", TmuxSession: "anon", Status: "idle",
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	r := httptest.NewRequest(http.MethodGet, "/api/spawn-focus-ws/anon", nil).WithContext(ctx)

	sess, reused := waitForDashboardSpawnFocusSession(r, "anon", "agt_someone")
	assert.Nil(t, sess)
	assert.False(t, reused)
}

func TestSpawnFocusWSPath_BindsTheAgent(t *testing.T) {
	assert.Equal(t, "/api/spawn-focus-ws/worker", spawnFocusWSPath("worker", ""))
	got := spawnFocusWSPath("my worker", "agt_abc")
	assert.True(t, strings.HasPrefix(got, "/api/spawn-focus-ws/my%20worker?agent=agt_abc"), got)
	assert.True(t, isAgentTerminalPath(got), "a bound path is still an agent terminal path")
}
