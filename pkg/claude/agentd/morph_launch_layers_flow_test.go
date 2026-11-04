package agentd_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// Launch slice 7c (tcl-z56w6m): a morph records the fields it sets as the
// agent's explicit launch values, so the morphed form is what later launches
// resolve: a resume relaunches with it, and re-applying the agent's spawn
// profile keeps it while the fields the morph left alone follow the profile.
func TestMorph_RecordsTheFormAsExplicitLaunchValues(t *testing.T) {
	t.Setenv(session.ResourceDelegationDirEnv, "")
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	f.HaveGroup("crew")
	rec := createProfile(t, f, map[string]any{"name": "agentic", "ask_user_question_timeout": "5m", "approval": "auto"})
	require.Equalf(t, http.StatusCreated, rec.Code, "create profile body=%s", rec.Body.String())
	spawn := spawnStopped(t, f, "crew", map[string]any{"name": "worker", "profile": "agentic"})

	rec = f.AsHuman().Morph(spawn.AgentID, map[string]any{"approval": "plan", "effort": "high"})
	resp := decodeMorph(t, rec.Code, rec.Body.Bytes())
	require.Equal(t, "morphed", resp.Result)

	layers, err := db.AgentLaunchLayersForConv(spawn.ConvID)
	require.NoError(t, err)
	require.NotNil(t, layers)
	for key, field := range map[string]string{"approval": "ApprovalPolicy", "effort": "Effort"} {
		value := layers.Explicit[key]
		assert.Truef(t, strings.HasPrefix(value.Provenance, "explicit (morph, "), "%s provenance %q", key, value.Provenance)
		assert.Containsf(t, value.Fields, field, "%s records its request field", key)
	}
	assert.NotContains(t, layers.Explicit, "ask_timeout", "a field the morph left alone is not made explicit")

	// Morph, then resume: the resume launches the morphed form.
	resumeAgain(t, f, spawn.ConvID)
	approval, ok := f.World.SpawnApproval(spawn.ConvID)
	require.True(t, ok)
	assert.Equal(t, "plan", approval, "resume after the morph")

	// Morph, then re-apply an edited spawn profile: the morphed approval
	// stays, the ask timeout the morph did not set follows the profile.
	if sess, err := db.FindSessionByConvID(spawn.ConvID); err == nil && sess != nil {
		f.MarkOffline(sess.TmuxSession)
	}
	newCopy := editedCopy(t, "agentic", func(p *db.SpawnProfile) { p.AskUserQuestionTimeout, p.Approval = "10m", "auto" })
	kind, msg := agentd.ApplySpawnCopyForTest(agentIDFor(t, spawn.ConvID), newCopy)
	require.Empty(t, kind, msg)
	resumeAgain(t, f, spawn.ConvID)
	approval, ok = f.World.SpawnApproval(spawn.ConvID)
	require.True(t, ok)
	assert.Equal(t, "plan", approval, "the re-applied profile keeps the morphed approval")
	askTimeout, ok := f.World.SpawnAskTimeout(spawn.ConvID)
	require.True(t, ok)
	assert.Equal(t, "10m", askTimeout, "the re-applied profile gives what the morph left alone")
	effort, ok := f.World.SpawnEffort(spawn.ConvID)
	require.True(t, ok)
	assert.Equal(t, "high", effort)
}

// A morph back sets every field of the form it restores, but makes explicit
// only what the morph it undoes changed: the fields no morph chose still
// follow a re-applied spawn profile.
func TestMorph_BackMakesExplicitOnlyWhatChanged(t *testing.T) {
	t.Setenv(session.ResourceDelegationDirEnv, "")
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	f.HaveGroup("crew")
	rec := createProfile(t, f, map[string]any{"name": "agentic", "ask_user_question_timeout": "5m", "approval": "auto"})
	require.Equalf(t, http.StatusCreated, rec.Code, "create profile body=%s", rec.Body.String())
	spawn := spawnStopped(t, f, "crew", map[string]any{"name": "worker", "profile": "agentic"})

	rec = f.AsHuman().Morph(spawn.AgentID, map[string]any{"approval": "plan"})
	require.Equal(t, "morphed", decodeMorph(t, rec.Code, rec.Body.Bytes()).Result)
	rec = f.AsHuman().Morph(spawn.AgentID, map[string]any{"back": true})
	require.Equal(t, "morphed", decodeMorph(t, rec.Code, rec.Body.Bytes()).Result)

	layers, err := db.AgentLaunchLayersForConv(spawn.ConvID)
	require.NoError(t, err)
	require.NotNil(t, layers)
	assert.JSONEq(t, `"auto"`, string(layers.Explicit["approval"].Fields["ApprovalPolicy"]), "the morph back restored the approval explicitly")
	for _, key := range []string{"model", "effort", "tools", "ask_timeout", "context_window"} {
		assert.NotContainsf(t, layers.Explicit, key, "no morph changed %s", key)
	}

	newCopy := editedCopy(t, "agentic", func(p *db.SpawnProfile) { p.AskUserQuestionTimeout = "10m" })
	kind, msg := agentd.ApplySpawnCopyForTest(agentIDFor(t, spawn.ConvID), newCopy)
	require.Empty(t, kind, msg)
	resumeAgain(t, f, spawn.ConvID)
	askTimeout, ok := f.World.SpawnAskTimeout(spawn.ConvID)
	require.True(t, ok)
	assert.Equal(t, "10m", askTimeout, "a field no morph changed follows the re-applied profile")
}
