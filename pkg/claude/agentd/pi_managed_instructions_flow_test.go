package agentd_test

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/skillbundle"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// TestPiManagedSkillsAndRulesRegisterPerLaunch: a pi agent's managed skills
// and rules are registered on its own launch line, the launch directory as a
// command-line package (so a managed skill shadows an ambient one of the same
// name) and RULES.md appended to the system prompt, after tclaude's status
// extension. Nothing is written to pi's or the shared skill roots, and a
// library change reaches the agent at its next launch, not the running one.
func TestPiManagedSkillsAndRulesRegisterPerLaunch(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	group, err := db.GetAgentGroupByName("alpha")
	require.NoError(t, err)
	skill, err := json.Marshal(skillbundle.Bundle{Format: skillbundle.Format, Name: "review", Files: []skillbundle.File{
		{Path: "SKILL.md", Data: []byte("---\nname: review\ndescription: Review a change\n---\nReview carefully")},
	}})
	require.NoError(t, err)
	_, err = db.PutManagedInstruction(group.ID, "skill", "review", skill, false)
	require.NoError(t, err)
	_, err = db.PutManagedInstruction(group.ID, "rule", "safety", []byte("ORIGINAL PI RULE"), false)
	require.NoError(t, err)

	spawned := f.AsHuman().SpawnWith("alpha", map[string]any{
		"name": "worker", "harness": harness.PiName, "sandbox_implementation": "tclaude-layer",
	})
	require.Equal(t, http.StatusOK, spawned.Code, string(spawned.Raw))
	first := piManagedLaunch(t, f, spawned.AgentID, spawned.ConvID)
	rules, err := os.ReadFile(filepath.Join(first.Root, "RULES.md"))
	require.NoError(t, err)
	assert.Contains(t, string(rules), "ORIGINAL PI RULE")

	home := os.Getenv("HOME")
	for _, global := range []string{
		filepath.Join(testharness.PiAgentDirFor(home), "skills"),
		filepath.Join(testharness.PiAgentDirFor(home), "settings.json"),
		filepath.Join(home, ".agents", "skills"),
	} {
		assert.NoFileExists(t, global, "a managed launch never writes pi's or the shared configuration")
		assert.NoDirExists(t, global, "a managed launch never writes pi's or the shared configuration")
	}

	// A library change waits for the next launch.
	_, err = db.PutManagedInstruction(group.ID, "rule", "safety", []byte("NEXT LAUNCH PI RULE"), true)
	require.NoError(t, err)
	rules, err = os.ReadFile(filepath.Join(first.Root, "RULES.md"))
	require.NoError(t, err)
	assert.NotContains(t, string(rules), "NEXT LAUNCH PI RULE", "the running launch keeps its snapshot")

	f.MarkOffline(spawned.TmuxSession)
	resumed := f.AsHuman().Resume(spawned.ConvID)
	require.Equal(t, "resumed", resumed.Action, string(resumed.Raw))
	second := piManagedLaunch(t, f, spawned.AgentID, spawned.ConvID)
	require.NotEqual(t, first.Root, second.Root)
	rules, err = os.ReadFile(filepath.Join(second.Root, "RULES.md"))
	require.NoError(t, err)
	assert.Contains(t, string(rules), "NEXT LAUNCH PI RULE")
}

// piManagedLaunch returns the agent's managed launch and checks that the pi
// pane was launched with it, in order.
func piManagedLaunch(t *testing.T, f *testharness.Flow, agentID, convID string) *db.ManagedInstructionLaunch {
	t.Helper()
	snapshot, err := db.GetManagedInstructionLaunch(agentID)
	require.NoError(t, err)
	require.NotNil(t, snapshot)
	t.Cleanup(func() { _ = os.RemoveAll(snapshot.Root) })
	sim := f.World.Pis.GetByConvID(convID)
	require.NotNil(t, sim)
	launch := sim.Launch()
	require.Len(t, launch.Extensions, 2, "status extension, then the managed package: %+v", launch)
	assert.True(t, launch.LoadsStatusExtension())
	assert.Equal(t, snapshot.Root, launch.Extensions[1])
	assert.Equal(t, []string{filepath.Join(snapshot.Root, "RULES.md")}, launch.AppendSystemPrompt)
	return snapshot
}
