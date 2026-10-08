package agentd_test

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/client"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func TestFederationIdentitySignedNoticeRebindsOnlyAfterWindow(t *testing.T) {
	fh := newFedHarness(t)
	_, err := config.Update(func(c *config.Config, e error) error {
		if e != nil {
			return e
		}
		c.Federation.IdentityRotationSeconds = 1
		return nil
	})
	require.NoError(t, err)
	next, err := proto.NewIdentity()
	require.NoError(t, err)
	require.NoError(t, db.UpsertFederationPeerGrant(db.FederationPeerGrant{Peer: fh.peer.id.ID(), Slug: "node.read"}))
	r, err := proto.NewRotation(fh.peer.id, next, "", 1, time.Now(), time.Second)
	require.NoError(t, err)
	fh.peer.send(fh.peer.envelope(proto.KindIdentityRotation, proto.Endpoint{}, r))
	fedEventually(t, "rotation visible", func() bool {
		rec := fedHuman(t, fh.f, http.MethodGet, "/v1/federation/identity/rotations", nil)
		return rec.Code == 200 && strings.Contains(rec.Body.String(), "pending")
	})
	old, err := db.GetFederationPeer(fh.peer.id.ID())
	require.NoError(t, err)
	require.NotNil(t, old)
	fedEventually(t, "successor trusted", func() bool { p, _ := db.GetFederationPeer(next.ID()); return p != nil })
	grants, err := db.ListFederationPeerGrants(next.ID())
	require.NoError(t, err)
	require.Len(t, grants, 1)
	old, err = db.GetFederationPeer(fh.peer.id.ID())
	require.NoError(t, err)
	require.Nil(t, old)
	rec := fedHuman(t, fh.f, http.MethodGet, "/v1/federation/audit?peer="+next.ID(), nil)
	require.Equal(t, 200, rec.Code)
	require.Contains(t, rec.Body.String(), "identity.accepted")
}

func TestFederationIdentityOperatorOnlyPreviewAndLocalKeyLossRecovery(t *testing.T) {
	fh := newFedHarness(t)
	fh.f.HaveConvWithTitle("identity-reader", "reader")
	for _, path := range []string{"rotate", "recover-local", "recover", "revoke"} {
		rec := testharness.Serve(fh.f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/federation/identity/"+path, map[string]any{}), "identity-reader"))
		require.Equal(t, 403, rec.Code, rec.Body.String())
	}
	old := fedStatus(t, fh.f).InstanceID
	rec := fedHuman(t, fh.f, http.MethodPost, "/v1/federation/identity/rotate", map[string]any{})
	require.Equal(t, 200, rec.Code)
	require.Contains(t, rec.Body.String(), old)
	require.Equal(t, old, fedStatus(t, fh.f).InstanceID, "preview must not rotate")
	// A previously known local identity must not silently become a new node
	// when the key disappears. Recovery is an explicit operator action.
	agentd.ResetFederationForTest()
	require.NoError(t, os.Remove(agentd.FederationKeyPath()))
	rec = fedHuman(t, fh.f, http.MethodGet, "/v1/federation/status", nil)
	require.NotEqual(t, 200, rec.Code)
	rec = fedHuman(t, fh.f, http.MethodPost, "/v1/federation/identity/recover-local", map[string]any{})
	require.Equal(t, 200, rec.Code)
	require.Contains(t, rec.Body.String(), old)
	rec = fedHuman(t, fh.f, http.MethodPost, "/v1/federation/identity/recover-local", map[string]any{"apply": true})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "re_pair_required")
	status := fedStatus(t, fh.f)
	require.NotEqual(t, old, status.InstanceID)
	peer, err := db.GetFederationPeer(fh.peer.id.ID())
	require.NoError(t, err)
	require.NotNil(t, peer, "remote authority records remain for explicit re-pairing")
}

func TestFederationIdentityLocalRotationAndRetiredKeyRefusal(t *testing.T) {
	fh := newFedHarness(t)
	_, err := config.Update(func(c *config.Config, e error) error {
		if e != nil {
			return e
		}
		c.Federation.IdentityRotationSeconds = 1
		return nil
	})
	require.NoError(t, err)
	old := fedStatus(t, fh.f).InstanceID
	rec := fedHuman(t, fh.f, http.MethodPost, "/v1/federation/identity/rotate", map[string]any{"apply": true})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	fedEventually(t, "new local identity", func() bool {
		r := fedHuman(t, fh.f, http.MethodGet, "/v1/federation/status", nil)
		var status fedStatusView
		return r.Code == 200 && json.Unmarshal(r.Body.Bytes(), &status) == nil && status.InstanceID != old
	})
	rec = fedHuman(t, fh.f, http.MethodGet, "/v1/federation/identity/rotations", nil)
	require.Equal(t, 200, rec.Code)
	require.Contains(t, rec.Body.String(), `"pending":false`)
}

func TestFederationIdentityRecoveryRequiresReplacementFingerprint(t *testing.T) {
	fh := newFedHarness(t)
	next, err := proto.NewIdentity()
	require.NoError(t, err)
	require.NoError(t, fh.store.Admit(next.ID()))
	fh.hub.RefreshPolicy()
	cl, err := client.New(client.Options{URL: fh.url, Identity: next, Name: "replacement"})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go cl.Run(ctx)
	fedEventually(t, "replacement in directory", func() bool {
		for _, p := range fedStatus(t, fh.f).Peers {
			if p.InstanceID == next.ID() {
				return true
			}
		}
		return false
	})
	require.NoError(t, db.UpsertFederationPeerGrant(db.FederationPeerGrant{Peer: fh.peer.id.ID(), Slug: "config.offer"}))
	input := map[string]any{"old": "bob", "new": next.ID()}
	rec := fedHuman(t, fh.f, http.MethodPost, "/v1/federation/identity/recover", input)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "config.offer")
	p, err := db.GetFederationPeer(next.ID())
	require.NoError(t, err)
	require.Nil(t, p)
	input["apply"] = true
	input["fingerprint"] = "wrong"
	rec = fedHuman(t, fh.f, http.MethodPost, "/v1/federation/identity/recover", input)
	require.Equal(t, 400, rec.Code, rec.Body.String())
	input["fingerprint"] = proto.Fingerprint(next.Pub)
	rec = fedHuman(t, fh.f, http.MethodPost, "/v1/federation/identity/recover", input)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	p, err = db.GetFederationPeer(next.ID())
	require.NoError(t, err)
	require.NotNil(t, p)
	require.Equal(t, "bob", p.Label)
	rec = fedHuman(t, fh.f, http.MethodPost, "/v1/federation/identity/revoke", map[string]any{"old": fh.peer.id.ID(), "apply": true})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	p, err = db.GetFederationPeer(next.ID())
	require.NoError(t, err)
	require.NotNil(t, p, "revoking retired predecessor must not roll successor back")
}
