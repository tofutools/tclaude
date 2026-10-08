package agentd_test

import (
	"bufio"
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
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/bundletransfer"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/federation/stream"
)

func fedRequesterCatalog(t *testing.T, fh *fedHarness, support int) {
	t.Helper()
	fh.peer.send(fh.peer.envelope(proto.KindCatalog, proto.Endpoint{}, proto.CatalogPayload{RequesterPays: support, Groups: []proto.CatalogGroup{{Name: "builders", Caps: []string{proto.CapSpawn}}}}))
	fedEventually(t, "requester-pays catalog", func() bool {
		raw, _, err := db.GetFederationCatalog(fh.peer.id.ID())
		var c proto.CatalogPayload
		return err == nil && json.Unmarshal([]byte(raw), &c) == nil && len(c.Groups) > 0 && c.RequesterPays == support
	})
}
func fedRequesterControl(t *testing.T, fh *fedHarness, p proto.ModelLeasePayload) bool {
	t.Helper()
	env := fh.peer.envelope(proto.KindModelLease, proto.Endpoint{}, p)
	env.From.Agent = ""
	env.InReplyTo = proto.NewEnvelopeID()
	fh.peer.send(env)
	var ok bool
	fedEventually(t, "lease answer", func() bool {
		for _, a := range fh.peer.envelopes(proto.KindModelLeaseAnswer) {
			if a.InReplyTo == env.InReplyTo {
				var answer proto.ModelLeaseAnswerPayload
				if a.DecodePayload(&answer) == nil {
					ok = answer.OK
					return true
				}
			}
		}
		return false
	})
	return ok
}
func TestFederation_RequesterPaysLeaseAdmissionAndStream(t *testing.T) {
	fh := newFedHarness(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "provider-secret-test", r.Header.Get("X-Api-Key"))
		if r.URL.Path == "/v1/messages/count_tokens" {
			_, _ = io.WriteString(w, `{"input_tokens":10}`)
			return
		}
		_, _ = io.WriteString(w, `{"type":"message","usage":{"input_tokens":10,"output_tokens":3}}`)
	}))
	defer upstream.Close()
	fedModelPolicy(t, fh, upstream.URL)
	rec := fedHuman(t, fh.f, http.MethodDelete, "/v1/federation/grants", map[string]any{"peer": "bob", "slug": agentd.PermModelsProxy, "scope": "http_proxy=model"})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	rec = fedHuman(t, fh.f, http.MethodPost, "/v1/federation/grants", map[string]any{"peer": "bob", "slug": agentd.PermModelsProxyLeased, "scope": "http_proxy=model"})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	fedRequesterCatalog(t, fh, 0)
	rec = fedHuman(t, fh.f, http.MethodPost, "/v1/federation/spawn-requests", map[string]any{"peer": "bob", "group": "builders", "brief": "work", "credentials": "proxy:model@self"})
	require.NotEqual(t, 200, rec.Code)
	require.Contains(t, rec.Body.String(), "does not support requester-pays")
	fedRequesterCatalog(t, fh, 1)
	rec = fedHuman(t, fh.f, http.MethodPost, "/v1/federation/spawn-requests", map[string]any{"peer": "bob", "group": "builders", "brief": "work", "credentials": "proxy:model@self"})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	rec = fedHuman(t, fh.f, http.MethodGet, "/v1/models/leases", nil)
	require.Equal(t, 200, rec.Code)
	var leases []db.ModelProxyLease
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &leases))
	require.Len(t, leases, 1)
	l := leases[0]
	control := proto.ModelLeasePayload{Lease: l.ID, Request: l.Request, Kind: "spawn", Proxy: "model", Worker: "agt_bobremote0000000000000000", Session: "remote-launch", Generation: "generation1"}
	wrong := control
	wrong.Request = proto.NewEnvelopeID()
	require.False(t, fedRequesterControl(t, fh, wrong))
	require.True(t, fedRequesterControl(t, fh, control))
	require.True(t, fedRequesterControl(t, fh, control))
	wrong = control
	wrong.Generation = "generation2"
	require.False(t, fedRequesterControl(t, fh, wrong))
	// A lease-only grant never admits an ordinary stream or another generation.
	for _, opening := range []proto.ModelOpenPayload{{Proxy: "model", Session: control.Session}, {Proxy: "model", Session: control.Session, Lease: l.ID, Generation: "generation2"}} {
		kp, err := stream.NewKeyPair()
		require.NoError(t, err)
		opening.Version, opening.Stream, opening.Key = 1, proto.NewEnvelopeID(), kp.Pub
		env := fh.peer.envelope(proto.KindModelOpen, proto.Endpoint{}, opening)
		env.From.Agent = ""
		fh.peer.send(env)
		fedEventually(t, "refused open", func() bool {
			for _, a := range fh.peer.envelopes(proto.KindModelAnswer) {
				var answer proto.ModelAnswerPayload
				if a.DecodePayload(&answer) == nil && answer.Stream == opening.Stream {
					return !answer.OK
				}
			}
			return false
		})
	}
	flow := fedModelFlow(t, fh, proto.ModelOpenPayload{Proxy: "model", Session: control.Session, Lease: l.ID, Generation: control.Generation})
	req, err := http.NewRequest(http.MethodPost, "http://model/v1/messages", strings.NewReader(`{"model":"test-model","max_tokens":5,"messages":[{"role":"user","content":"hello"}]}`))
	require.NoError(t, err)
	require.NoError(t, req.Write(flow))
	require.NoError(t, flow.CloseWrite())
	resp, err := http.ReadResponse(bufio.NewReader(flow), req)
	require.NoError(t, err)
	result, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	_ = resp.Body.Close()
	_ = flow.Close()
	require.Equal(t, 200, resp.StatusCode, string(result))
	rec = fedHuman(t, fh.f, http.MethodPost, "/v1/models/leases", map[string]string{"id": l.ID})
	require.Equal(t, 200, rec.Code)
	require.False(t, fedRequesterControl(t, fh, control))
}

func TestFederation_RequesterPaysRequiredRefusesMissingOffer(t *testing.T) {
	fh := newFedHarness(t)
	fh.f.HaveGroup("team")
	rec := fedHuman(t, fh.f, http.MethodPost, "/v1/federation/grants", map[string]any{"peer": "bob", "slug": agentd.PermGroupsMembersSpawn, "scope": "group=team", "spawn_policy": map[string]any{"requester_pays": "required"}})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	env := fh.peer.envelope(proto.KindSpawnReq, proto.Endpoint{}, proto.SpawnRequestPayload{Group: "team", Brief: "work"})
	fh.peer.send(env)
	fedEventually(t, "missing payer refusal", func() bool {
		for _, a := range fh.peer.envelopes(proto.KindAck) {
			var ack proto.AckPayload
			if a.InReplyTo == env.ID && a.DecodePayload(&ack) == nil {
				return ack.Status == proto.AckRefused && strings.Contains(ack.Reason, "requires requester-pays")
			}
		}
		return false
	})
	rows, err := db.ListFederationSpawnRequests(100)
	require.NoError(t, err)
	require.Empty(t, rows)
}

func TestFederation_RequesterPaysWorkerRouteIsFrozenBeforeBirth(t *testing.T) {
	for _, policy := range []string{"off", "allowed", "required"} {
		t.Run(policy, func(t *testing.T) {
			fh := newFedHarness(t)
			fh.f.HaveGroup("team")
			profile := fedNodeProfile(t, fh, "payer-workers", db.FederationNodeProfileSpec{RequesterPays: policy, PeerGrants: []db.FederationPeerGrant{{Slug: agentd.PermGroupsRosterRead, Scope: "group=team"}}, WorkerPermissions: map[string]db.PermissionOverride{agentd.PermModelsProxy: {Effect: "grant", Scope: `{"peer":["` + fh.peer.id.ID() + `"],"http_proxy":["model"]}`}}})
			fedApplyNodeProfile(t, fh, profile)
			lease := proto.NewEnvelopeID()
			env := fh.peer.envelope(proto.KindSpawnReq, proto.Endpoint{}, proto.SpawnRequestPayload{Group: "team", Name: "worker", Brief: "work", Credentials: "proxy:model@" + fh.peer.id.ID(), ModelLease: lease})
			fh.peer.send(env)
			ack := fedAckFor(t, fh.peer, env.ID)
			if policy == "off" {
				require.Equal(t, proto.AckRefused, ack.Status)
				require.Contains(t, ack.Reason, "policy is off")
				return
			}
			require.Equal(t, proto.AckAccepted, ack.Status)
			var row *db.FederationSpawnRequest
			fedEventually(t, "pending requester-paid worker", func() bool {
				rows, err := db.ListFederationSpawnRequests(100)
				if err != nil {
					return false
				}
				for _, r := range rows {
					if r.EnvelopeID == env.ID {
						row = r
						return true
					}
				}
				return false
			})
			previous := agentd.Spawn
			births := 0
			agentd.Spawn = &fedProfileBirthSpawner{inner: previous, check: func(a clcommon.SpawnArgs) {
				births++
				require.Equal(t, "model@"+fh.peer.id.ID(), a.ModelProxy)
				worker, err := db.AgentIDForConv(a.SessionID)
				require.NoError(t, err)
				binding, err := db.GetModelProxyWorkerLease(worker)
				require.NoError(t, err)
				require.NotNil(t, binding)
				require.Equal(t, lease, binding.Lease)
				require.Equal(t, env.ID, binding.Request)
			}}
			t.Cleanup(func() { agentd.Spawn = previous })
			rec := fedHuman(t, fh.f, http.MethodPost, fmt.Sprintf("/v1/federation/spawn-requests/%d/approve", row.ID), map[string]any{})
			require.Equal(t, 200, rec.Code, rec.Body.String())
			require.Equal(t, 1, births)
		})
	}
}

func TestFederation_RequesterPaysTeleportIssuesOfferBoundLease(t *testing.T) {
	fh := newFedHarness(t)
	fedTeleportSource(t, fh)
	fedModelPolicy(t, fh, "https://provider.invalid")
	require.NoError(t, db.PutFederationCatalog(fh.peer.id.ID(), string(mustJSON(t, proto.CatalogPayload{RequesterPays: 1, AgentTeleports: 1, AgentMoves: true, Groups: []proto.CatalogGroup{}})), time.Now()))
	require.NoError(t, db.GrantAgentPermissionWithScope(moveSourceConv, agentd.PermSelfTeleport, `{"peer":["`+fh.peer.id.ID()+`"]}`, "test"))
	require.NoError(t, db.GrantAgentPermissionWithScope(moveSourceConv, agentd.PermModelsProxy, `{"peer":["`+fh.peer.id.ID()+`"],"http_proxy":["model"]}`, "test"))
	rec := fedSelfTeleport(t, fh, map[string]any{"peer": "bob", "group": "receiver", "credentials": "proxy:model@self"})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var result struct {
		Offer struct {
			D bundletransfer.Descriptor `json:"offer"`
		} `json:"offer"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &result))
	d := result.Offer.D
	require.NotNil(t, d.Teleport)
	require.NotEmpty(t, d.Teleport.ModelLease)
	require.Equal(t, "proxy:model@"+fh.peer.agentdID, d.Teleport.Credentials)
	l, err := db.GetModelProxyLease(d.Teleport.ModelLease)
	require.NoError(t, err)
	require.NotNil(t, l)
	require.Equal(t, d.ID, l.Request)
	require.Equal(t, "teleport", l.Kind)
}
