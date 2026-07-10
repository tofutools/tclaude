package agentd_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// JOH-360: the agent-caller path for template editing. Templates are the
// "summoning circles" a human wants to define + edit by just talking to a
// scribe agent, so template mutations must be fully supported AND
// permission-gated for agent callers (not just the dashboard human), and the
// refusal must be fail-closed. These flow tests drive the production mux as an
// identified agent peer (the same identity `tclaude agent templates …` resolves
// to when run from inside a session) and assert at the wire surfaces the CLI
// reads back — never at DB internals.

// agentReq issues an HTTP request against the daemon mux as an identified agent
// caller bound to convID. Permission gates resolve against that conv's grants,
// so an ungranted conv is refused fail-closed while a granted one passes —
// exactly the boundary a scribe agent hits.
func agentReq(t *testing.T, f *testharness.Flow, convID, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	r := agentd.AsAgentPeer(testharness.JSONRequest(t, method, path, body), convID)
	return testharness.Serve(f.Mux, r)
}

// Scenario: a scribe agent holding templates.manage runs the exact loop an LLM
// caller runs — create → show(--json) → mutate the JSON → edit(full replace) →
// show(verify) → rm — through the production mux. An ungranted agent is refused
// on every mutation (403, fail-closed) yet can still read (ls / show are open),
// and the refused writes change nothing. Every assertion is at the wire surface
// (the JSON the CLI shows back), not a DB internal.
func TestGroupTemplate_AgentCallerGatedCRUD(t *testing.T) {
	f := newFlow(t)

	const scribe = "scrb-aaaa-bbbb-cccc-dddd"
	const intruder = "intr-aaaa-bbbb-cccc-dddd"

	// The scribe holds templates.manage; the intruder holds nothing.
	require.NoError(t, db.GrantAgentPermission(scribe, agentd.PermTemplatesManage, "test"),
		"grant scribe templates.manage")

	// --- create (scribe) → 201 ---
	createBody := map[string]any{
		"name":                "scribe-circle",
		"descr":               "first draft",
		"default_context":     "Use worktrees and open PRs.",
		"per_agent_worktrees": true,
		"agents": []map[string]any{
			{"name": "lead", "role": "lead", "is_owner": true, "initial_message": "Lead the party."},
		},
	}
	rec := agentReq(t, f, scribe, http.MethodPost, "/v1/templates", createBody)
	require.Equalf(t, http.StatusCreated, rec.Code, "scribe create: %s", rec.Body.String())

	// --- show --json (scribe): dump the current wire state to mutate ---
	rec = agentReq(t, f, scribe, http.MethodGet, "/v1/templates/scribe-circle", nil)
	require.Equalf(t, http.StatusOK, rec.Code, "scribe show: %s", rec.Body.String())
	var current map[string]any
	testharness.DecodeJSON(t, rec, &current)
	assert.Equal(t, "first draft", current["descr"], "show reflects the created descr")
	assert.Equal(t, true, current["per_agent_worktrees"], "show reflects the worktree default")
	agents, _ := current["agents"].([]any)
	require.Len(t, agents, 1, "one agent so far")

	// --- mutate the JSON as an LLM would: change descr, add a second agent ---
	current["descr"] = "second draft"
	current["per_agent_worktrees"] = false
	current["agents"] = append(agents, map[string]any{
		"name": "hand", "role": "hand", "initial_message": "Assist the lead.",
	})

	// --- edit --file (scribe): PATCH is a FULL REPLACE, so post the whole body ---
	rec = agentReq(t, f, scribe, http.MethodPatch, "/v1/templates/scribe-circle", current)
	require.Equalf(t, http.StatusOK, rec.Code, "scribe edit (full replace): %s", rec.Body.String())

	// --- show (scribe): the mutation persisted at the wire surface ---
	rec = agentReq(t, f, scribe, http.MethodGet, "/v1/templates/scribe-circle", nil)
	require.Equalf(t, http.StatusOK, rec.Code, "scribe re-show: %s", rec.Body.String())
	var after map[string]any
	testharness.DecodeJSON(t, rec, &after)
	assert.Equal(t, "second draft", after["descr"], "edited descr round-trips through the wire")
	assert.NotContains(t, after, "per_agent_worktrees",
		"the cleared false default is omitted from the compact wire response")
	afterAgents, _ := after["agents"].([]any)
	require.Len(t, afterAgents, 2, "the full-replace edit added the second agent")

	// --- an ungranted agent: reads are open, every mutation is refused ---
	rec = agentReq(t, f, intruder, http.MethodGet, "/v1/templates", nil)
	assert.Equalf(t, http.StatusOK, rec.Code, "ungranted ls is open: %s", rec.Body.String())
	rec = agentReq(t, f, intruder, http.MethodGet, "/v1/templates/scribe-circle", nil)
	assert.Equalf(t, http.StatusOK, rec.Code, "ungranted show is open: %s", rec.Body.String())

	rec = agentReq(t, f, intruder, http.MethodPost, "/v1/templates",
		map[string]any{"name": "intruder-circle", "agents": []map[string]any{{"name": "x"}}})
	assert.Equalf(t, http.StatusForbidden, rec.Code, "ungranted create refused: %s", rec.Body.String())

	rec = agentReq(t, f, intruder, http.MethodPatch, "/v1/templates/scribe-circle",
		map[string]any{"name": "scribe-circle", "descr": "hijacked", "agents": []map[string]any{{"name": "x"}}})
	assert.Equalf(t, http.StatusForbidden, rec.Code, "ungranted edit refused: %s", rec.Body.String())

	rec = agentReq(t, f, intruder, http.MethodDelete, "/v1/templates/scribe-circle", nil)
	assert.Equalf(t, http.StatusForbidden, rec.Code, "ungranted delete refused: %s", rec.Body.String())

	// The refused mutations changed nothing — the template is intact + unchanged,
	// and the intruder's create never landed.
	rec = agentReq(t, f, scribe, http.MethodGet, "/v1/templates/scribe-circle", nil)
	require.Equalf(t, http.StatusOK, rec.Code, "template survived the refused mutations: %s", rec.Body.String())
	var intact map[string]any
	testharness.DecodeJSON(t, rec, &intact)
	assert.Equal(t, "second draft", intact["descr"], "the intruder's PATCH did not take")
	rec = agentReq(t, f, scribe, http.MethodGet, "/v1/templates/intruder-circle", nil)
	assert.Equalf(t, http.StatusNotFound, rec.Code, "the intruder's create never landed: %s", rec.Body.String())

	// --- rm (scribe): the granted agent completes the loop ---
	rec = agentReq(t, f, scribe, http.MethodDelete, "/v1/templates/scribe-circle", nil)
	require.Equalf(t, http.StatusNoContent, rec.Code, "scribe delete: %s", rec.Body.String())
	rec = agentReq(t, f, scribe, http.MethodGet, "/v1/templates/scribe-circle", nil)
	assert.Equalf(t, http.StatusNotFound, rec.Code, "template gone after delete: %s", rec.Body.String())
}

// Scenario: validation failures from buildTemplateFromJSON are actionable for an
// LLM caller — each names the offending field and says what is allowed (the bar
// the unknown-permission-slug error already sets). Asserted at the wire (the 400
// body a scribe reads back), covering the by-name references most likely to
// dangle in a hand-authored edit: role_ref and spawn_profile.
func TestGroupTemplate_ValidationErrorsAreActionable(t *testing.T) {
	f := newFlow(t)

	// Seed a role + a spawn profile with distinctive names so each hint lists a
	// real value (unique names avoid collisions with the JOH-240 seed library).
	require.Equalf(t, http.StatusCreated,
		createRole(t, f, map[string]any{"name": "circ-scribe-role"}).Code, "seed role")
	require.Equalf(t, http.StatusCreated,
		createProfile(t, f, map[string]any{"name": "circ-scribe-kit"}).Code, "seed profile")

	// Dangling role_ref: names the field, echoes the bad value, lists known roles.
	rec := humanReq(t, f, http.MethodPost, "/v1/templates", map[string]any{
		"name":   "bad-roleref",
		"agents": []map[string]any{{"name": "a", "role_ref": "ghost"}},
	})
	require.Equalf(t, http.StatusBadRequest, rec.Code, "dangling role_ref 400s: %s", rec.Body.String())
	body := rec.Body.String()
	assert.Contains(t, body, "role_ref", "role_ref error names the field")
	assert.Contains(t, body, "ghost", "role_ref error echoes the bad value")
	assert.Contains(t, body, "circ-scribe-role", "role_ref error lists the known roles")

	// Dangling spawn_profile: names the field, echoes the bad value, lists the
	// known profiles.
	rec = humanReq(t, f, http.MethodPost, "/v1/templates", map[string]any{
		"name":   "bad-profile",
		"agents": []map[string]any{{"name": "a", "spawn_profile": "ghost-kit"}},
	})
	require.Equalf(t, http.StatusBadRequest, rec.Code, "dangling spawn_profile 400s: %s", rec.Body.String())
	body = rec.Body.String()
	assert.Contains(t, body, "spawn_profile", "spawn_profile error names the field")
	assert.Contains(t, body, "ghost-kit", "spawn_profile error echoes the bad value")
	assert.Contains(t, body, "circ-scribe-kit", "spawn_profile error lists the known profiles")
}
