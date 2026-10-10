package agentd_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func TestDashboardIdentityRotatePreviewIsReadOnly(t *testing.T) {
	fh := newFedHarness(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://localhost:12345"))
	key, err := os.ReadFile(agentd.FederationKeyPath())
	require.NoError(t, err)
	before := fedStatus(t, fh.f).InstanceID
	h := agentd.BuildDashboardHandlerForTest()
	rec := testharness.Serve(h, testharness.JSONRequest(t, "POST", "/api/federation/identity/rotate", map[string]any{}))
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.Equal(t, "private, no-store", rec.Header().Get("Cache-Control"))
	var preview struct {
		InstanceID    string  `json:"instance_id"`
		Fingerprint   string  `json:"fingerprint"`
		WindowSeconds float64 `json:"window_seconds"`
		HopCount      int     `json:"hop_count"`
		HopLimit      int     `json:"hop_limit"`
		Pending       bool
		Effects       map[string]bool
	}
	testharness.DecodeJSON(t, rec, &preview)
	require.Equal(t, before, preview.InstanceID)
	require.NotEmpty(t, preview.Fingerprint)
	require.Positive(t, preview.WindowSeconds)
	require.Equal(t, 0, preview.HopCount)
	require.Equal(t, proto.MaxRotationHops, preview.HopLimit)
	require.False(t, preview.Pending)
	for _, effect := range []string{"successor_linked", "streams_reconnect", "pending_sealed_mail_requires_resend", "issued_model_credentials_revoked", "requester_paid_leases_revoked"} {
		require.True(t, preview.Effects[effect], effect)
	}
	after, err := os.ReadFile(agentd.FederationKeyPath())
	require.NoError(t, err)
	require.True(t, bytes.Equal(key, after))
	require.Equal(t, before, fedStatus(t, fh.f).InstanceID)
	rec = testharness.Serve(h, testharness.JSONRequest(t, "GET", "/api/federation/identity/rotations", nil))
	require.Equal(t, 200, rec.Code)
	var state struct {
		Local struct {
			Chain   []proto.Rotation
			Pending bool
		}
	}
	testharness.DecodeJSON(t, rec, &state)
	require.Empty(t, state.Local.Chain)
	require.False(t, state.Local.Pending, "preview must not stage a key")
}

func TestDashboardIdentityRotateApplyStagesLinkedSuccessor(t *testing.T) {
	fh := newFedHarness(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://localhost:12345"))
	_, err := config.Update(func(c *config.Config, err error) error {
		if err != nil {
			return err
		}
		c.Federation.IdentityRotationSeconds = 3600
		return nil
	})
	require.NoError(t, err)
	before := fedStatus(t, fh.f).InstanceID
	h := agentd.BuildDashboardHandlerForTest()
	rec := testharness.Serve(h, testharness.JSONRequest(t, "POST", "/api/federation/identity/rotate", map[string]any{"apply": true}))
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.Equal(t, "private, no-store", rec.Header().Get("Cache-Control"))
	var result struct {
		proto.Rotation
		OldFingerprint string `json:"old_fingerprint"`
		NewFingerprint string `json:"new_fingerprint"`
	}
	testharness.DecodeJSON(t, rec, &result)
	require.Equal(t, before, result.OldID)
	require.NotEqual(t, before, result.NewID)
	require.Equal(t, 1, result.Sequence)
	require.Equal(t, proto.Fingerprint(result.OldKey), result.OldFingerprint)
	require.Equal(t, proto.Fingerprint(result.NewKey), result.NewFingerprint)
	require.NoError(t, proto.VerifyRotationChain([]proto.Rotation{result.Rotation}, before, result.OldKey, result.NewID))
	require.Equal(t, float64(3600), result.ActivateAt.Sub(result.IssuedAt).Seconds())
	rec = testharness.Serve(h, testharness.JSONRequest(t, "GET", "/api/federation/identity/rotations", nil))
	require.Equal(t, 200, rec.Code)
	var state struct {
		Local struct {
			Chain []struct {
				proto.Rotation
				NewFingerprint string `json:"new_fingerprint"`
			}
			Pending bool
		}
	}
	testharness.DecodeJSON(t, rec, &state)
	require.True(t, state.Local.Pending)
	require.Len(t, state.Local.Chain, 1)
	require.Equal(t, result.NewID, state.Local.Chain[0].NewID)
	require.Equal(t, result.NewFingerprint, state.Local.Chain[0].NewFingerprint)
	require.Equal(t, result.ActivateAt, state.Local.Chain[0].ActivateAt)
	rec = testharness.Serve(h, testharness.JSONRequest(t, "POST", "/api/federation/identity/rotate", map[string]any{}))
	require.Equal(t, 200, rec.Code)
	var preview map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &preview))
	require.Equal(t, true, preview["pending"])
	require.Equal(t, float64(1), preview["hop_count"])
	fedEventually(t, "rotation runtime reconnected", func() bool { status := fedStatus(t, fh.f); return status.Hub != nil && status.Hub.State == "connected" })
	rec = testharness.Serve(h, testharness.JSONRequest(t, "POST", "/api/federation/identity/rotate", map[string]any{"apply": true}))
	require.Equal(t, 409, rec.Code)
	require.Contains(t, rec.Body.String(), "rotation is already pending")
}

func TestDashboardIdentityRotateRefusesPeersAndNonHumans(t *testing.T) {
	fh := newFedHarness(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://localhost:12345"))
	peer, err := db.GetFederationPeer(fh.peer.id.ID())
	require.NoError(t, err)
	peer.TrustLevel = db.FederationTrustUnrestricted
	require.NoError(t, db.TrustFederationPeer(*peer))
	fh.f.HaveConvWithTitle("identity-agent", "reader")
	before := fedStatus(t, fh.f).InstanceID
	raw := http.NewServeMux()
	agentd.RegisterDashboardRoutesForTest(raw)
	for _, apply := range []bool{false, true} {
		body := map[string]any{"apply": apply}
		rec := testharness.Serve(agentd.PeerViewHandler(peer.InstanceID), testharness.JSONRequest(t, "POST", "/api/federation/identity/rotate", body))
		require.Equal(t, 403, rec.Code, rec.Body.String())
		rec = testharness.Serve(raw, testharness.JSONRequest(t, "POST", "/api/federation/identity/rotate", body))
		require.Equal(t, 403, rec.Code, rec.Body.String())
		rec = testharness.Serve(fh.f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, "POST", "/v1/federation/identity/rotate", body), "identity-agent"))
		require.Equal(t, 403, rec.Code, rec.Body.String())
	}
	require.Equal(t, before, fedStatus(t, fh.f).InstanceID)
	rec := testharness.Serve(agentd.BuildDashboardHandlerForTest(), testharness.JSONRequest(t, "GET", "/api/federation/identity/rotations", nil))
	require.Equal(t, 200, rec.Code)
	require.NotContains(t, rec.Body.String(), `"pending":true`)
}
