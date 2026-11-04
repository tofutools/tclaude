package agentd_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Launch slice 7d (tcl-7by92o): a cross-harness morph records its form as
// explicit launch values, as a same-harness morph does (7c), so a re-applied
// spawn profile keeps the morphed form.

func crossMorphFixture(t *testing.T) (*testharness.Flow, testharness.SpawnResp) {
	t.Helper()
	t.Setenv(session.ResourceDelegationDirEnv, "")
	t.Setenv("CODEX_HOME", "")
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	f.HaveGroup("crew")
	rec := createProfile(t, f, map[string]any{"name": "agentic", "ask_user_question_timeout": "5m", "approval": "auto"})
	require.Equalf(t, http.StatusCreated, rec.Code, "create profile body=%s", rec.Body.String())
	spawn := spawnMorphWorker(t, f, "crew", "shifter", map[string]any{"profile": "agentic"})
	seedClaudeHistory(t, f, spawn.ConvID)
	f.SetSessionStatus(spawn.ConvID, session.StatusIdle)
	return f, spawn
}

func morphToCodex(t *testing.T, f *testharness.Flow, agentID string) string {
	t.Helper()
	rec := f.AsHuman().Morph(agentID, map[string]any{"harness": "codex", "model": "gpt-5.4", "effort": "high", "approval": "on-request"})
	resp := decodeCrossMorph(t, rec.Code, rec.Body.Bytes())
	require.Equal(t, "morphed", resp.Result)
	return resp.ConvID
}

// relaunchFields is the stored relaunch profile without the morph's own
// bookkeeping.
func relaunchFields(t *testing.T, convID string) map[string]any {
	t.Helper()
	var fields map[string]any
	require.NoError(t, json.Unmarshal([]byte(relaunchProfileJSON(t, convID)), &fields))
	delete(fields, "previous_morph_form")
	return fields
}

// Claude to Codex and back keeps every field the morph does not change, and
// the explicit layer ends naming the form the agent is back in.
func TestMorphCrossHarness_RoundTripKeepsTheRest(t *testing.T) {
	f, spawn := crossMorphFixture(t)
	before := relaunchFields(t, spawn.ConvID)
	spawnedLayers, err := db.AgentLaunchLayersForConv(spawn.ConvID)
	require.NoError(t, err)
	require.NotNil(t, spawnedLayers)

	codexConv := morphToCodex(t, f, spawn.AgentID)
	layers, err := db.AgentLaunchLayersForConv(codexConv)
	require.NoError(t, err)
	require.NotNil(t, layers)
	for key, want := range map[string]string{"harness": `"codex"`, "model": `"gpt-5.4"`, "approval": `"on-request"`} {
		value := layers.Explicit[key]
		assert.Truef(t, strings.HasPrefix(value.Provenance, "explicit (morph, "), "%s provenance %q", key, value.Provenance)
		for _, raw := range value.Fields {
			assert.JSONEqf(t, want, string(raw), "%s", key)
		}
	}

	f.SetSessionStatus(codexConv, session.StatusIdle)
	rec := f.AsHuman().Morph(spawn.AgentID, map[string]any{"back": true})
	back := decodeCrossMorph(t, rec.Code, rec.Body.Bytes())
	require.Equal(t, "morphed", back.Result)

	after := relaunchFields(t, back.ConvID)
	// The built-in mode is now the morph back's choice; its value is the same.
	assert.Equal(t, "morph", after["sandbox_mode_source"])
	delete(before, "sandbox_mode_source")
	delete(after, "sandbox_mode_source")
	assert.Equal(t, before, after, "the round trip keeps every other field")
	askTimeout, ok := f.World.SpawnAskTimeout(back.ConvID)
	require.True(t, ok)
	assert.Equal(t, "5m", askTimeout)

	backLayers, err := db.AgentLaunchLayersForConv(back.ConvID)
	require.NoError(t, err)
	require.NotNil(t, backLayers)
	assert.Equal(t, spawnedLayers.Explicit, backLayers.Explicit, "the explicit layer is as it was before the round trip")
}

// Re-applying the agent's spawn profile after a cross-harness morph keeps the
// morphed form: the Claude profile's approval does not come back over the
// Codex one.
func TestMorphCrossHarness_ReapplyKeepsTheMorphedForm(t *testing.T) {
	f, spawn := crossMorphFixture(t)
	codexConv := morphToCodex(t, f, spawn.AgentID)
	if sess, err := db.FindSessionByConvID(codexConv); err == nil && sess != nil {
		f.MarkOffline(sess.TmuxSession)
	}

	newCopy := editedCopy(t, "agentic", func(p *db.SpawnProfile) { p.Approval = "plan" })
	kind, msg := agentd.ApplySpawnCopyForTest(agentIDFor(t, codexConv), newCopy)
	require.Empty(t, kind, msg)
	stored := storedRelaunch(t, codexConv)
	require.NotNil(t, stored.ApprovalPolicy)
	assert.Equal(t, "on-request", *stored.ApprovalPolicy)

	resumeAgain(t, f, codexConv)
	approval, ok := f.World.SpawnApproval(codexConv)
	require.True(t, ok)
	assert.Equal(t, "on-request", approval, "the re-applied profile keeps the morphed approval")
	model, ok := f.World.SpawnModel(codexConv)
	require.True(t, ok)
	assert.Equal(t, "gpt-5.4", model)
}
