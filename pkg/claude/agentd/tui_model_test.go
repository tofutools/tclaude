package agentd

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
)

func TestTUISpawnModelAndEffort(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(map[bool]string{false: "defaults", true: "overrides"}[explicit], func(t *testing.T) {
			var got agent.SpawnRequest
			api := stubTUIAPI(func(w http.ResponseWriter, r *http.Request) {
				require.NoError(t, json.NewDecoder(r.Body).Decode(&got))
				writeJSON(w, http.StatusOK, agent.SpawnResponse{Group: "dev", AgentID: "agt_1"})
			})
			m := newTUIModel(api)
			m.groups = []tuiGroupRow{{Name: "dev"}}
			m.profiles = []tuiProfileRow{{Name: "kit"}}
			m = m.openSpawnForm().focusSpawnField(tuiFieldProfile).cycleChoice(1)
			if explicit {
				m = typeInto(m, tuiFieldModel, " sonnet ")
				m = typeInto(m, tuiFieldEffort, " high ")
			}
			assert.Contains(t, m.renderSpawnForm(), "Model:")
			assert.Contains(t, m.renderSpawnForm(), "Effort:")
			_, cmd := m.submitSpawn()
			require.NotNil(t, cmd)
			result := cmd().(tuiSpawnedMsg)
			require.NoError(t, result.err)
			assert.Equal(t, "kit", got.Profile)
			assert.Empty(t, got.Harness, "overrides must not silently pin a harness")
			if explicit {
				assert.Equal(t, "sonnet", got.Model)
				assert.Equal(t, "high", got.Effort)
			} else {
				assert.Empty(t, got.Model)
				assert.Empty(t, got.Effort)
			}
		})
	}
}

func TestTUISpawnModelAndEffortNavigation(t *testing.T) {
	m := newTUIModel(nil).openSpawnForm().focusSpawnField(tuiFieldHarness)
	m = m.moveSpawnField(1)
	assert.Equal(t, tuiFieldModel, m.form.field)
	assert.True(t, m.form.model.Focused())
	m = m.moveSpawnField(1)
	assert.Equal(t, tuiFieldEffort, m.form.field)
	assert.False(t, m.form.model.Focused())
	assert.True(t, m.form.effort.Focused())
	m = m.moveSpawnField(1)
	assert.Equal(t, tuiFieldBrief, m.form.field)
	assert.False(t, m.form.effort.Focused())
	m = m.moveSpawnField(-1)
	assert.Equal(t, tuiFieldEffort, m.form.field)
}
