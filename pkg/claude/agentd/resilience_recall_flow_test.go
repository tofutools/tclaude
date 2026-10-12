package agentd_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/bundletransfer"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/testharness"
	"github.com/tofutools/tclaude/pkg/testutil"
)

// The custodian returns a held snapshot only to the instance it holds it
// for, and only one it holds.
func TestResilience_CustodianReturnsHeldSnapshotOnRecall(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	rec := fedHuman(t, f, http.MethodPost, "/v1/federation/grants", map[string]any{"peer": "bob", "slug": "snapshots.receive"})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	held := fedSendSnapshot(t, p, fedSealed(t, "bob's agent"), time.Now().UTC().Truncate(time.Second), 3)
	require.Equal(t, proto.AckAccepted, fedAckFor(t, p, held.ID).Status)
	fedEventually(t, "snapshot held", func() bool { return len(fedHeld(t, f).Held) == 1 })

	recall := func(agent string) *proto.Envelope {
		env := p.envelope(proto.KindResilienceRecall, proto.Endpoint{}, proto.ResilienceRecallPayload{Recall: proto.NewEnvelopeID(), SourceAgent: agent})
		env.From.Agent = ""
		p.send(env)
		return env
	}
	missing := recall("agt_notheld000000000000000000")
	ack := fedAckFor(t, p, missing.ID)
	require.Equal(t, proto.AckRefused, ack.Status)
	require.Equal(t, "not_held", ack.Code)

	asked := recall(resilienceSource)
	require.Equal(t, proto.AckAccepted, fedAckFor(t, p, asked.ID).Status)
	var returned bundletransfer.Descriptor
	fedEventually(t, "returned snapshot offered", func() bool {
		for _, e := range p.envelopes(proto.KindBundleOffer) {
			if e.DecodePayload(&returned) == nil && returned.Type == bundletransfer.SnapshotReturn.Name {
				return true
			}
		}
		return false
	})
	var payload proto.ResilienceRecallPayload
	require.NoError(t, asked.DecodePayload(&payload))
	require.NotNil(t, returned.Return)
	require.Equal(t, payload.Recall, returned.Return.Recall)
	require.Equal(t, held.ID, returned.Return.Offer)
	require.Equal(t, held.SHA256, returned.SHA256, "the held ciphertext goes back unchanged")
}

// A rebuilt origin recalls its snapshot, admits exactly the matching return
// once, and recovers from it.
func TestResilience_OriginRecallsAndRecovers(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	f.HaveGroup("recovered")
	const conv = "019fe740-43a4-7023-b8ae-1ee64459f2e7"
	f.HaveAliveSession(conv, "origin", "origin-pane", testutil.CanonicalTempDir(t))
	f.HaveEnrolledAgent(conv)
	f.SetSessionStatus(conv, "idle")
	agentID, err := db.AgentIDForConv(conv)
	require.NoError(t, err)

	// The policy's snapshot is what bob holds.
	p.send(p.envelope(proto.KindCatalog, proto.Endpoint{}, proto.CatalogPayload{ResilienceSnapshots: 1}))
	fedEventually(t, "peer catalog stored", func() bool {
		raw, _, _ := db.GetFederationCatalog(p.id.ID())
		return containsJSON(raw, `"resilience_snapshots":1`)
	})
	rec := fedHuman(t, f, http.MethodPost, "/v1/resilience/policies", map[string]any{"target": conv, "every": "1h", "to": "peer:bob", "keep": 3})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var policy struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &policy))
	resilienceRun(t, f, policy.ID)
	var sent bundletransfer.Descriptor
	fedEventually(t, "snapshot sent", func() bool {
		for _, e := range p.envelopes(proto.KindBundleOffer) {
			if e.DecodePayload(&sent) == nil && sent.Type == bundletransfer.Snapshot.Name {
				return true
			}
		}
		return false
	})
	require.NotEmpty(t, sent.Inline)

	// A peer that cannot return snapshots is told so.
	ask := map[string]any{"from": "peer:bob", "source_agent": agentID}
	rec = fedHuman(t, f, http.MethodPost, "/v1/resilience/recalls", ask)
	require.Equal(t, 409, rec.Code, rec.Body.String())
	p.send(p.envelope(proto.KindCatalog, proto.Endpoint{}, proto.CatalogPayload{ResilienceSnapshots: 2}))
	fedEventually(t, "peer catalog v2", func() bool {
		raw, _, _ := db.GetFederationCatalog(p.id.ID())
		return containsJSON(raw, `"resilience_snapshots":2`)
	})
	rec = testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/resilience/recalls", ask), "agent-caller"))
	require.Equal(t, 403, rec.Code)
	rec = fedHuman(t, f, http.MethodPost, "/v1/resilience/recalls", ask)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var recall struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &recall))
	fedEventually(t, "recall reaches the custodian", func() bool { return len(p.envelopes(proto.KindResilienceRecall)) == 1 })

	sendReturn := func(recallID, agent string, raw []byte) bundletransfer.Descriptor {
		d := bundletransfer.New(bundletransfer.SnapshotReturn, raw, "Returned", time.Now().Add(time.Hour))
		d.Return = &bundletransfer.ReturnIntent{Recall: recallID, SourceAgent: agent, Offer: sent.ID, TakenAt: sent.Resilience.TakenAt}
		env := p.envelope(proto.KindBundleOffer, proto.Endpoint{}, d)
		env.From.Agent = ""
		env.ID = d.ID
		p.send(env)
		return d
	}
	// Only an answer to an outstanding recall of that agent is admitted.
	stray := sendReturn(proto.NewEnvelopeID(), agentID, sent.Inline)
	require.Equal(t, proto.AckRefused, fedAckFor(t, p, stray.ID).Status)
	wrongAgent := sendReturn(recall.ID, "agt_other00000000000000000000", sent.Inline)
	require.Equal(t, proto.AckRefused, fedAckFor(t, p, wrongAgent.ID).Status)
	good := sendReturn(recall.ID, agentID, sent.Inline)
	require.Equal(t, proto.AckAccepted, fedAckFor(t, p, good.ID).Status)
	// Single use: a second return for the same recall is refused.
	second := sendReturn(recall.ID, agentID, append(append([]byte{}, sent.Inline...), ' '))
	require.Equal(t, proto.AckRefused, fedAckFor(t, p, second.ID).Status)

	type recallsView struct {
		Recalls []struct {
			ID    string `json:"id"`
			State string `json:"state"`
		} `json:"recalls"`
	}
	fedEventually(t, "returned snapshot ready", func() bool {
		rec := fedHuman(t, f, http.MethodGet, "/v1/resilience/recalls", nil)
		var v recallsView
		return json.Unmarshal(rec.Body.Bytes(), &v) == nil && len(v.Recalls) == 1 && v.Recalls[0].State == "ready"
	})

	// The agent still runs here: keeping its id is a question, and a
	// running agent is never taken over.
	path := "/v1/resilience/recalls/" + recall.ID + "/"
	body := map[string]any{"group": "recovered", "cwd": testutil.CanonicalTempDir(t)}
	rec = fedHuman(t, f, http.MethodPost, path+"preview", body)
	require.Equal(t, 409, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "is running on this node")
	body["new_id"] = true
	rec = fedHuman(t, f, http.MethodPost, path+"preview", body)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var preview struct {
		Token string `json:"preview_token"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &preview))
	body["preview_token"] = preview.Token
	rec = fedHuman(t, f, http.MethodPost, path+"import", body)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var landed struct {
		Spawn struct {
			ConvID string `json:"conv_id"`
		} `json:"spawn"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &landed))
	got, err := db.AgentIDForConv(landed.Spawn.ConvID)
	require.NoError(t, err)
	require.NotEqual(t, agentID, got)

	// A recall lands one agent.
	rec = fedHuman(t, f, http.MethodPost, path+"preview", body)
	require.Equal(t, 409, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "recovered")
}

func containsJSON(raw, frag string) bool {
	return json.Valid([]byte(raw)) && strings.Contains(raw, frag)
}
