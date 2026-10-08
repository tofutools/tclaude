package agentd_test

import (
	"crypto/ed25519"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func TestFederationAuditPermissionPeerAndSince(t *testing.T) {
	fh := newFedHarness(t)
	// Audit reads do not need a live sender consuming the placeholder below.
	agentd.ResetFederationForTest()
	const conv = "federation-audit-reader"
	fh.f.HaveConvWithTitle(conv, "reader")
	require.NoError(t, db.InsertFederationOutbox(db.FederationOutboxRow{EnvelopeID: "audit-outbound", Kind: "mail", ToInstance: fh.peer.id.ID(), Subject: "secret-subject", BodyPreview: "secret-body", Sealed: []byte("secret-envelope"), ExpiresAt: time.Now().Add(time.Hour)}))
	path := "/v1/federation/audit?peer=bob"
	rec := testharness.Serve(fh.f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodGet, path, nil), conv))
	require.Equal(t, 403, rec.Code, rec.Body.String())
	grant := postPermissionScope(t, fh.f, "grant", map[string]any{"target": conv, "slug": agentd.PermFederationAuditRead})
	require.Equal(t, 200, grant.Code, grant.Body)
	rec = testharness.Serve(fh.f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodGet, path, nil), conv))
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "audit-outbound")
	require.NotContains(t, rec.Body.String(), "secret-")
	rec = fedHuman(t, fh.f, http.MethodGet, path+"&since="+url.QueryEscape(time.Now().Add(time.Hour).Format(time.RFC3339Nano)), nil)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.JSONEq(t, "[]", rec.Body.String())
	for _, query := range []string{"since=bad", "limit=0", "limit=1001"} {
		rec = fedHuman(t, fh.f, http.MethodGet, "/v1/federation/audit?"+query, nil)
		require.Equal(t, 400, rec.Code, rec.Body.String())
	}
	rec = fedHuman(t, fh.f, http.MethodPost, "/v1/federation/peers/untrust", map[string]any{"instance": "bob"})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	rec = fedHuman(t, fh.f, http.MethodGet, "/v1/federation/audit?peer="+fh.peer.id.ID(), nil)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "audit-outbound", "immutable IDs keep retained activity queryable after untrust")

}

func TestFederationOutboxRejectsTruncatedSealedPayloads(t *testing.T) {
	fh := newFedHarness(t)
	for _, size := range []int{0, 15, ed25519.SignatureSize - 1, ed25519.SignatureSize} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			id := fmt.Sprintf("truncated-%d", size)
			require.NoError(t, db.InsertFederationOutbox(db.FederationOutboxRow{
				EnvelopeID: id, Kind: "mail", ToInstance: fh.peer.id.ID(),
				Sealed: make([]byte, size), ExpiresAt: time.Now().Add(time.Hour),
			}))
			agentd.FlushFederationOutboxForTest()
			row, err := db.GetFederationOutbox(id)
			require.NoError(t, err)
			require.Equal(t, db.FedOutboxRefused, row.State)
			require.Equal(t, "corrupt outbox row: truncated sealed envelope", row.LastError)
		})
	}
}
