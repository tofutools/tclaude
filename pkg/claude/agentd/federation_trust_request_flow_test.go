package agentd_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/testharness"
)

type fedTrustRequestView struct {
	Peers []struct {
		InstanceID   string `json:"instance_id"`
		Trusted      bool   `json:"trusted"`
		TrustRequest *struct {
			Note       string    `json:"note"`
			TrustsYou  bool      `json:"trusts_you"`
			ReceivedAt time.Time `json:"received_at"`
		} `json:"trust_request"`
		TrustRequestedAt *time.Time `json:"trust_requested_at"`
	} `json:"peers"`
}

func fedTrustRequestStatus(t *testing.T, h *fedHarness) fedTrustRequestView {
	t.Helper()
	rec := fedHuman(t, h.f, http.MethodGet, "/v1/federation/status?summary=1&untrusted=1", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var v fedTrustRequestView
	testharness.DecodeJSON(t, rec, &v)
	return v
}

// TestFederation_TrustRequests: an untrusted hub instance can ask to be
// trusted (shown on its status row, nothing else changes), the operator can
// dismiss it, and this node can ask an instance in turn.
func TestFederation_TrustRequests(t *testing.T) {
	h := newFedHarness(t)
	bob := h.peer.id.ID()
	ask := func(note string, trustsYou bool) *proto.Envelope {
		env, err := proto.NewEnvelope(h.peer.id, proto.KindTrustRequest, proto.Endpoint{}, proto.Endpoint{Instance: h.peer.agentdID}, time.Minute, proto.TrustRequest{Note: note, TrustsYou: trustsYou})
		require.NoError(t, err)
		h.peer.send(env)
		return env
	}

	rec := fedHuman(t, h.f, http.MethodPost, "/v1/federation/peers/untrust", map[string]any{"instance": bob})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Nil(t, fedTrustRequestStatus(t, h).Peers[0].TrustRequest)

	first := ask("hi, it's bob\x1b[31m\non the laptop", true)
	fedEventually(t, "trust request shown", func() bool { return fedTrustRequestStatus(t, h).Peers[0].TrustRequest != nil })
	row := fedTrustRequestStatus(t, h).Peers[0]
	require.Equal(t, bob, row.InstanceID)
	require.False(t, row.Trusted, "asking grants nothing")
	require.Equal(t, "hi, it's bob [31m on the laptop", row.TrustRequest.Note, "control characters are flattened")
	require.True(t, row.TrustRequest.TrustsYou)

	rec = fedHuman(t, h.f, http.MethodPost, "/v1/federation/peers/trust-request/dismiss", map[string]any{"instance": bob})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Nil(t, fedTrustRequestStatus(t, h).Peers[0].TrustRequest)
	// A replay of the dismissed envelope stays dismissed; a new request shows.
	h.peer.send(first)
	time.Sleep(10 * time.Millisecond)
	ask("asking again", false)
	fedEventually(t, "new trust request shown", func() bool { return fedTrustRequestStatus(t, h).Peers[0].TrustRequest != nil })
	time.Sleep(50 * time.Millisecond)
	require.Equal(t, "asking again", fedTrustRequestStatus(t, h).Peers[0].TrustRequest.Note)

	rec = fedHuman(t, h.f, http.MethodPost, "/v1/federation/peers/trust-request", map[string]any{"instance": bob, "note": strings.Repeat("x", 201)})
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	rec = fedHuman(t, h.f, http.MethodPost, "/v1/federation/peers/trust-request", map[string]any{"instance": bob, "note": "alice here"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	fedEventually(t, "bob receives the request", func() bool { return len(h.peer.envelopes(proto.KindTrustRequest)) == 1 })
	var got proto.TrustRequest
	require.NoError(t, h.peer.envelopes(proto.KindTrustRequest)[0].DecodePayload(&got))
	require.Equal(t, proto.TrustRequest{Note: "alice here"}, got, "this node does not trust bob any more")
	require.NotNil(t, fedTrustRequestStatus(t, h).Peers[0].TrustRequestedAt)
	rec = fedHuman(t, h.f, http.MethodPost, "/v1/federation/peers/trust-request", map[string]any{"instance": bob})
	require.Equal(t, http.StatusTooManyRequests, rec.Code, "one request per instance a minute")
	rec = fedHuman(t, h.f, http.MethodPost, "/v1/federation/peers/trust-request", map[string]any{"instance": "inst_nobody"})
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
}
