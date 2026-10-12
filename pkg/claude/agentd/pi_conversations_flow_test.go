package agentd_test

import (
	"net/http"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// A clone of a pi agent carries its conversation: the session file is copied
// into pi's store under the clone's own id, the clone's pane resumes it, and
// both conversations are listed with their names.
func TestPiConversations_CloneCarriesHistory(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	optInPi(t)
	f := newFlow(t)
	f.HaveGroup("crew")
	resp, sim := spawnPi(t, f, "crew", map[string]any{"name": "pi-worker"})
	typeInto(sim, "remember the word PELICAN")
	require.NoError(t, sim.Reply("Noted: PELICAN."))

	cloned := f.AsHuman().CloneWith(resp.ConvID, map[string]any{})
	require.Equalf(t, http.StatusOK, cloned.Code, "clone: %s", cloned.Raw)
	require.NotEqual(t, resp.ConvID, cloned.NewConv)
	clone := f.World.Pis.GetByConvID(cloned.NewConv)
	require.NotNil(t, clone, "the clone runs in a pi pane")
	assert.NotEqual(t, sim.Path(), clone.Path(), "a copy, not the original file")
	raw, err := os.ReadFile(clone.Path())
	require.NoError(t, err)
	assert.Contains(t, string(raw), "remember the word PELICAN")
	assert.Contains(t, string(raw), "Noted: PELICAN.")

	convs, err := harness.MustGet(harness.PiName).Convs.ListConvs("")
	require.NoError(t, err)
	titles := map[string]string{}
	for _, c := range convs {
		titles[c.SessionID] = c.DisplayTitle()
	}
	assert.Equal(t, "pi-worker", titles[resp.ConvID])
	assert.Contains(t, titles, cloned.NewConv)
	require.NotNil(t, findDashMember(fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest()), "crew", cloned.NewConv),
		"the clone joins the group")
}
