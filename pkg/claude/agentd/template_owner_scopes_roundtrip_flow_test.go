package agentd_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/testharness"
	"github.com/tofutools/tclaude/pkg/testutil"
)

// tcl-cnua7d: `templates edit --file` is a full replace, and the CLI read the
// file into a mirror with no owner_scopes, so the show --json → edit --file
// loop the agent-circles skill teaches deleted a template's owner-scope
// narrowing (TCL-1071). The loop now keeps it, and a file that changes it
// changes it.

const roundTripScopes = `{"groups.members.spawn":{"spawn_profile":["p1"]}}`

func templateOwnerScopes(t *testing.T, h http.Handler, name string) string {
	t.Helper()
	rec := testharness.Serve(h, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/templates/"+name, nil)))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body struct {
		OwnerScopes json.RawMessage `json:"owner_scopes"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	return string(body.OwnerScopes)
}

func haveScopedTemplate(t *testing.T, h http.Handler) {
	t.Helper()
	var scopes map[string]any
	require.NoError(t, json.Unmarshal([]byte(roundTripScopes), &scopes))
	rec := testharness.Serve(h, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/templates", map[string]any{
		"name": "crew", "agents": []map[string]any{{"name": "lead"}}, "owner_scopes": scopes,
	})))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	require.JSONEq(t, roundTripScopes, templateOwnerScopes(t, h, "crew"))
}

func TestTemplateEdit_TheShowEditLoopKeepsOwnerScopes(t *testing.T) {
	f := newFlow(t)
	haveScopedTemplate(t, f.Mux)
	bridgeAgentClientToMuxWithPeer(t, f.Mux, agentd.AsHumanPeer)

	var shown, stderr bytes.Buffer
	require.Equal(t, 0, agent.RunTemplatesShow("crew", true, &shown, &stderr), stderr.String())
	file := filepath.Join(testutil.CanonicalTempDir(t), "crew.json")
	require.NoError(t, os.WriteFile(file, shown.Bytes(), 0o600))
	require.Equal(t, 0, agent.RunTemplatesEdit("crew", file, nil, new(bytes.Buffer), &stderr), stderr.String())

	assert.JSONEq(t, roundTripScopes, templateOwnerScopes(t, f.Mux, "crew"), "the loop keeps the owner scopes")
}

func TestTemplateEdit_AFileThatChangesOwnerScopesChangesThem(t *testing.T) {
	f := newFlow(t)
	haveScopedTemplate(t, f.Mux)
	bridgeAgentClientToMuxWithPeer(t, f.Mux, agentd.AsHumanPeer)

	var shown, stderr bytes.Buffer
	require.Equal(t, 0, agent.RunTemplatesShow("crew", true, &shown, &stderr), stderr.String())
	var edited map[string]any
	require.NoError(t, json.Unmarshal(shown.Bytes(), &edited))
	edited["owner_scopes"] = map[string]any{"groups.members.spawn": map[string]any{"spawn_profile": []string{"p2"}}}
	raw, err := json.Marshal(edited)
	require.NoError(t, err)
	file := filepath.Join(testutil.CanonicalTempDir(t), "crew.json")
	require.NoError(t, os.WriteFile(file, raw, 0o600))
	require.Equal(t, 0, agent.RunTemplatesEdit("crew", file, nil, new(bytes.Buffer), &stderr), stderr.String())

	assert.JSONEq(t, `{"groups.members.spawn":{"spawn_profile":["p2"]}}`, templateOwnerScopes(t, f.Mux, "crew"))
}
