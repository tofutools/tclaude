package agentd_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/skillbundle"
	"github.com/tofutools/tclaude/pkg/testharness"
	"github.com/tofutools/tclaude/pkg/testutil"
)

func TestManagedInstructionsImportApprovalAndReceiverRules(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("source")
	f.HaveGroup("receiver")
	sourceGroup, err := db.GetAgentGroupByName("source")
	require.NoError(t, err)
	receiverGroup, err := db.GetAgentGroupByName("receiver")
	require.NoError(t, err)
	bundle := skillbundle.Bundle{Format: skillbundle.Format, Name: "review", Files: []skillbundle.File{{Path: "SKILL.md", Data: []byte("Review fixture")}, {Path: "check.sh", Data: []byte("#!/bin/sh\nexit 0\n"), Executable: true}}}
	raw, err := json.Marshal(bundle)
	require.NoError(t, err)
	_, err = db.PutManagedInstruction(sourceGroup.ID, "skill", "review", raw, false)
	require.NoError(t, err)
	_, err = db.PutManagedInstruction(sourceGroup.ID, "rule", "operator", []byte("SOURCE RULE"), false)
	require.NoError(t, err)
	_, err = db.PutManagedInstruction(receiverGroup.ID, "rule", "operator", []byte("RECEIVER RULE"), false)
	require.NoError(t, err)
	source := f.AsHuman().Spawn("source", "worker")
	original, err := db.GetManagedInstructionLaunch(source.AgentID)
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(original.Root) })
	exported := profileReq(t, f, http.MethodGet, "/v1/agent-bundle/export?agent="+source.ConvID+"&format=paf", nil)
	require.Equal(t, 200, exported.Code, exported.Body.String())
	archive := append([]byte(nil), exported.Body.Bytes()...)
	query := url.Values{"group": {"receiver"}, "cwd": {testutil.CanonicalTempDir(t)}}
	call := func(human bool, caller string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/v1/agent-bundle/import?"+query.Encode(), bytes.NewReader(archive))
		if human {
			request = agentd.AsHumanPeer(request)
		} else {
			request = agentd.AsAgentPeer(request, caller)
		}
		return testharness.Serve(f.Mux, request)
	}
	preview := call(true, "")
	require.Equal(t, 200, preview.Code, preview.Body.String())
	var view struct {
		ManagedInstructionDigest string `json:"managed_instruction_digest"`
		ApprovalRequired         bool   `json:"managed_script_approval_required"`
		Items                    []struct {
			CodeText map[string]string `json:"code_text"`
		} `json:"managed_instructions"`
	}
	require.NoError(t, json.Unmarshal(preview.Body.Bytes(), &view))
	require.True(t, view.ApprovalRequired)
	require.NotEmpty(t, view.ManagedInstructionDigest)
	require.Contains(t, preview.Body.String(), "check.sh")
	require.Contains(t, preview.Body.String(), "exit 0")
	query.Set("apply", "true")
	denied := call(true, "")
	require.Equal(t, 409, denied.Code, denied.Body.String())
	query.Set("approve_managed_scripts", "wrong-digest")
	require.Equal(t, 409, call(true, "").Code)
	query.Set("approve_managed_scripts", view.ManagedInstructionDigest)
	require.NoError(t, db.GrantAgentPermission(source.ConvID, agentd.PermAgentBundleImport, "human"))
	denied = call(false, source.ConvID)
	require.Equal(t, 403, denied.Code, denied.Body.String())
	applied := call(true, "")
	require.Equal(t, 200, applied.Code, applied.Body.String())
	var result struct {
		Spawn struct {
			AgentID string `json:"agent_id"`
			ConvID  string `json:"conv_id"`
		} `json:"spawn"`
	}
	require.NoError(t, json.Unmarshal(applied.Body.Bytes(), &result))
	id, err := db.AgentIDForConv(result.Spawn.ConvID)
	require.NoError(t, err)
	landed, err := db.GetManagedInstructionLaunch(id)
	require.NoError(t, err)
	require.NotNil(t, landed)
	t.Cleanup(func() { _ = os.RemoveAll(landed.Root) })
	require.NotEqual(t, original.Root, landed.Root)
	require.Contains(t, landed.Body, "SOURCE RULE")
	require.Contains(t, landed.Body, "RECEIVER RULE")
	receiverLibrary, err := db.ListManagedInstructions(receiverGroup.ID, "skill")
	require.NoError(t, err)
	require.Empty(t, receiverLibrary, "arrived skills must stay agent-local")
	require.NoDirExists(t, os.Getenv("HOME")+"/.claude/skills")
}
