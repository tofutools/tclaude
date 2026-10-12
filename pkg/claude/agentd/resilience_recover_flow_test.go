package agentd_test

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/paf"
	"github.com/tofutools/tclaude/pkg/paf/privacy"
	"github.com/tofutools/tclaude/pkg/paf/seal"
	"github.com/tofutools/tclaude/pkg/testharness"
	"github.com/tofutools/tclaude/pkg/testutil"
)

// fedPeerSnapshot builds what bob's resilience policy would send: a PAF of
// one agent signed by signer, sealed to this node's key.
func fedPeerSnapshot(t *testing.T, fh *fedHarness, conv string, signer *proto.Identity) []byte {
	t.Helper()
	rec := fedHuman(t, fh.f, http.MethodGet, "/v1/agent-bundle/export?agent="+conv+"&history=true&format=paf", nil)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	raw := rec.Body.Bytes()
	a, err := paf.Open(bytes.NewReader(raw), int64(len(raw)), paf.Options{})
	require.NoError(t, err)
	var signed bytes.Buffer
	_, err = privacy.AddConsent(a, &signed, privacy.ConsentRequest{By: "test", Audience: privacy.AudienceSelf}, paf.DefaultLimits(), paf.Producer{Name: "test", Version: "0"},
		paf.SigningKey{Key: signer.Priv, KeyID: signer.ID(), Signer: &paf.Signer{Kind: "instance"}})
	require.NoError(t, err)
	key, ok := fh.peer.cl.LookupKey(fh.peer.agentdID)
	require.True(t, ok)
	r, err := seal.Recipient(ed25519.PublicKey(key))
	require.NoError(t, err)
	var sealed bytes.Buffer
	require.NoError(t, seal.Encrypt(&sealed, bytes.NewReader(signed.Bytes()), r))
	return sealed.Bytes()
}

func TestResilience_RecoverHeldSnapshotKeepsAgentID(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	f.HaveGroup("recovered")
	const conv = "019fe740-43a4-7023-b8ae-1ee64459f2d3"
	f.HaveAliveSession(conv, "lost", "lost-pane", testutil.CanonicalTempDir(t))
	rec := fedHuman(t, f, http.MethodPost, "/v1/federation/grants", map[string]any{"peer": "bob", "slug": "snapshots.receive"})
	require.Equal(t, 200, rec.Code, rec.Body.String())

	now := time.Now().UTC().Truncate(time.Second)
	good := fedSendSnapshot(t, p, fedPeerSnapshot(t, fh, conv, p.id), now, 5)
	require.Equal(t, proto.AckAccepted, fedAckFor(t, p, good.ID).Status)
	stranger, err := proto.NewIdentity()
	require.NoError(t, err)
	forged := fedSendSnapshot(t, p, fedPeerSnapshot(t, fh, conv, stranger), now.Add(-time.Hour), 5)
	require.Equal(t, proto.AckAccepted, fedAckFor(t, p, forged.ID).Status)
	older := fedSendSnapshot(t, p, fedPeerSnapshot(t, fh, conv, p.id), now.Add(-2*time.Hour), 5)
	require.Equal(t, proto.AckAccepted, fedAckFor(t, p, older.ID).Status)
	fedEventually(t, "snapshots held", func() bool { return len(fedHeld(t, f).Held) == 3 })

	path := func(offer, op string) string { return "/v1/resilience/held/bob/" + offer + "/" + op }
	body := map[string]any{"group": "recovered", "name": "recovered-agent", "cwd": testutil.CanonicalTempDir(t)}

	// Only the operator recovers, and only into a named group.
	rec = testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, path(good.ID, "preview"), body), "agent-caller"))
	require.Equal(t, 403, rec.Code)
	rec = fedHuman(t, f, http.MethodPost, path(good.ID, "preview"), map[string]any{})
	require.Equal(t, 400, rec.Code)
	// The snapshot must come from the instance that sent it.
	rec = fedHuman(t, f, http.MethodPost, path(forged.ID, "preview"), body)
	require.Equal(t, 422, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "provenance")

	// Keeping the id of an older snapshot than one held is a question for
	// the operator, answered with force_keep_id or new_id.
	rec = fedHuman(t, f, http.MethodPost, path(older.ID, "preview"), body)
	require.Equal(t, 409, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "identity_conflict")
	require.Contains(t, rec.Body.String(), "newer snapshot")
	forced := map[string]any{"group": "recovered", "cwd": body["cwd"], "force_keep_id": true}
	rec = fedHuman(t, f, http.MethodPost, path(older.ID, "preview"), forced)
	require.Equal(t, 200, rec.Code, rec.Body.String())

	rec = fedHuman(t, f, http.MethodPost, path(good.ID, "preview"), body)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var preview struct {
		Token    string `json:"preview_token"`
		Snapshot struct {
			Identity    string `json:"identity"`
			SourceAgent string `json:"source_agent"`
		} `json:"snapshot"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &preview))
	require.NotEmpty(t, preview.Token)
	require.Equal(t, "keep", preview.Snapshot.Identity)

	// The token binds the options it was issued for.
	other := map[string]any{"group": "recovered", "name": "other-name", "cwd": body["cwd"], "preview_token": preview.Token}
	rec = fedHuman(t, f, http.MethodPost, path(good.ID, "import"), other)
	require.Equal(t, 409, rec.Code, rec.Body.String())
	rec = fedHuman(t, f, http.MethodPost, path(good.ID, "preview"), body)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &preview))
	body["preview_token"] = preview.Token
	rec = fedHuman(t, f, http.MethodPost, path(good.ID, "import"), body)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var landed struct {
		Applied bool `json:"applied"`
		Spawn   struct {
			ConvID string `json:"conv_id"`
		} `json:"spawn"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &landed))
	require.True(t, landed.Applied, rec.Body.String())
	// A token is single-use.
	rec = fedHuman(t, f, http.MethodPost, path(good.ID, "import"), body)
	require.Equal(t, 409, rec.Code, rec.Body.String())

	require.NotEqual(t, conv, landed.Spawn.ConvID)
	got, err := db.AgentIDForConv(landed.Spawn.ConvID)
	require.NoError(t, err)
	require.Equal(t, resilienceSource, got, "a recovery keeps the agent id by default")

	// The agent now runs here: keeping its id again is refused even with
	// force; a new id is the way.
	rec = fedHuman(t, f, http.MethodPost, path(good.ID, "preview"), forced)
	require.Equal(t, 409, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "is running on this node")
	// Stopped, it may be taken over with force: the recovered conversation
	// becomes its current one.
	f.Stop(landed.Spawn.ConvID, true)
	rec = fedHuman(t, f, http.MethodPost, path(good.ID, "preview"), body)
	require.Equal(t, 409, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "exists on this node")
	rec = fedHuman(t, f, http.MethodPost, path(good.ID, "preview"), forced)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &preview))
	forced["preview_token"] = preview.Token
	rec = fedHuman(t, f, http.MethodPost, path(good.ID, "import"), forced)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	first := landed.Spawn.ConvID
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &landed))
	require.NotEqual(t, first, landed.Spawn.ConvID)
	a, err := db.GetAgent(resilienceSource)
	require.NoError(t, err)
	require.Equal(t, landed.Spawn.ConvID, a.CurrentConvID, "the kept id now heads the recovered conversation")

	fresh := map[string]any{"group": "recovered", "name": "second", "cwd": body["cwd"], "new_id": true}
	rec = fedHuman(t, f, http.MethodPost, path(good.ID, "preview"), fresh)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &preview))
	require.Equal(t, "new", preview.Snapshot.Identity)
	fresh["preview_token"] = preview.Token
	rec = fedHuman(t, f, http.MethodPost, path(good.ID, "import"), fresh)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &landed))
	got, err = db.AgentIDForConv(landed.Spawn.ConvID)
	require.NoError(t, err)
	require.NotEmpty(t, got)
	require.NotEqual(t, resilienceSource, got)
}
