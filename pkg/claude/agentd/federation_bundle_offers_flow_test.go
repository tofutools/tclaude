package agentd_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/configbundle"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/bundletransfer"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/federation/stream"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func fedOfferedConfig(t *testing.T, brief string) []byte {
	t.Helper()
	chunks := []string{}
	for len(brief) > 8000 {
		chunks = append(chunks, brief[:8000])
		brief = brief[8000:]
	}
	b := configbundle.Bundle{Format: configbundle.Format, FormatVersion: 1, CreatedAt: time.Now().UTC().Format(time.RFC3339), TclaudeVersion: "test", Sections: map[string][]configbundle.Item{
		"roles":    {{Name: "offered-role", Value: json.RawMessage(`{"name":"offered-role","brief":` + string(mustJSON(t, brief)) + `}`)}},
		"profiles": {{Name: "offered-profile", Value: json.RawMessage(`{"format":"tclaude-spawn-profiles","format_version":2,"profiles":[{"name":"offered-profile","role_ref":"offered-role"}]}`)}},
	}}
	for i, chunk := range chunks {
		name := fmt.Sprintf("extra-role-%02d", i)
		b.Sections["roles"] = append(b.Sections["roles"], configbundle.Item{Name: name, Value: mustJSON(t, map[string]string{"name": name, "brief": chunk})})
	}
	return mustJSON(t, b)
}
func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	require.NoError(t, err)
	return raw
}
func fedSendOffer(t *testing.T, p *fedPeer, raw []byte) bundletransfer.Descriptor {
	t.Helper()
	d := bundletransfer.New(bundletransfer.Config, raw, "Configuration from Bob", time.Now().Add(time.Hour))
	env := p.envelope(proto.KindBundleOffer, proto.Endpoint{}, d)
	env.From.Agent = ""
	env.ID = d.ID
	p.send(env)
	return d
}
func fedGrantConfig(t *testing.T, fh *fedHarness) {
	t.Helper()
	r := fedHuman(t, fh.f, http.MethodPost, "/v1/federation/grants", map[string]any{"peer": "bob", "slug": "config.offer"})
	require.Equal(t, 200, r.Code, r.Body.String())
}
func TestFederation_ConfigOfferAdmissionPreviewAndSelectiveApply(t *testing.T) {
	fh := newFedHarness(t)
	p, f := fh.peer, fh.f
	f.HaveGroup("private")
	raw := fedOfferedConfig(t, "Read the diff.")
	denied := fedSendOffer(t, p, raw)
	require.Equal(t, proto.AckRefused, fedAckFor(t, p, denied.ID).Status)
	fedGrantConfig(t, fh)
	// An instance receive grant must not accidentally expose private groups.
	rec := fedHuman(t, f, http.MethodGet, "/v1/federation/status", nil)
	require.Equal(t, 200, rec.Code)
	cats := p.envelopes(proto.KindCatalog)
	require.NotEmpty(t, cats)
	var cat proto.CatalogPayload
	require.NoError(t, cats[len(cats)-1].DecodePayload(&cat))
	require.Empty(t, cat.Groups)
	rec = fedHuman(t, f, http.MethodPost, "/v1/federation/grants", map[string]any{"peer": "bob", "slug": "config.offer", "scope": "group=private"})
	require.Equal(t, 400, rec.Code)
	d := fedSendOffer(t, p, raw)
	require.Equal(t, proto.AckAccepted, fedAckFor(t, p, d.ID).Status)
	role, err := db.GetRole("offered-role")
	require.NoError(t, err)
	require.Nil(t, role)
	rec = fedHuman(t, f, http.MethodGet, "/v1/federation/inbox", nil)
	require.Equal(t, 200, rec.Code)
	require.Contains(t, rec.Body.String(), d.ID)
	require.Contains(t, rec.Body.String(), `"offer_state":"ready"`)
	path := "/v1/federation/bundle-offers/" + d.ID + "/import"
	rec = fedHuman(t, f, http.MethodPost, path, map[string]any{})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"security_changes":`)
	require.Contains(t, rec.Body.String(), p.id.ID())
	role, err = db.GetRole("offered-role")
	require.NoError(t, err)
	require.Nil(t, role)
	// Every offer action is operator-only, even if an agent has config.import.
	for _, endpoint := range []string{"/v1/federation/offer-config", "/v1/federation/bundle-offers", path, path[:len(path)-6] + "decline"} {
		method := http.MethodPost
		if endpoint == "/v1/federation/bundle-offers" {
			method = http.MethodGet
		}
		rec = testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, method, endpoint, map[string]any{"peer": "bob"}), "agent-caller"))
		require.Equal(t, 403, rec.Code, endpoint+rec.Body.String())
	}
	rec = fedHuman(t, f, http.MethodPost, path, map[string]any{"only": []string{"roles"}, "apply": true})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	role, err = db.GetRole("offered-role")
	require.NoError(t, err)
	require.NotNil(t, role)
	profile, err := db.GetSpawnProfile("offered-profile")
	require.NoError(t, err)
	require.Nil(t, profile)
	o, err := db.GetFederationBundleOffer("in", p.id.ID(), d.ID)
	require.NoError(t, err)
	require.Equal(t, "applied", o.State)
	require.True(t, o.ResultQueued)
	// Replay is acknowledged but cannot recreate a finished payload or apply it.
	env := p.envelope(proto.KindBundleOffer, proto.Endpoint{}, d)
	env.ID = d.ID
	env.From.Agent = ""
	p.send(env)
	rec = fedHuman(t, f, http.MethodPost, path, nil)
	require.Equal(t, 409, rec.Code)
}
func TestFederation_ConfigOffersUnrestrictedDeclineExpiryAndSendScan(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	setFedTrustLevel(t, fh, "unrestricted")
	d := fedSendOffer(t, p, fedOfferedConfig(t, "Never auto apply."))
	require.Equal(t, proto.AckAccepted, fedAckFor(t, p, d.ID).Status)
	role, err := db.GetRole("offered-role")
	require.NoError(t, err)
	require.Nil(t, role)
	rec := fedHuman(t, f, http.MethodPost, "/v1/federation/bundle-offers/"+d.ID+"/decline", nil)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	o, err := db.GetFederationBundleOffer("in", p.id.ID(), d.ID)
	require.NoError(t, err)
	require.Equal(t, "declined", o.State)
	// Source file flags cannot be forged to bypass a fresh credential scan.
	var bundle configbundle.Bundle
	require.NoError(t, json.Unmarshal(fedOfferedConfig(t, "Use api_key=secretvalue123456789"), &bundle))
	rec = fedHuman(t, f, http.MethodPost, "/v1/federation/offer-config", map[string]any{"peer": "bob", "bundle": bundle})
	require.Equal(t, 422, rec.Code)
	require.NotContains(t, rec.Body.String(), "secretvalue")
	rec = fedHuman(t, f, http.MethodPost, "/v1/federation/offer-config", map[string]any{"peer": "bob", "bundle": bundle, "allow_flagged": true})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var sent struct {
		Offer db.FederationBundleOffer `json:"offer"`
	}
	testharness.DecodeJSON(t, rec, &sent)
	require.NotEmpty(t, sent.Offer.Descriptor.ID)
	fedEventually(t, "outgoing bundle envelope", func() bool { return len(p.envelopes(proto.KindBundleOffer)) > 0 })
	var offered bundletransfer.Descriptor
	require.NoError(t, p.envelopes(proto.KindBundleOffer)[0].DecodePayload(&offered))
	require.Contains(t, string(offered.Inline), "secretvalue")
	// A source records the remote operator's decline and removes payload bytes.
	env := p.envelope(proto.KindBundleResult, proto.Endpoint{}, bundletransfer.Result{Offer: offered.ID, State: "declined"})
	env.From.Agent = ""
	p.send(env)
	require.Equal(t, proto.AckAccepted, fedAckFor(t, p, env.ID).Status)
	o, err = db.GetFederationBundleOffer("out", p.id.ID(), offered.ID)
	require.NoError(t, err)
	require.Equal(t, "declined", o.State)
	// Persisted expiration survives reconnect/restart and is reconciled on list.
	expired := bundletransfer.New(bundletransfer.Config, []byte("payload"), "expired", time.Now().Add(-time.Second))
	_, err = db.InsertFederationBundleOffer(db.FederationBundleOffer{Descriptor: expired, Peer: p.id.ID(), Direction: "in", State: "pending"}, bundletransfer.Config)
	require.NoError(t, err)
	rec = fedHuman(t, f, http.MethodGet, "/v1/federation/bundle-offers?direction=in", nil)
	require.Equal(t, 200, rec.Code)
	o, err = db.GetFederationBundleOffer("in", p.id.ID(), expired.ID)
	require.NoError(t, err)
	require.Equal(t, "expired", o.State)
}
func TestFederation_ConfigOfferLargeFetchVerifiedAndRetryable(t *testing.T) {
	fh := newFedHarness(t)
	p, f := fh.peer, fh.f
	fedGrantConfig(t, fh)
	raw := fedOfferedConfig(t, strings.Repeat("large history-like text ", 16000))
	d := fedSendOffer(t, p, raw)
	require.Empty(t, d.Inline)
	require.Equal(t, proto.AckAccepted, fedAckFor(t, p, d.ID).Status)
	path := "/v1/federation/bundle-offers/" + d.ID + "/import"
	for attempt := 0; attempt < 4; attempt++ {
		done := make(chan *httptest.ResponseRecorder, 1)
		go func() { done <- fedHuman(t, f, http.MethodPost, path, map[string]any{}) }()
		fedEventually(t, "bundle fetch request", func() bool { return len(p.envelopes(proto.KindBundleFetch)) > attempt })
		var req bundletransfer.Request
		require.NoError(t, p.envelopes(proto.KindBundleFetch)[attempt].DecodePayload(&req))
		kp, err := stream.NewKeyPair()
		require.NoError(t, err)
		ans := p.envelope(proto.KindBundleAnswer, proto.Endpoint{}, bundletransfer.Answer{Request: bundletransfer.Request{Offer: req.Offer, Stream: req.Stream, SHA256: req.SHA256, Key: kp.Pub}, OK: true})
		ans.From.Agent = ""
		p.send(ans)
		conn := fedPeerStream(t, p, req.Stream, kp, req.Key, false)
		payload := raw
		if attempt == 1 {
			payload = append([]byte{}, raw...)
			payload[len(payload)-1] ^= 1
		}
		if attempt == 2 {
			payload = raw[:len(raw)-1]
		}
		_, err = io.Copy(conn, bytes.NewReader(payload))
		require.NoError(t, err)
		if attempt == 0 {
			require.NoError(t, conn.Close())
		} else {
			require.NoError(t, conn.CloseWrite())
		}
		select {
		case rec := <-done:
			if attempt < 3 {
				require.Equal(t, 502, rec.Code, rec.Body.String())
				o, err := db.GetFederationBundleOffer("in", p.id.ID(), d.ID)
				require.NoError(t, err)
				require.Equal(t, "pending", o.State)
			} else {
				require.Equal(t, 200, rec.Code, rec.Body.String())
				require.Contains(t, rec.Body.String(), `"security_changes":`)
			}
		case <-time.After(15 * time.Second):
			t.Fatal("bundle fetch did not finish")
		}
		_ = conn.Close()
	}
	role, err := db.GetRole("offered-role")
	require.NoError(t, err)
	require.Nil(t, role, "fetch and preview never apply")
}

func TestFederation_ConfigOfferLargeSourceStream(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	var b configbundle.Bundle
	raw := fedOfferedConfig(t, strings.Repeat("large transferable content ", 16000))
	require.NoError(t, json.Unmarshal(raw, &b))
	rec := fedHuman(t, f, http.MethodPost, "/v1/federation/offer-config", map[string]any{"peer": "bob", "bundle": b})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	fedEventually(t, "large outgoing descriptor", func() bool { return len(p.envelopes(proto.KindBundleOffer)) > 0 })
	var d bundletransfer.Descriptor
	require.NoError(t, p.envelopes(proto.KindBundleOffer)[0].DecodePayload(&d))
	require.Empty(t, d.Inline)
	kp, err := stream.NewKeyPair()
	require.NoError(t, err)
	req := bundletransfer.Request{Offer: d.ID, Stream: proto.NewEnvelopeID(), SHA256: d.SHA256, Key: kp.Pub}
	env := p.envelope(proto.KindBundleFetch, proto.Endpoint{}, req)
	env.From.Agent = ""
	p.send(env)
	var answer bundletransfer.Answer
	fedEventually(t, "source bundle answer", func() bool {
		for _, e := range p.envelopes(proto.KindBundleAnswer) {
			if e.DecodePayload(&answer) == nil && answer.Stream == req.Stream {
				return true
			}
		}
		return false
	})
	require.True(t, answer.OK, answer.Reason)
	conn := fedPeerStream(t, p, req.Stream, kp, answer.Key, true)
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	received, err := io.ReadAll(conn)
	require.NoError(t, err)
	require.NoError(t, d.Verify(received))
	require.Contains(t, string(received), "large transferable content")
}
func TestFederation_ConfigOfferRevokedAdmissionCanStillDecline(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	fedGrantConfig(t, fh)
	d := fedSendOffer(t, p, fedOfferedConfig(t, "Read only."))
	require.Equal(t, proto.AckAccepted, fedAckFor(t, p, d.ID).Status)
	rec := fedHuman(t, f, http.MethodDelete, "/v1/federation/grants", map[string]any{"peer": "bob", "slug": "config.offer"})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	rec = fedHuman(t, f, http.MethodPost, "/v1/federation/bundle-offers/"+d.ID+"/import", nil)
	require.Equal(t, 403, rec.Code)
	rec = fedHuman(t, f, http.MethodPost, "/v1/federation/peers/untrust", map[string]any{"instance": "bob"})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	rec = fedHuman(t, f, http.MethodPost, "/v1/federation/bundle-offers/"+d.ID+"/decline?peer="+p.id.ID(), nil)
	require.Equal(t, 200, rec.Code, rec.Body.String())
}

func TestFederation_ConfigOfferSelectionExcludesOriginalPathsAndScansKeptOnes(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	var b configbundle.Bundle
	require.NoError(t, json.Unmarshal(fedOfferedConfig(t, "Inspect the diff."), &b))
	b.Placeholders = []configbundle.Placeholder{{Name: "excluded_path", Item: "profiles/offered-profile", Field: "value.cwd", Original: "/private/should-not-be-offered"}}
	rec := fedHuman(t, f, http.MethodPost, "/v1/federation/offer-config", map[string]any{"peer": "bob", "bundle": b, "only": []string{"roles"}})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	fedEventually(t, "selected outgoing offer", func() bool { return len(p.envelopes(proto.KindBundleOffer)) > 0 })
	var d bundletransfer.Descriptor
	require.NoError(t, p.envelopes(proto.KindBundleOffer)[0].DecodePayload(&d))
	require.NotContains(t, string(d.Inline), "should-not-be-offered")
	b.Placeholders = append(b.Placeholders, configbundle.Placeholder{Name: "kept_path", Item: "roles/offered-role", Field: "value.path", Original: "/tmp/api_key=privatevalue123456789"})
	rec = fedHuman(t, f, http.MethodPost, "/v1/federation/offer-config", map[string]any{"peer": "bob", "bundle": b, "only": []string{"roles"}})
	require.Equal(t, 422, rec.Code, rec.Body.String())
	require.NotContains(t, rec.Body.String(), "privatevalue")
}
