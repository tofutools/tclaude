package agentd_test

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/tofutools/tclaude/pkg/federation/stream"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/agentbundle"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	tclcommon "github.com/tofutools/tclaude/pkg/common"
	"github.com/tofutools/tclaude/pkg/federation/bundletransfer"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/testharness"
	"github.com/tofutools/tclaude/pkg/testutil"
)

func fedAgentBundle(t *testing.T) *agentbundle.Bundle {
	t.Helper()
	return &agentbundle.Bundle{Manifest: agentbundle.Manifest{Format: agentbundle.Format, FormatVersion: 1, CreatedAt: time.Now().UTC().Format(time.RFC3339), TclaudeVersion: "test", Agent: agentbundle.Definition{Name: "shared-worker", Harness: "claude", Profile: json.RawMessage(`{"is_owner":true,"permission_overrides":{"config.import":{"effect":"grant"}},"role_refs":["remote-only-role"]}`), Permissions: []agentbundle.Permission{{Slug: "config.import", Effect: "grant", Source: "source-agent"}}, Paths: agentbundle.Paths{Cwd: "/missing/source/cwd"}, StartupContext: "Review the changes."}}}
}
func fedAgentOffer(t *testing.T, p *fedPeer, group string, b *agentbundle.Bundle) bundletransfer.Descriptor {
	t.Helper()
	raw, err := b.Encode()
	require.NoError(t, err)
	d := bundletransfer.New(bundletransfer.Agent, raw, "Shared agent", time.Now().Add(time.Hour))
	d.Group = group
	env := p.envelope(proto.KindBundleOffer, proto.Endpoint{}, d)
	env.ID = d.ID
	p.send(env)
	return d
}
func fedReceiveAgents(t *testing.T, fh *fedHarness, group string) {
	t.Helper()
	rec := fedHuman(t, fh.f, http.MethodPost, "/v1/federation/grants", map[string]any{"peer": "bob", "slug": "agents.receive", "scope": "group=" + group})
	require.Equal(t, 200, rec.Code, rec.Body.String())
}
func TestFederation_AgentOfferAdmissionAndLocalImport(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	f.HaveGroup("receiver")
	f.HaveGroup("private")
	b := fedAgentBundle(t)
	denied := fedAgentOffer(t, p, "receiver", b)
	require.Equal(t, proto.AckRefused, fedAckFor(t, p, denied.ID).Status)
	rec := fedHuman(t, f, http.MethodPost, "/v1/federation/grants", map[string]any{"peer": "bob", "slug": "agents.receive"})
	require.Equal(t, 400, rec.Code)
	fedReceiveAgents(t, fh, "receiver")
	hidden := fedAgentOffer(t, p, "private", b)
	require.Equal(t, proto.AckRefused, fedAckFor(t, p, hidden.ID).Status)
	d := fedAgentOffer(t, p, "receiver", b)
	require.Equal(t, proto.AckAccepted, fedAckFor(t, p, d.ID).Status)
	group, err := db.GetAgentGroupByName("receiver")
	require.NoError(t, err)
	stored, err := db.GetFederationBundleOffer("in", p.id.ID(), d.ID)
	require.NoError(t, err)
	require.Equal(t, group.ID, stored.GroupID)
	require.NotEmpty(t, stored.SenderAgent)
	path := "/v1/federation/bundle-offers/" + d.ID + "/import"
	rec = fedHuman(t, f, http.MethodPost, path, nil)
	require.Equal(t, 409, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"code":"landing_unresolved"`)
	require.Contains(t, rec.Body.String(), `"candidates"`)
	members, err := db.ListAgentGroupMembers(group.ID)
	require.NoError(t, err)
	require.Empty(t, members)
	cwd := testutil.CanonicalTempDir(t)
	rec = fedHuman(t, f, http.MethodPost, path, map[string]any{"apply": true, "cwd": cwd, "group": "private"})
	require.Equal(t, 403, rec.Code)
	rec = fedHuman(t, f, http.MethodPost, path, map[string]any{"apply": true})
	require.Equal(t, 409, rec.Code)
	stored, err = db.GetFederationBundleOffer("in", p.id.ID(), d.ID)
	require.NoError(t, err)
	require.Empty(t, stored.ImportAgent, "invalid paths must not consume the launch")
	rec = fedHuman(t, f, http.MethodPost, path, map[string]any{"apply": true, "cwd": filepath.Join(cwd, "missing")})
	require.Equal(t, 409, rec.Code, rec.Body.String())
	stored, err = db.GetFederationBundleOffer("in", p.id.ID(), d.ID)
	require.NoError(t, err)
	require.Empty(t, stored.ImportAgent, "validation failure before dispatch must be retryable")
	// Renaming keeps the exact admitted group, rather than re-resolving the wire name.
	_, err = db.RenameAgentGroup("receiver", "renamed-receiver", "")
	require.NoError(t, err)
	rec = fedHuman(t, f, http.MethodPost, path, map[string]any{"apply": true, "cwd": cwd, "name": "received-worker"})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var imported struct {
		Applied bool `json:"applied"`
		Spawn   struct {
			ConvID  string `json:"conv_id"`
			AgentID string `json:"agent_id"`
		} `json:"spawn"`
	}
	testharness.DecodeJSON(t, rec, &imported)
	require.True(t, imported.Applied)
	require.NotEmpty(t, imported.Spawn.ConvID)
	member, err := db.FindMemberInGroup(group.ID, imported.Spawn.ConvID)
	require.NoError(t, err)
	require.NotNil(t, member)
	owner, err := db.IsAgentGroupOwner(group.ID, imported.Spawn.ConvID)
	require.NoError(t, err)
	require.False(t, owner)
	grants, err := db.ListAgentPermissionOverrideRowsForConv(imported.Spawn.ConvID)
	require.NoError(t, err)
	require.Empty(t, grants)
	stored, err = db.GetFederationBundleOffer("in", p.id.ID(), d.ID)
	require.NoError(t, err)
	require.Equal(t, "applied", stored.State)
	require.Equal(t, imported.Spawn.AgentID, stored.ImportAgent)
	rec = fedHuman(t, f, http.MethodPost, path, map[string]any{"apply": true, "cwd": cwd})
	require.Equal(t, 409, rec.Code)
}
func TestFederation_AgentOfferGroupIdentityAndRevocation(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	f.HaveGroup("receiver")
	fedReceiveAgents(t, fh, "receiver")
	d := fedAgentOffer(t, p, "receiver", fedAgentBundle(t))
	require.Equal(t, proto.AckAccepted, fedAckFor(t, p, d.ID).Status)
	require.NoError(t, db.DeleteAgentGroup("receiver"))
	f.HaveGroup("receiver")
	fedReceiveAgents(t, fh, "receiver")
	path := "/v1/federation/bundle-offers/" + d.ID + "/import"
	rec := fedHuman(t, f, http.MethodPost, path, map[string]any{"cwd": testutil.CanonicalTempDir(t), "apply": true})
	require.Equal(t, 403, rec.Code, "recreating a group must not retarget a pending offer")
	rec = fedHuman(t, f, http.MethodPost, "/v1/federation/bundle-offers/"+d.ID+"/decline", nil)
	require.Equal(t, 200, rec.Code)
	setFedTrustLevel(t, fh, "unrestricted")
	d = fedAgentOffer(t, p, "receiver", fedAgentBundle(t))
	require.Equal(t, proto.AckAccepted, fedAckFor(t, p, d.ID).Status)
	group, err := db.GetAgentGroupByName("receiver")
	require.NoError(t, err)
	members, err := db.ListAgentGroupMembers(group.ID)
	require.NoError(t, err)
	require.Empty(t, members, "unrestricted receipt must not launch an agent")
}
func TestFederation_AgentSharingPeerScopeAndHistoryDefault(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	f.HaveGroup("source")
	const source = "019fe740-43a4-7023-b8ae-1ee64459f2a1"
	cwd := testutil.CanonicalTempDir(t)
	f.HaveAliveSession(source, "share-source", "share-source-pane", cwd)
	f.HaveMember("source", source)
	share := func(history bool, sourceRef string) int {
		rec := testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/federation/share-agent", map[string]any{"peer": "bob", "group": "destination", "agent": sourceRef, "history": history}), source))
		return rec.Code
	}
	require.Equal(t, 403, share(false, "self"))
	require.NoError(t, db.GrantAgentPermission(source, "agent.share", "human"))
	require.Equal(t, 403, share(false, "self"), "unscoped grants confer no remote authority")
	require.NoError(t, db.GrantAgentPermissionWithScope(source, "agent.share", fmt.Sprintf(`{"peer":["%s/other"]}`, p.id.ID()), "human"))
	require.Equal(t, 403, share(false, "self"))
	require.NoError(t, db.GrantAgentPermissionWithScope(source, "agent.share", fmt.Sprintf(`{"peer":["%s/destination"]}`, p.id.ID()), "human"))
	require.Equal(t, 200, share(false, "self"))
	fedEventually(t, "config-only agent offer", func() bool { return len(p.envelopes(proto.KindBundleOffer)) >= 1 })
	var d bundletransfer.Descriptor
	require.NoError(t, p.envelopes(proto.KindBundleOffer)[0].DecodePayload(&d))
	b, err := agentbundle.Decode(d.Inline)
	require.NoError(t, err)
	require.Nil(t, b.Manifest.History)
	h, _ := harness.Get("claude")
	before, err := h.History.Export(source, cwd)
	require.NoError(t, err)
	require.Equal(t, 200, share(true, "self"))
	fedEventually(t, "agent history offer", func() bool { return len(p.envelopes(proto.KindBundleOffer)) >= 2 })
	require.NoError(t, p.envelopes(proto.KindBundleOffer)[1].DecodePayload(&d))
	b, err = agentbundle.Decode(d.Inline)
	require.NoError(t, err)
	require.NotNil(t, b.Manifest.History)
	require.Equal(t, before, b.Transcript)
	after, err := h.History.Export(source, cwd)
	require.NoError(t, err)
	require.Equal(t, before, after)
	f.HaveAliveSession("other-source-agent", "other-share", "other-pane", cwd)
	f.HaveConvWithTitle("other-source-agent", "other-source-agent")
	f.HaveMember("source", "other-source-agent")
	require.Equal(t, 403, share(false, "other-source-agent"), "peer-scoped sharing of self cannot export another agent")
	aid, err := db.AgentIDForConv(source)
	require.NoError(t, err)
	require.NoError(t, db.SetAgentInitialSpawnConfig(aid, `{"initial_message":"Use api_key=keepverbatim123456789"}`))
	rec := testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/federation/share-agent", map[string]any{"peer": "bob", "group": "destination", "agent": "self"}), source))
	require.Equal(t, 422, rec.Code, rec.Body.String())
	require.NotContains(t, rec.Body.String(), "keepverbatim")
	require.Contains(t, rec.Body.String(), "credential-assignment")
	rec = testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/federation/share-agent", map[string]any{"peer": "bob", "group": "destination", "agent": "self", "allow_flagged": true}), source))
	require.Equal(t, 200, rec.Code, rec.Body.String())
}
func TestFederation_AgentOfferHistoryResumesFreshIdentity(t *testing.T) {
	for _, name := range []string{"claude", "codex"} {
		t.Run(name, func(t *testing.T) {
			fh := newFedHarness(t)
			f, p := fh.f, fh.peer
			t.Setenv("CODEX_HOME", "")
			f.HaveGroup("receiver")
			f.HaveGroup("source")
			fedReceiveAgents(t, fh, "receiver")
			source := "019fe740-43a4-7023-b8ae-1ee64459f2a1"
			cwd := testutil.CanonicalTempDir(t)
			if name == "codex" {
				f.HaveAliveCodexSession(source, "history-source", "history-source-pane", cwd)
			} else {
				f.HaveAliveSession(source, "history-source", "history-source-pane", cwd)
			}
			f.HaveMember("source", source)
			rec := profileReq(t, f, http.MethodGet, "/v1/agent-bundle/export?agent="+source+"&history=true", nil)
			require.Equal(t, 200, rec.Code, rec.Body.String())
			b, err := agentbundle.Decode(rec.Body.Bytes())
			require.NoError(t, err)
			original := append([]byte{}, b.Transcript...)
			d := fedAgentOffer(t, p, "receiver", b)
			require.Equal(t, proto.AckAccepted, fedAckFor(t, p, d.ID).Status)
			dest := testutil.CanonicalTempDir(t)
			rec = fedHuman(t, f, http.MethodPost, "/v1/federation/bundle-offers/"+d.ID+"/import", map[string]any{"cwd": dest, "apply": true, "name": "received-history"})
			require.Equal(t, 200, rec.Code, rec.Body.String())
			var result struct {
				History bool `json:"history"`
				Spawn   struct {
					ConvID string `json:"conv_id"`
				} `json:"spawn"`
			}
			testharness.DecodeJSON(t, rec, &result)
			require.True(t, result.History)
			require.NotEqual(t, source, result.Spawn.ConvID)
			h, _ := harness.Get(name)
			after, err := h.History.Export(source, cwd)
			require.NoError(t, err)
			require.Equal(t, original, after)
			imported, err := h.History.Export(result.Spawn.ConvID, dest)
			require.NoError(t, err)
			require.Contains(t, string(imported), dest)
		})
	}
}

func TestFederation_AgentOfferLargeHistoryTransfer(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	f.HaveGroup("receiver")
	fedReceiveAgents(t, fh, "receiver")
	b := fedAgentBundle(t)
	entropy := make([]byte, 450<<10)
	_, err := rand.Read(entropy)
	require.NoError(t, err)
	content := base64.StdEncoding.EncodeToString(entropy)
	const source = "019fe740-43a4-7023-b8ae-1ee64459f2a1"
	transcript := append(mustJSON(t, map[string]any{"type": "user", "sessionId": source, "cwd": "/source", "message": map[string]string{"content": content}}), '\n')
	b.SetHistory("claude-jsonl", source, transcript)
	raw, err := b.Encode()
	require.NoError(t, err)
	d := fedAgentOffer(t, p, "receiver", b)
	require.Empty(t, d.Inline)
	require.Equal(t, proto.AckAccepted, fedAckFor(t, p, d.ID).Status)
	path := "/v1/federation/bundle-offers/" + d.ID + "/import"
	cwd := testutil.CanonicalTempDir(t)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- fedHuman(t, f, http.MethodPost, path, map[string]any{"cwd": cwd}) }()
	fedEventually(t, "agent archive fetch", func() bool { return len(p.envelopes(proto.KindBundleFetch)) > 0 })
	var req bundletransfer.Request
	require.NoError(t, p.envelopes(proto.KindBundleFetch)[0].DecodePayload(&req))
	kp, err := stream.NewKeyPair()
	require.NoError(t, err)
	ans := p.envelope(proto.KindBundleAnswer, proto.Endpoint{}, bundletransfer.Answer{Request: bundletransfer.Request{Offer: req.Offer, Stream: req.Stream, SHA256: req.SHA256, Key: kp.Pub}, OK: true})
	ans.From.Agent = ""
	p.send(ans)
	conn := fedPeerStream(t, p, req.Stream, kp, req.Key, false)
	_, err = io.Copy(conn, bytes.NewReader(raw))
	require.NoError(t, err)
	require.NoError(t, conn.CloseWrite())
	select {
	case rec := <-done:
		require.Equal(t, 200, rec.Code, rec.Body.String())
		require.Contains(t, rec.Body.String(), `"history":true`)
		require.Contains(t, rec.Body.String(), `"applied":false`)
	case <-time.After(15 * time.Second):
		t.Fatal("agent archive transfer did not finish")
	}
	_ = conn.Close()
	rec := fedHuman(t, f, http.MethodPost, path, map[string]any{"cwd": cwd, "apply": true, "name": "large-history-worker"})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var result struct {
		Spawn struct {
			ConvID string `json:"conv_id"`
		} `json:"spawn"`
	}
	testharness.DecodeJSON(t, rec, &result)
	h, _ := harness.Get("claude")
	history, err := h.History.Export(result.Spawn.ConvID, cwd)
	require.NoError(t, err)
	require.Contains(t, string(history), content)
	require.NotEqual(t, source, result.Spawn.ConvID)
}
func TestFederation_AgentOfferUncertainLaunchCannotDuplicate(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	f.HaveGroup("receiver")
	fedReceiveAgents(t, fh, "receiver")
	b := fedAgentBundle(t)
	const id = "019fe740-43a4-7023-b8ae-1ee64459f2a1"
	b.SetHistory("claude-jsonl", id, []byte(`{"type":"user","sessionId":"`+id+`","cwd":"/source","message":{"content":"source text"}}`+"\n"))
	d := fedAgentOffer(t, p, "receiver", b)
	require.Equal(t, proto.AckAccepted, fedAckFor(t, p, d.ID).Status)
	spawner := &failingBundleSpawner{}
	previous := agentd.Spawn
	agentd.Spawn = spawner
	t.Cleanup(func() { agentd.Spawn = previous })
	path := "/v1/federation/bundle-offers/" + d.ID + "/import"
	in := map[string]any{"cwd": testutil.CanonicalTempDir(t), "apply": true}
	rec := fedHuman(t, f, http.MethodPost, path, in)
	require.Equal(t, 500, rec.Code, rec.Body.String())
	offer, err := db.GetFederationBundleOffer("in", p.id.ID(), d.ID)
	require.NoError(t, err)
	require.NotEmpty(t, offer.ImportAgent)
	require.NotEmpty(t, offer.ImportLabel)
	rec = fedHuman(t, f, http.MethodPost, path, in)
	require.Equal(t, 409, rec.Code)
	require.Contains(t, rec.Body.String(), "launch_reserved")
	rec = fedHuman(t, f, http.MethodPost, "/v1/federation/bundle-offers/"+d.ID+"/decline", nil)
	require.Equal(t, 200, rec.Code)
}

func TestFederation_AgentOfferPreparationFailureIsRetryable(t *testing.T) {
	for _, history := range []bool{false, true} {
		t.Run(fmt.Sprintf("history=%t", history), func(t *testing.T) {
			fh := newFedHarness(t)
			f, p := fh.f, fh.peer
			t.Setenv("CODEX_HOME", "")
			f.HaveGroup("receiver")
			fedReceiveAgents(t, fh, "receiver")
			b := fedAgentBundle(t)
			b.Manifest.Agent.Harness = "codex"
			b.Manifest.Agent.Profile = json.RawMessage(`{"codex_app_server":true,"sandbox":"danger-full-access"}`)
			if history {
				f.HaveGroup("source")
				const source = "019fe740-43a4-7023-b8ae-1ee64459f2a1"
				cwd := testutil.CanonicalTempDir(t)
				f.HaveAliveCodexSession(source, "source-session", "source-pane", cwd)
				f.HaveMember("source", source)
				rec := profileReq(t, f, http.MethodGet, "/v1/agent-bundle/export?agent="+source+"&history=true", nil)
				require.Equal(t, 200, rec.Code, rec.Body.String())
				exported, err := agentbundle.Decode(rec.Body.Bytes())
				require.NoError(t, err)
				b.SetHistory(exported.Manifest.History.Format, source, exported.Transcript)
			}
			d := fedAgentOffer(t, p, "receiver", b)
			require.Equal(t, proto.AckAccepted, fedAckFor(t, p, d.ID).Status)
			// A regular file prevents app-server directory preparation, before any
			// external spawn call can have dispatched a child.
			blocker := filepath.Join(tclcommon.TclaudeAPIDir(), "codex")
			require.NoError(t, os.MkdirAll(filepath.Dir(blocker), 0700))
			require.NoError(t, os.WriteFile(blocker, []byte("blocked"), 0600))
			spawner := &failingBundleSpawner{}
			previous := agentd.Spawn
			agentd.Spawn = spawner
			t.Cleanup(func() { agentd.Spawn = previous })
			path := "/v1/federation/bundle-offers/" + d.ID + "/import"
			in := map[string]any{"cwd": testutil.CanonicalTempDir(t), "apply": true}
			for attempt := 0; attempt < 2; attempt++ {
				rec := fedHuman(t, f, http.MethodPost, path, in)
				require.Equal(t, 500, rec.Code, rec.Body.String())
				require.Contains(t, rec.Body.String(), "create Codex app-server owner directory")
				offer, err := db.GetFederationBundleOffer("in", p.id.ID(), d.ID)
				require.NoError(t, err)
				require.Empty(t, offer.ImportAgent, "definite pre-dispatch failure permits retry")
				require.Empty(t, offer.ImportLabel)
				require.Empty(t, spawner.importedID, "resume was never dispatched")
			}
		})
	}
}
