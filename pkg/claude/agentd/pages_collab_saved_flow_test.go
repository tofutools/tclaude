package agentd_test

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// tcl-01arix: a saved page keeps the operator's marks and its live patches
// through a close, a daemon restart and a reopen; a private page saves none.

func (e *pageEnv) patchNumbers(t *testing.T, a pageAgent, name string) []int64 {
	t.Helper()
	rec := e.call(t, a, http.MethodGet, "/v1/pages/"+name+"/patches", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var out struct {
		Patches []struct {
			N   int64  `json:"n"`
			CSS string `json:"css"`
		} `json:"patches"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	ns := []int64{}
	for _, p := range out.Patches {
		ns = append(ns, p.N)
	}
	return ns
}

func TestPageSavedKeepsMarksAndPatchesAcrossReopen(t *testing.T) {
	e := newPageEnv(t)
	require.NoError(t, config.Save(&config.Config{
		Agent:    &config.AgentConfig{DefaultPermissions: []string{agentd.PermSelfPage, agentd.PermGroupsPagesPatch}},
		Features: &config.FeaturesConfig{Processes: true},
	}))
	shower := e.agent(t, "keeper", true)
	e.f.HaveGroup("crew")
	e.f.HaveMember("crew", shower.conv)
	target := ":" + strconv.Itoa(devServer(t, "kept"))

	p, rec := e.show(t, shower, target, "app", "")
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	require.Equal(t, http.StatusOK, e.dashJSON(t, http.MethodPost, "/api/pages/"+p.ID+"/marks", sampleMarks()).Code)
	rec = e.call(t, shower, http.MethodPost, "/v1/pages/app/patches", map[string]any{"kind": "css", "css": "button { color: blue }"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// Closed by the operator, then the daemon restarts: nothing is in memory.
	require.Equal(t, http.StatusOK, e.dash(t, http.MethodDelete, "/api/pages/"+p.ID).Code)
	agentd.RestartLivePagesForTest()

	// The operator reopens it: the marks and the patch are back, numbered as before.
	rec = e.dashJSON(t, http.MethodPost, "/api/pages/"+p.ID+"/reopen", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	marks := e.marks(t, shower, "app", "")
	require.Len(t, marks.Batches, 1)
	assert.Equal(t, int64(1), marks.Batches[0].Seq)
	assert.Equal(t, "Make the pay button blue", marks.Batches[0].Note)
	assert.Equal(t, []int64{1}, e.patchNumbers(t, shower, "app"))

	// What comes next continues the numbering, so --since still works.
	require.Equal(t, http.StatusOK, e.dashJSON(t, http.MethodPost, "/api/pages/"+p.ID+"/marks", map[string]any{"note": "And the total"}).Code)
	assert.Equal(t, int64(2), e.marks(t, shower, "app", "").LastSeq)
	assert.Len(t, e.marks(t, shower, "app", "?since=1").Batches, 1)

	// The agent closes it and shows the name again: reopened, still with both.
	require.Equal(t, http.StatusNoContent, e.call(t, shower, http.MethodDelete, "/v1/pages/app", nil).Code)
	again, rec := e.show(t, shower, target, "app", "")
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	require.Equal(t, p.ID, again.ID)
	assert.Len(t, e.marks(t, shower, "app", "").Batches, 2)
	assert.Equal(t, []int64{1}, e.patchNumbers(t, shower, "app"))

	// A reset is saved too.
	require.Equal(t, http.StatusOK, e.call(t, shower, http.MethodDelete, "/v1/pages/app/patches", nil).Code)
	kept, err := db.LoadGroupPageCollab(p.ID)
	require.NoError(t, err)
	assert.JSONEq(t, `[]`, kept.Patches)

	// Delete takes what it kept with it.
	require.Equal(t, http.StatusOK, e.dashJSON(t, http.MethodPost, "/api/pages/"+p.ID+"/delete", nil).Code)
	kept, err = db.LoadGroupPageCollab(p.ID)
	require.NoError(t, err)
	assert.Nil(t, kept)
}

func TestPagePrivateSavesNoMarks(t *testing.T) {
	e := newPageEnv(t)
	shower := e.agent(t, "loner", true)
	target := ":" + strconv.Itoa(devServer(t, "alone"))
	p, rec := e.show(t, shower, target, "app", "")
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	require.Equal(t, http.StatusOK, e.dashJSON(t, http.MethodPost, "/api/pages/"+p.ID+"/marks", sampleMarks()).Code)
	assert.Len(t, e.marks(t, shower, "app", "").Batches, 1)
	kept, err := db.LoadGroupPageCollab(p.ID)
	require.NoError(t, err)
	assert.Nil(t, kept)
}
