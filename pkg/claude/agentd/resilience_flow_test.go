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
	"github.com/tofutools/tclaude/pkg/federation/bundletransfer"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/paf/seal"
	"github.com/tofutools/tclaude/pkg/testharness"
	"github.com/tofutools/tclaude/pkg/testutil"
)

const resilienceSource = "agt_bobsource00000000000000000"

func fedSendSnapshot(t *testing.T, p *fedPeer, raw []byte, takenAt time.Time, keep int) bundletransfer.Descriptor {
	t.Helper()
	d := bundletransfer.New(bundletransfer.Snapshot, raw, "Resilience snapshot", time.Now().Add(time.Hour))
	d.Resilience = &bundletransfer.ResilienceIntent{SourceAgent: resilienceSource, AgentName: "worker", Harness: "claude", TakenAt: takenAt, Keep: keep, MaxAgeDays: 7}
	env := p.envelope(proto.KindBundleOffer, proto.Endpoint{}, d)
	env.From.Agent = ""
	env.ID = d.ID
	p.send(env)
	return d
}

func fedSealed(t *testing.T, payload string) []byte {
	t.Helper()
	r, err := seal.Recipient(ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, 32)).Public().(ed25519.PublicKey))
	require.NoError(t, err)
	var out bytes.Buffer
	require.NoError(t, seal.Encrypt(&out, bytes.NewReader([]byte(payload)), r))
	return out.Bytes()
}

type heldView struct {
	Held []struct {
		Peer        string `json:"peer"`
		SourceAgent string `json:"source_agent"`
		Offer       string `json:"offer"`
	} `json:"held"`
}

func fedHeld(t *testing.T, f *testharness.Flow) heldView {
	t.Helper()
	rec := fedHuman(t, f, http.MethodGet, "/v1/resilience/held", nil)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var v heldView
	testharness.DecodeJSON(t, rec, &v)
	return v
}

func TestResilience_PeerHoldsSnapshotsUnderGrantWithRetention(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	now := time.Now().UTC().Truncate(time.Second)

	denied := fedSendSnapshot(t, p, fedSealed(t, "one"), now, 2)
	require.Equal(t, proto.AckRefused, fedAckFor(t, p, denied.ID).Status)

	rec := fedHuman(t, f, http.MethodPost, "/v1/federation/grants", map[string]any{"peer": "bob", "slug": "snapshots.receive"})
	require.Equal(t, 200, rec.Code, rec.Body.String())

	first := fedSendSnapshot(t, p, fedSealed(t, "one"), now.Add(-2*time.Hour), 2)
	require.Equal(t, proto.AckAccepted, fedAckFor(t, p, first.ID).Status)
	fedEventually(t, "first snapshot held", func() bool { return len(fedHeld(t, f).Held) == 1 })
	held := fedHeld(t, f).Held[0]
	require.Equal(t, p.id.ID(), held.Peer)
	require.Equal(t, resilienceSource, held.SourceAgent)
	// The sender learns its snapshot is kept.
	fedEventually(t, "applied result to the sender", func() bool {
		for _, e := range p.envelopes(proto.KindBundleResult) {
			var r bundletransfer.Result
			if e.DecodePayload(&r) == nil && r.Offer == first.ID && r.State == "applied" {
				return true
			}
		}
		return false
	})

	// A snapshot is never imported or opened through the generic routes.
	for _, path := range []string{"/v1/federation/bundle-offers/" + first.ID + "/import", "/v1/federation/bundle-offers/" + first.ID + "/contents"} {
		method := http.MethodPost
		if path[len(path)-8:] == "contents" {
			method = http.MethodGet
		}
		rec = fedHuman(t, f, method, path, map[string]any{})
		require.Equal(t, 409, rec.Code, path+" "+rec.Body.String())
		require.Contains(t, rec.Body.String(), "resilience_snapshot")
	}

	// Keep 2: the oldest goes when a third arrives.
	second := fedSendSnapshot(t, p, fedSealed(t, "two"), now.Add(-time.Hour), 2)
	require.Equal(t, proto.AckAccepted, fedAckFor(t, p, second.ID).Status)
	fedEventually(t, "second snapshot held", func() bool { return len(fedHeld(t, f).Held) == 2 })
	third := fedSendSnapshot(t, p, fedSealed(t, "three"), now, 2)
	require.Equal(t, proto.AckAccepted, fedAckFor(t, p, third.ID).Status)
	fedEventually(t, "oldest snapshot pruned", func() bool {
		h := fedHeld(t, f).Held
		return len(h) == 2 && h[0].Offer == third.ID && h[1].Offer == second.ID
	})

	// A plaintext payload is never kept.
	plain := fedSendSnapshot(t, p, []byte("PK\x03\x04 not encrypted"), now.Add(time.Minute), 2)
	fedAckFor(t, p, plain.ID)
	fedEventually(t, "plaintext snapshot declined", func() bool {
		o, _ := db.GetFederationBundleOffer("in", p.id.ID(), plain.ID)
		return o == nil || o.State == "declined"
	})
	require.Len(t, fedHeld(t, f).Held, 2)

	// Custody is the operator's to inspect.
	rec = testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/resilience/held", nil), "agent-caller"))
	require.Equal(t, 403, rec.Code)
}

type resiliencePoliciesView struct {
	Policies []struct {
		ID     string `json:"id"`
		Agents []struct {
			Agent      string `json:"agent"`
			Pending    string `json:"pending_offer"`
			Deferred   string `json:"deferred"`
			LastError  string `json:"last_error"`
			LastDigest string `json:"last_digest"`
			Failures   int    `json:"failures"`
		} `json:"agents"`
	} `json:"policies"`
}

func resilienceRun(t *testing.T, f *testharness.Flow, id string) resiliencePoliciesView {
	t.Helper()
	rec := fedHuman(t, f, http.MethodPost, "/v1/resilience/policies/"+id+"/run", nil)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var v resiliencePoliciesView
	testharness.DecodeJSON(t, rec, &v)
	require.Len(t, v.Policies, 1)
	require.Len(t, v.Policies[0].Agents, 1)
	return v
}

func TestResilience_PolicySendsSealedSnapshotsToPeer(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	const conv = "019fe740-43a4-7023-b8ae-1ee64459f2c1"
	f.HaveAliveSession(conv, "resilient", "resilient-pane", testutil.CanonicalTempDir(t))
	f.HaveEnrolledAgent(conv)

	// Only the operator sets policies.
	body := map[string]any{"target": conv, "every": "1h", "to": "peer:bob", "keep": 3}
	rec := testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/resilience/policies", body), "agent-caller"))
	require.Equal(t, 403, rec.Code)
	rec = fedHuman(t, f, http.MethodPost, "/v1/resilience/policies", map[string]any{"target": conv, "every": "1m", "to": "peer:bob", "keep": 3})
	require.Equal(t, 400, rec.Code)
	rec = fedHuman(t, f, http.MethodPost, "/v1/resilience/policies", map[string]any{"target": conv, "every": "1h", "to": "board:team", "keep": 3})
	require.Equal(t, 400, rec.Code)
	rec = fedHuman(t, f, http.MethodPost, "/v1/resilience/policies", body)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var policy struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &policy))

	// A busy agent is deferred, not snapshotted mid-turn.
	v := resilienceRun(t, f, policy.ID)
	require.Equal(t, "busy", v.Policies[0].Agents[0].Deferred)

	// A peer that has not advertised the capability is a visible failure.
	f.SetSessionStatus(conv, "idle")
	v = resilienceRun(t, f, policy.ID)
	require.Contains(t, v.Policies[0].Agents[0].LastError, "does not accept resilience snapshots")
	require.Equal(t, 1, v.Policies[0].Agents[0].Failures)

	p.send(p.envelope(proto.KindCatalog, proto.Endpoint{}, proto.CatalogPayload{ResilienceSnapshots: 1}))
	fedEventually(t, "peer catalog stored", func() bool {
		raw, _, err := db.GetFederationCatalog(p.id.ID())
		return err == nil && bytes.Contains([]byte(raw), []byte(`"resilience_snapshots":1`))
	})
	v = resilienceRun(t, f, policy.ID)
	st := v.Policies[0].Agents[0]
	require.Empty(t, st.LastError)
	require.NotEmpty(t, st.Pending)

	var offered bundletransfer.Descriptor
	fedEventually(t, "snapshot offer sent", func() bool {
		for _, e := range p.envelopes(proto.KindBundleOffer) {
			if e.DecodePayload(&offered) == nil && offered.ID == st.Pending {
				return true
			}
		}
		return false
	})
	require.Equal(t, bundletransfer.Snapshot.Name, offered.Type)
	require.NotNil(t, offered.Resilience)
	require.Equal(t, 3, offered.Resilience.Keep)
	a, err := db.GetAgent(offered.Resilience.SourceAgent)
	require.NoError(t, err)
	require.Equal(t, conv, a.CurrentConvID)
	if offered.Inline != nil {
		require.True(t, seal.IsEncrypted(offered.Inline), "the snapshot leaves sealed")
	}

	// The peer keeps it; the next run sees nothing new and sends nothing.
	receipt := p.envelope(proto.KindBundleResult, proto.Endpoint{}, bundletransfer.Result{Offer: offered.ID, State: "applied"})
	receipt.From.Agent = ""
	p.send(receipt)
	require.Equal(t, proto.AckAccepted, fedAckFor(t, p, receipt.ID).Status)
	v = resilienceRun(t, f, policy.ID)
	st = v.Policies[0].Agents[0]
	require.Empty(t, st.Pending)
	require.NotEmpty(t, st.LastDigest)
	require.Zero(t, st.Failures)
	require.Len(t, p.envelopes(proto.KindBundleOffer), 1)

	rec = fedHuman(t, f, http.MethodDelete, "/v1/resilience/policies/"+policy.ID, nil)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	rec = fedHuman(t, f, http.MethodGet, "/v1/resilience/policies", nil)
	require.Equal(t, 200, rec.Code)
	require.Contains(t, rec.Body.String(), `"policies":[]`)
}
