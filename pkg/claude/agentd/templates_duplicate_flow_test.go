package agentd_test

import (
	"encoding/json"
	"maps"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// JOH-365: the templates manager's ⧉ duplicate card action. There is no
// dedicated backend — the dashboard clones a template by re-POSTing its own
// fetched JSON to the create endpoint (POST /v1/templates) with the name
// swapped. That works ONLY because the create endpoint round-trips EVERY
// stored field, so a copy is byte-for-byte the original bar the name. This
// flow test pins that fidelity contract at the real HTTP surface the dashboard
// drives, so a future field added to the template struct but dropped from the
// create round-trip fails here rather than silently producing lossy copies.
//
// It mirrors the client exactly: GET the source template's JSON, drop the
// response-only created_at/updated_at, swap the name, POST it back — then diff
// the stored copy against the original and require equality on every field but
// the name.
func TestTemplateDuplicate_FullFidelityRoundTrip(t *testing.T) {
	f := newFlow(t)

	// A role and a spawn profile the maximally-populated template references by
	// name — the copy must keep those references verbatim (same-machine
	// duplicate: they resolve locally, so no re-embedding is needed).
	require.Equalf(t, http.StatusCreated, createRole(t, f, map[string]any{
		"name":        "cold-reviewer",
		"descr":       "cold reviewer",
		"brief":       "You review changes with fresh eyes.",
		"model":       "haiku",
		"permissions": []string{"human.notify"},
	}).Code, "create role")
	require.Equalf(t, http.StatusCreated,
		createProfile(t, f, map[string]any{"name": "cheap", "model": "haiku"}).Code, "create profile")

	// A template exercising every stored field: descr, default_context, agents
	// (owner + role + role_ref + spawn_profile + inline launch overrides +
	// permissions + initial_message + distinct waves), a routed work pattern,
	// a multi-phase process, interval + cron rhythms, and a wave-max-wait cap.
	origin := map[string]any{
		"name":            "origin",
		"descr":           "a lead and a reviewer",
		"default_context": "TEAM RULES: worktrees + PRs",
		"agents": []map[string]any{
			{
				"name": "lead", "role": "lead", "descr": "coordinates",
				"is_owner": true, "initial_message": "Lead the team.",
				"permissions":   []string{"groups.spawn"},
				"spawn_profile": "cheap", "model": "opus", "effort": "high",
				"wave": 0,
			},
			{
				"name": "rev", "role": "qa", "role_ref": "cold-reviewer",
				"descr": "reviews", "wave": 1,
			},
		},
		"work_pattern": []map[string]any{
			{"send_to": "lead", "value": "Kick off: {{task}}"},
			{"send_to": "all", "value": "House rules apply."},
		},
		"process": []map[string]any{
			{"name": "build", "roles": []string{"lead"}, "criteria": "ship the change"},
			{"name": "review", "roles": []string{"qa"}, "criteria": "cold review"},
		},
		"rhythms": []map[string]any{
			{"name": "standup", "target_role": "all", "interval": "10m", "subject": "sync", "body": "status?"},
			{"name": "nightly", "cron_expr": "0 0 * * *", "body": "wrap up"},
		},
		"wave_max_wait": 120,
	}
	require.Equalf(t, http.StatusCreated,
		humanReq(t, f, http.MethodPost, "/v1/templates", origin).Code, "create origin template")

	// Fetch the stored source exactly as the dashboard reads it.
	rec := humanReq(t, f, http.MethodGet, "/v1/templates/origin", nil)
	require.Equalf(t, http.StatusOK, rec.Code, "fetch origin: %s", rec.Body.String())
	var srcJSON map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &srcJSON))

	// Guard that the rich fields actually landed in the stored source. The
	// copy==source diff below shares templateToJSON on both sides, so a field
	// the store/projection drops would vanish SYMMETRICALLY and the diff would
	// still pass — asserting concrete source values makes such a drop fail here,
	// on the source side, which is the fidelity guarantee this test claims.
	assert.Equal(t, "a lead and a reviewer", srcJSON["descr"], "descr stored")
	assert.Equal(t, "TEAM RULES: worktrees + PRs", srcJSON["default_context"], "default_context stored")
	assert.EqualValues(t, 120, srcJSON["wave_max_wait"], "wave_max_wait stored")
	require.Len(t, srcJSON["work_pattern"], 2, "work pattern stored")
	require.Len(t, srcJSON["process"], 2, "process stored")
	require.Len(t, srcJSON["rhythms"], 2, "rhythms stored")
	agents, ok := srcJSON["agents"].([]any)
	require.True(t, ok, "agents is a list")
	require.Len(t, agents, 2, "both agents stored")
	lead, _ := agents[0].(map[string]any)
	assert.Equal(t, "lead", lead["name"])
	assert.Equal(t, true, lead["is_owner"], "owner flag stored")
	assert.Equal(t, "cheap", lead["spawn_profile"], "spawn profile ref stored")
	assert.Equal(t, "opus", lead["model"], "inline model stored")
	assert.Equal(t, "high", lead["effort"], "inline effort stored")
	assert.Equal(t, "Lead the team.", lead["initial_message"], "initial message stored")
	assert.Equal(t, []any{"groups.spawn"}, lead["permissions"], "permission slug stored")
	rev, _ := agents[1].(map[string]any)
	assert.Equal(t, "cold-reviewer", rev["role_ref"], "role_ref stored")
	assert.EqualValues(t, 1, rev["wave"], "distinct wave stored")

	// Client-side clone: shallow-copy the fetched JSON, drop the response-only
	// timestamps, swap the name — the same payload submitDuplicate() builds.
	dupPayload := maps.Clone(srcJSON)
	delete(dupPayload, "created_at")
	delete(dupPayload, "updated_at")
	dupPayload["name"] = "origin-copy"
	require.Equalf(t, http.StatusCreated,
		humanReq(t, f, http.MethodPost, "/v1/templates", dupPayload).Code, "duplicate via create endpoint")

	// Fetch the copy and diff it against the original: everything equal but name.
	rec = humanReq(t, f, http.MethodGet, "/v1/templates/origin-copy", nil)
	require.Equalf(t, http.StatusOK, rec.Code, "fetch copy: %s", rec.Body.String())
	var copyJSON map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &copyJSON))

	// Normalize both for the diff: name is expected to differ; created_at/
	// updated_at are per-row provenance, not blueprint. Everything else — every
	// agent field, the work pattern, process, rhythms, waves, context — must be
	// identical, proving the create round-trip is lossless.
	assert.Equal(t, "origin", srcJSON["name"], "sanity: source name")
	assert.Equal(t, "origin-copy", copyJSON["name"], "copy took the new name")
	for _, m := range []map[string]any{srcJSON, copyJSON} {
		delete(m, "name")
		delete(m, "created_at")
		delete(m, "updated_at")
	}
	assert.Equal(t, srcJSON, copyJSON,
		"the duplicate is field-for-field identical to the original (bar the name)")
}

// TestTemplateDuplicate_NameCollisionIs409 pins the collision guard the
// dashboard relies on: re-POSTing under an existing name is a 409, which the
// dialog surfaces inline (keeping the dialog open) rather than clobbering. The
// duplicate action has no overwrite mode — a copy always needs a fresh name.
func TestTemplateDuplicate_NameCollisionIs409(t *testing.T) {
	f := newFlow(t)

	body := map[string]any{
		"name":   "taken",
		"agents": []map[string]any{{"name": "solo"}},
	}
	require.Equalf(t, http.StatusCreated,
		humanReq(t, f, http.MethodPost, "/v1/templates", body).Code, "create original")

	// Duplicating onto the SAME name (what the dialog blocks client-side, but
	// the server is the real authority) is refused with a conflict.
	rec := humanReq(t, f, http.MethodPost, "/v1/templates", body)
	assert.Equalf(t, http.StatusConflict, rec.Code,
		"a duplicate onto an existing name must 409: %s", rec.Body.String())
}
