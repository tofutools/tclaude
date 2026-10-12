package agentd_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/bundletransfer"
	"github.com/tofutools/tclaude/pkg/federation/client"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/paf/seal"
	"github.com/tofutools/tclaude/pkg/testharness"
	"github.com/tofutools/tclaude/pkg/testutil"
)

const resilienceSource = "agt_bobsource00000000000000000"

func fedSendSnapshot(t *testing.T, p *fedPeer, raw []byte, takenAt time.Time, keep int) bundletransfer.Descriptor {
	t.Helper()
	return fedSendSnapshotWith(t, p, raw, takenAt, keep, nil)
}

// fedSendSnapshotWith runs before, when set, between building the offer and
// sending it.
func fedSendSnapshotWith(t *testing.T, p *fedPeer, raw []byte, takenAt time.Time, keep int, before func(bundletransfer.Descriptor)) bundletransfer.Descriptor {
	t.Helper()
	d := bundletransfer.New(bundletransfer.Snapshot, raw, "Resilience snapshot", time.Now().Add(time.Hour))
	d.Resilience = &bundletransfer.ResilienceIntent{SourceAgent: resilienceSource, AgentName: "worker", Harness: "claude", TakenAt: takenAt, Keep: keep, MaxAgeDays: 7}
	if before != nil {
		before(d)
	}
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

	// Unrestricted trust does not imply custody: it takes the explicit grant.
	peer, err := db.GetFederationPeer(p.id.ID())
	require.NoError(t, err)
	peer.TrustLevel = db.FederationTrustUnrestricted
	require.NoError(t, db.TrustFederationPeer(*peer))
	require.True(t, db.FederationPeerUnrestricted(p.id.ID()))
	denied = fedSendSnapshot(t, p, fedSealed(t, "one"), now.Add(-3*time.Hour), 2)
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
	rec = testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodDelete, "/v1/resilience/held/bob", nil), "agent-caller"))
	require.Equal(t, 403, rec.Code)
	rec = fedHuman(t, f, http.MethodDelete, "/v1/resilience/held/bob", nil)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"removed":2`)
	require.Empty(t, fedHeld(t, f).Held)
}

// A replacement that cannot be written leaves the last acknowledged backup
// in place: with keep 1 the old copy goes only once the new one is kept.
func TestResilience_FailedReplacementKeepsPreviousSnapshot(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	now := time.Now().UTC().Truncate(time.Second)
	rec := fedHuman(t, f, http.MethodPost, "/v1/federation/grants", map[string]any{"peer": "bob", "slug": "snapshots.receive"})
	require.Equal(t, 200, rec.Code, rec.Body.String())

	first := fedSendSnapshot(t, p, fedSealed(t, "one"), now.Add(-time.Hour), 1)
	require.Equal(t, proto.AckAccepted, fedAckFor(t, p, first.ID).Status)
	fedEventually(t, "first snapshot held", func() bool { return len(fedHeld(t, f).Held) == 1 })

	// The replacement's ciphertext path is taken by a directory, so its
	// final rename fails.
	second := fedSendSnapshotWith(t, p, fedSealed(t, "two"), now, 1, func(d bundletransfer.Descriptor) {
		blocker := filepath.Join(config.DataDir(), "resilience", "held", p.id.ID(), resilienceSource, d.ID+".paf.age", "x")
		require.NoError(t, os.MkdirAll(blocker, 0o700))
	})
	require.Equal(t, proto.AckAccepted, fedAckFor(t, p, second.ID).Status)
	fedEventually(t, "replacement received", func() bool {
		o, _ := db.GetFederationBundleOffer("in", p.id.ID(), second.ID)
		return o != nil && o.State == "ready"
	})
	require.Never(t, func() bool {
		h := fedHeld(t, f).Held
		return len(h) != 1 || h[0].Offer != first.ID
	}, time.Second, 50*time.Millisecond, "the previous snapshot stays held")
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

	// Unchanged, it is still sent again before the peer's retention (30
	// days by default) could remove the only copy.
	backdateResilienceLastGood(t, 20*24*time.Hour)
	v = resilienceRun(t, f, policy.ID)
	require.NotEmpty(t, v.Policies[0].Agents[0].Pending, "a copy halfway to expiry is refreshed")
	fedEventually(t, "refresh offer sent", func() bool { return len(p.envelopes(proto.KindBundleOffer)) == 2 })

	rec = fedHuman(t, f, http.MethodDelete, "/v1/resilience/policies/"+policy.ID, nil)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	rec = fedHuman(t, f, http.MethodGet, "/v1/resilience/policies", nil)
	require.Equal(t, 200, rec.Code)
	require.Contains(t, rec.Body.String(), `"policies":[]`)
}

// fedBoardWithMember creates a board on the harness hub and joins another
// instance to it as a publisher.
func fedBoardWithMember(t *testing.T, fh *fedHarness) (string, *proto.Identity) {
	t.Helper()
	rec := fedHuman(t, fh.f, http.MethodPost, "/v1/federation/boards", map[string]any{"name": "resilience-board"})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var created struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	rec = fedHuman(t, fh.f, http.MethodPost, "/v1/federation/boards/"+created.ID+"/invites", map[string]any{"role": "publisher", "ttl_seconds": 120})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var invitation struct {
		Token string `json:"token"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &invitation))
	var invite struct{ Secret string }
	encoded, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(invitation.Token, "board1_"))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(encoded, &invite))
	member, err := proto.NewIdentity()
	require.NoError(t, err)
	joined, err := client.BoardCall(context.Background(), client.Options{URL: fh.url, Identity: member}, invite.Secret, "keys.join", map[string]any{"board": created.ID, "token": invite.Secret})
	require.NoError(t, err)
	require.Equal(t, 200, joined.Status, string(joined.Body))
	return created.ID, member
}

func TestResilience_PolicyPutsSnapshotsOnBoard(t *testing.T) {
	fh := newFedHarness(t)
	f := fh.f
	const conv = "019fe740-43a4-7023-b8ae-1ee64459f2c9"
	f.HaveAliveSession(conv, "resilient", "resilient-pane", testutil.CanonicalTempDir(t))
	f.HaveEnrolledAgent(conv)
	f.SetSessionStatus(conv, "idle")
	board, member := fedBoardWithMember(t, fh)
	to := "board:" + board

	rec := fedHuman(t, f, http.MethodPost, "/v1/resilience/policies", map[string]any{"target": conv, "every": "1h", "to": to, "keep": 1})
	require.Equal(t, 400, rec.Code, "a board destination names its recipients")
	rec = fedHuman(t, f, http.MethodPost, "/v1/resilience/policies", map[string]any{"target": conv, "every": "1h", "to": to, "recipients": []string{member.ID()}, "keep": 1, "max_age_days": 31})
	require.Equal(t, 400, rec.Code, "a board snapshot expires within 30 days")
	rec = fedHuman(t, f, http.MethodPost, "/v1/resilience/policies", map[string]any{"target": conv, "every": "1h", "to": to, "recipients": []string{"nobody"}, "keep": 1})
	require.Equal(t, 400, rec.Code, "a recipient is an instance id or a trusted peer's label")
	rec = fedHuman(t, f, http.MethodPost, "/v1/resilience/policies", map[string]any{"target": conv, "every": "1h", "to": to, "recipients": []string{member.ID()}, "keep": 1, "max_age_days": 3})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var policy struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &policy))

	type boardList struct {
		Snapshots []struct {
			Item      string    `json:"item"`
			Version   string    `json:"version"`
			Name      string    `json:"name"`
			ExpiresAt time.Time `json:"expires_at"`
		} `json:"snapshots"`
	}
	list := func() boardList {
		t.Helper()
		rec := fedHuman(t, f, http.MethodGet, "/v1/federation/boards/"+board+"/snapshots", nil)
		require.Equal(t, 200, rec.Code, rec.Body.String())
		var l boardList
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &l))
		return l
	}

	v := resilienceRun(t, f, policy.ID)
	require.Empty(t, v.Policies[0].Agents[0].LastError)
	require.NotEmpty(t, v.Policies[0].Agents[0].LastDigest)
	first := list()
	require.Len(t, first.Snapshots, 1)
	require.True(t, strings.HasPrefix(first.Snapshots[0].Name, "resilience-"), first.Snapshots[0].Name)
	require.WithinDuration(t, time.Now().Add(72*time.Hour), first.Snapshots[0].ExpiresAt, time.Minute)

	// Unchanged: nothing new on the board.
	resilienceRun(t, f, policy.ID)
	require.Len(t, list().Snapshots, 1)

	// Changed: a new snapshot replaces the old one (keep 1).
	require.NoError(t, f.World.CCs.GetByConvID(conv).WriteUserTurn("one more turn"))
	v = resilienceRun(t, f, policy.ID)
	require.Empty(t, v.Policies[0].Agents[0].LastError)
	second := list()
	require.Len(t, second.Snapshots, 1)
	require.NotEqual(t, first.Snapshots[0].Item, second.Snapshots[0].Item)

	// A board never takes suspected credentials.
	require.NoError(t, f.World.CCs.GetByConvID(conv).WriteUserTurn("token ghp_"+strings.Repeat("a1B2", 9)))
	v = resilienceRun(t, f, policy.ID)
	require.Contains(t, v.Policies[0].Agents[0].LastError, "suspected credential")
	require.Len(t, list().Snapshots, 1)
}

// backdateResilienceLastGood moves every policy state's last good snapshot
// back by d, as if that much time had passed.
func backdateResilienceLastGood(t *testing.T, d time.Duration) {
	t.Helper()
	path := filepath.Join(config.DataDir(), "snapshots", "resilience.json")
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var file map[string]any
	require.NoError(t, json.Unmarshal(raw, &file))
	for _, agents := range file["state"].(map[string]any) {
		for _, st := range agents.(map[string]any) {
			st := st.(map[string]any)
			at, err := time.Parse(time.RFC3339Nano, st["last_good"].(string))
			require.NoError(t, err)
			st["last_good"] = at.Add(-d).Format(time.RFC3339Nano)
		}
	}
	raw, err = json.Marshal(file)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, raw, 0o600))
}
