package agentd_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/harnesscredentials"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func TestStandaloneCredentialsReceiveWithoutInstallAndRestore(t *testing.T) {
	fh := newFedHarness(t)
	home, err := filepath.EvalSymlinks(os.Getenv("HOME"))
	require.NoError(t, err)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	h := agentd.PeerViewHandler(fh.peer.id.ID())
	payload := map[string]any{"harness": "codex", "confirm_share": true, "credentials": harnesscredentials.Bundle{Harness: "codex", Files: []harnesscredentials.File{{Name: "auth.json", Data: []byte(`{"token":"fixture-remote-secret"}`)}}}}
	rec := testharness.Serve(h, testharness.JSONRequest(t, "POST", "/api/harnesses/credentials/push", payload))
	require.Equal(t, 403, rec.Code)
	require.NoError(t, db.UpsertFederationPeerGrant(db.FederationPeerGrant{Peer: fh.peer.id.ID(), Slug: agentd.PermNodeHarnessesInstall}))
	rec = testharness.Serve(h, testharness.JSONRequest(t, "POST", "/api/harnesses/credentials/push", payload))
	require.Equal(t, 403, rec.Code, "install alone cannot receive credentials")
	_, err = db.DeleteFederationPeerGrant(fh.peer.id.ID(), agentd.PermNodeHarnessesInstall, "")
	require.NoError(t, err)
	require.NoError(t, db.UpsertFederationPeerGrant(db.FederationPeerGrant{Peer: fh.peer.id.ID(), Slug: agentd.PermNodeCredentialsReceive}))
	rec = testharness.Serve(h, testharness.JSONRequest(t, "POST", "/api/harnesses/credentials/push", payload))
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.NotContains(t, rec.Body.String(), "fixture-remote-secret")
	var reply struct {
		Receipt harnesscredentials.Receipt `json:"receipt"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &reply))
	require.True(t, reply.Receipt.Copied)
	path := filepath.Join(home, ".codex", "auth.json")
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Contains(t, string(raw), "fixture-remote-secret")
	rec = testharness.Serve(h, testharness.JSONRequest(t, "GET", "/api/harnesses/credentials/backups?harness=codex", nil))
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), reply.Receipt.BackupID)
	require.NotContains(t, rec.Body.String(), "fixture-remote-secret")
	rec = testharness.Serve(h, testharness.JSONRequest(t, "POST", "/api/harnesses/credentials/restore", map[string]any{"harness": "codex", "backup": reply.Receipt.BackupID}))
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"restored":true`)
	_, err = os.Stat(path)
	require.True(t, os.IsNotExist(err))
	rec = fedHuman(t, fh.f, "POST", "/v1/harnesses/credentials/push", payload)
	require.Equal(t, 400, rec.Code)
	require.NotContains(t, rec.Body.String(), "fixture-remote-secret")
	rec = testharness.Serve(fh.f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, "POST", "/v1/harnesses/credentials/backup", map[string]any{"harness": "codex"}), "reader"))
	require.Equal(t, 403, rec.Code)
	rec = fedHuman(t, fh.f, "POST", "/v1/harnesses/credentials/backup", map[string]any{"harness": "codex"})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	rec = fedHuman(t, fh.f, "GET", "/v1/federation/audit", nil)
	require.Equal(t, 200, rec.Code)
	require.Contains(t, rec.Body.String(), "federation.credentials.push")
	require.NotContains(t, rec.Body.String(), "fixture-remote-secret")
}
