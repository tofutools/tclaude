package agentd_test

import (
	"encoding/json"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/agentbundle"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testutil"
)

// These pin the import refusals no other flow test reaches, before
// handleAgentBundleImport is split into steps: each is refused with its
// status, code and message, and none of them lands an agent.
func TestAgentBundleImport_RefusalsLandNothing(t *testing.T) {
	cwd := testutil.CanonicalTempDir(t)
	at := "&cwd=" + url.QueryEscape(cwd)
	for _, tc := range []struct {
		name    string
		query   string
		bundle  func(*agentbundle.Bundle)
		status  int
		code    string
		message string
	}{
		{"a binding without a value", "set=NOVALUE" + at, nil,
			400, "binding", "--set requires name=value; HOME is automatic"},
		{"a HOME binding", "set=HOME=/elsewhere" + at, nil,
			400, "binding", "--set requires name=value; HOME is automatic"},
		{"too many carried permissions", "carry_permissions=true" + at,
			func(b *agentbundle.Bundle) {
				b.Manifest.Agent.CarryPermissions = true
				b.Manifest.Agent.Permissions = make([]agentbundle.Permission, 513)
			}, 400, "permissions", "permission carry exceeds 512 rows"},
		{"unresolved paths", "apply=true&group=receiver&keep_paths=true",
			func(b *agentbundle.Bundle) { b.Manifest.Agent.Paths.Cwd = "/missing/portable/project" },
			409, "unresolved_paths", "resolve paths before applying"},
		{"apply without a group", "apply=true" + at, nil,
			400, "group", "--apply requires --group naming an existing receiving group, as with agent spawn"},
		{"apply into a missing group", "apply=true&group=nowhere" + at, nil,
			404, "group", "receiving group does not exist"},
		{"offline without a conversation", "apply=true&group=receiver&start=false" + at, nil,
			400, "no_start", "only an agent whose conversation lands can be imported without starting; import and start it instead"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFlow(t)
			f.HaveGroup("receiver")
			b := &agentbundle.Bundle{Manifest: agentbundle.Manifest{
				Format: agentbundle.Format, FormatVersion: 1,
				Agent: agentbundle.Definition{Name: "portable", Harness: "claude",
					Profile: json.RawMessage(`{}`), Paths: agentbundle.Paths{Cwd: cwd}},
			}}
			if tc.bundle != nil {
				tc.bundle(b)
			}
			agentsBefore, err := db.ListActiveAgents()
			require.NoError(t, err)

			rec := postAgentBundle(t, f, b, tc.query, "")

			require.Equal(t, tc.status, rec.Code, rec.Body.String())
			var body struct {
				Code  string `json:"code"`
				Error string `json:"error"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())
			assert.Equal(t, tc.code, body.Code)
			assert.Equal(t, tc.message, body.Error)
			agentsAfter, err := db.ListActiveAgents()
			require.NoError(t, err)
			assert.Len(t, agentsAfter, len(agentsBefore), "a refused import lands no agent")
		})
	}
}
