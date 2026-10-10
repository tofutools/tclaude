package agentd_test

import (
	"encoding/json"
	"github.com/tofutools/tclaude/pkg/federation/bundletransfer"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/testharness"
	"github.com/tofutools/tclaude/pkg/testutil"
)

type landingFlowReply struct {
	Code    string `json:"code"`
	Landing struct {
		Cwd              string `json:"cwd"`
		Reason           string `json:"reason"`
		Exists           bool   `json:"exists"`
		CheckoutRequired bool   `json:"checkout_required"`
		SourceCwd        string `json:"source_cwd"`
		Candidates       []struct {
			ID     string `json:"id"`
			Cwd    string `json:"cwd"`
			Exists bool   `json:"exists"`
		} `json:"candidates"`
	} `json:"landing"`
}

func TestFederationLandingReceiverChoices(t *testing.T) {
	fh := newFedHarness(t)
	fh.f.HaveGroup("receiver")
	fedReceiveAgents(t, fh, "receiver")
	fallback := testutil.CanonicalTempDir(t)
	source := testutil.CanonicalTempDir(t)
	explicit := testutil.CanonicalTempDir(t)
	_, err := db.SetAgentGroupDefaultCwd("receiver", fallback)
	require.NoError(t, err)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://localhost:12345"))
	dash := agentd.BuildDashboardHandlerForTest()
	for _, tc := range []struct {
		name, source string
		body         map[string]any
		want, reason string
		status       int
	}{
		{"same path", source, nil, source, "same_path", 200},
		{"group default", "/missing/source", nil, fallback, "group_default", 200},
		{"explicit wins", source, map[string]any{"cwd": explicit}, explicit, "explicit", 200},
		{"candidate overrides same path", source, map[string]any{"landing": "group_default"}, fallback, "group_default", 200},
		{"HOME source skips to group default", os.Getenv("HOME"), nil, fallback, "group_default", 200},
		{"explicit HOME allowed", source, map[string]any{"cwd": os.Getenv("HOME")}, os.Getenv("HOME"), "explicit", 200},
		{"strict keep paths", "/missing/source", map[string]any{"keep_paths": true}, "/missing/source", "same_path", 409},
		{"explicit missing does not fallback", source, map[string]any{"cwd": "/missing/explicit"}, "/missing/explicit", "explicit", 409},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := fedAgentBundle(t)
			b.Manifest.Agent.Paths.Cwd = tc.source
			d := fedAgentOffer(t, fh.peer, "receiver", b)
			require.Equal(t, proto.AckAccepted, fedAckFor(t, fh.peer, d.ID).Status)
			path := "/api/federation/bundle-offers/" + d.ID + "/import?peer=" + fh.peer.id.ID()
			rec := testharness.Serve(dash, testharness.JSONRequest(t, http.MethodPost, path, tc.body))
			require.Equal(t, tc.status, rec.Code, rec.Body.String())
			var reply landingFlowReply
			testharness.DecodeJSON(t, rec, &reply)
			require.Equal(t, tc.want, reply.Landing.Cwd)
			require.Equal(t, tc.reason, reply.Landing.Reason)
			require.Equal(t, tc.source, reply.Landing.SourceCwd)
			v1 := fedHuman(t, fh.f, http.MethodPost, "/v1/federation/bundle-offers/"+d.ID+"/import?peer="+fh.peer.id.ID(), tc.body)
			require.Equal(t, rec.Code, v1.Code)
			var cli landingFlowReply
			testharness.DecodeJSON(t, v1, &cli)
			require.Equal(t, reply.Landing, cli.Landing)
			if tc.status != 200 {
				row, err := db.GetFederationBundleOffer("in", fh.peer.id.ID(), d.ID)
				require.NoError(t, err)
				require.Empty(t, row.ImportAgent)
				return
			}
			body := map[string]any{"apply": true}
			for k, v := range tc.body {
				body[k] = v
			}
			rec = testharness.Serve(dash, testharness.JSONRequest(t, http.MethodPost, path, body))
			require.Equal(t, 200, rec.Code, rec.Body.String())
			var result struct {
				Spawn struct {
					ConvID string `json:"conv_id"`
				} `json:"spawn"`
			}
			testharness.DecodeJSON(t, rec, &result)
			sessions, err := db.FindSessionsByConvID(result.Spawn.ConvID)
			require.NoError(t, err)
			require.NotEmpty(t, sessions)
			require.Equal(t, tc.want, sessions[0].Cwd)
		})
	}
	if os.Geteuid() != 0 {
		b := fedAgentBundle(t)
		d := fedAgentOffer(t, fh.peer, "receiver", b)
		require.Equal(t, proto.AckAccepted, fedAckFor(t, fh.peer, d.ID).Status)
		rec := fedHuman(t, fh.f, http.MethodPost, "/v1/federation/bundle-offers/"+d.ID+"/import", map[string]any{"cwd": "/"})
		require.Equal(t, 403, rec.Code, rec.Body.String())
		require.Contains(t, rec.Body.String(), "landing_unowned")
	}
	_, err = db.SetAgentGroupDefaultCwd("receiver", os.Getenv("HOME"))
	require.NoError(t, err)
	homeBundle := fedAgentBundle(t)
	homeOffer := fedAgentOffer(t, fh.peer, "receiver", homeBundle)
	require.Equal(t, proto.AckAccepted, fedAckFor(t, fh.peer, homeOffer.ID).Status)
	homePreview := fedHuman(t, fh.f, http.MethodPost, "/v1/federation/bundle-offers/"+homeOffer.ID+"/import", nil)
	require.Equal(t, 200, homePreview.Code, homePreview.Body.String())
	require.Contains(t, homePreview.Body.String(), `"reason":"group_default"`)
	_, err = db.SetAgentGroupDefaultCwd("receiver", "")
	require.NoError(t, err)
	b := fedAgentBundle(t)
	d := fedAgentOffer(t, fh.peer, "receiver", b)
	require.Equal(t, proto.AckAccepted, fedAckFor(t, fh.peer, d.ID).Status)
	rec := fedHuman(t, fh.f, http.MethodPost, "/v1/federation/bundle-offers/"+d.ID+"/import", nil)
	require.Equal(t, 409, rec.Code)
	require.Contains(t, rec.Body.String(), "landing_unresolved")
	require.Contains(t, rec.Body.String(), "candidates")
}
func TestFederationLandingAllowlistedRepoIsolated(t *testing.T) {
	fh := newFedHarness(t)
	fh.f.HaveGroup("team")
	fedReceiveAgents(t, fh, "team")
	clone := fedJobRepo(t, fh)
	repo, err := db.GetFederationRepo("project")
	require.NoError(t, err)
	// Matching never contacts the source hint. A pinned receiver HEAD can use
	// local objects even when the allowlisted transport is unavailable.
	repo.Definition.URL = "https://127.0.0.1:1/example/project.git"
	require.NoError(t, db.SaveFederationRepo(repo))
	b := fedAgentBundle(t)
	b.Manifest.Agent.Paths.RepoURL = "ssh://git@127.0.0.1:1/example/project.git"
	b.Manifest.Agent.Paths.Cwd = clone
	d := fedAgentOffer(t, fh.peer, "team", b)
	require.Equal(t, proto.AckAccepted, fedAckFor(t, fh.peer, d.ID).Status)
	path := "/v1/federation/bundle-offers/" + d.ID + "/import"
	rec := fedHuman(t, fh.f, http.MethodPost, path, nil)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var preview landingFlowReply
	testharness.DecodeJSON(t, rec, &preview)
	require.Equal(t, "repo_match", preview.Landing.Reason)
	require.True(t, preview.Landing.CheckoutRequired)
	require.False(t, preview.Landing.Exists)
	require.NotEqual(t, clone, preview.Landing.Cwd)
	_, err = os.Stat(preview.Landing.Cwd)
	require.True(t, os.IsNotExist(err), "preview never creates checkout")
	rec = fedHuman(t, fh.f, http.MethodPost, path, map[string]any{"apply": true})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	data, err := os.ReadFile(filepath.Join(preview.Landing.Cwd, "hello"))
	require.NoError(t, err)
	require.Equal(t, "from git\n", string(data))
	// A chosen candidate disappearing cannot silently fall back to same_path.
	next := fedAgentOffer(t, fh.peer, "team", b)
	require.Equal(t, proto.AckAccepted, fedAckFor(t, fh.peer, next.ID).Status)
	require.NoError(t, db.DisableFederationRepo(repo.ID))
	rec = fedHuman(t, fh.f, http.MethodPost, "/v1/federation/bundle-offers/"+next.ID+"/import", map[string]any{"landing": "repo:" + repo.ID, "apply": true})
	require.Equal(t, 409, rec.Code)
	require.Contains(t, rec.Body.String(), "landing_candidate_changed")
	raw, err := json.Marshal(b.Manifest.Agent.Paths)
	require.NoError(t, err)
	require.Contains(t, string(raw), "repo_url")
}

func TestFederationLandingMovePreparationFailureRetries(t *testing.T) {
	fh := newFedHarness(t)
	fh.f.HaveGroup("team")
	fedReceiveAgents(t, fh, "team")
	clone := fedJobRepo(t, fh)
	repo, err := db.GetFederationRepo("project")
	require.NoError(t, err)
	repo.Definition.URL = "https://127.0.0.1:1/project.git"
	require.NoError(t, db.SaveFederationRepo(repo))
	b := fedAgentBundle(t)
	b.Manifest.Agent.Paths.RepoURL = repo.Definition.URL
	b.SetHistory("claude-jsonl", moveSourceConv, []byte(`{"type":"user","sessionId":"`+moveSourceConv+`","cwd":"/source","message":{"content":"continue"}}`+"\n"))
	raw, err := b.Encode()
	require.NoError(t, err)
	d := bundletransfer.New(bundletransfer.Agent, raw, "Move", time.Now().Add(time.Hour))
	d.Group = "team"
	d.Move = &bundletransfer.MoveIntent{SourceAgent: "agt_bobremote0000000000000000", SourceConv: moveSourceConv}
	env := fh.peer.envelope(proto.KindBundleOffer, proto.Endpoint{}, d)
	env.ID = d.ID
	fh.peer.send(env)
	require.Equal(t, proto.AckAccepted, fedAckFor(t, fh.peer, d.ID).Status)
	database, err := db.Open()
	require.NoError(t, err)
	_, err = database.Exec(`CREATE TRIGGER fail_landing_move BEFORE INSERT ON federation_agent_moves BEGIN SELECT RAISE(ABORT,'injected before dispatch'); END`)
	require.NoError(t, err)
	path := "/v1/federation/bundle-offers/" + d.ID + "/import"
	preview := fedHuman(t, fh.f, http.MethodPost, path, nil)
	require.Equal(t, 200, preview.Code, preview.Body.String())
	var result landingFlowReply
	testharness.DecodeJSON(t, preview, &result)
	failed := fedHuman(t, fh.f, http.MethodPost, path, map[string]any{"apply": true})
	require.Equal(t, 400, failed.Code, failed.Body.String())
	row, err := db.GetFederationBundleOffer("in", fh.peer.id.ID(), d.ID)
	require.NoError(t, err)
	require.Empty(t, row.ImportAgent)
	_, err = os.Stat(filepath.Dir(result.Landing.Cwd))
	require.True(t, os.IsNotExist(err), "released undispatched checkout is removed")
	_, err = database.Exec(`DROP TRIGGER fail_landing_move`)
	require.NoError(t, err)
	retried := fedHuman(t, fh.f, http.MethodPost, path, map[string]any{"apply": true})
	require.Equal(t, 200, retried.Code, retried.Body.String())
	data, err := os.ReadFile(filepath.Join(result.Landing.Cwd, "hello"))
	require.NoError(t, err)
	require.Equal(t, "from git\n", string(data))
	require.NotEqual(t, clone, result.Landing.Cwd)
}
func TestFederationLandingPendingTeleportGroupDefault(t *testing.T) {
	fh := newFedHarness(t)
	fh.f.HaveGroup("receiver")
	fedReceiveAgents(t, fh, "receiver")
	cwd := testutil.CanonicalTempDir(t)
	_, err := db.SetAgentGroupDefaultCwd("receiver", cwd)
	require.NoError(t, err)
	d := fedIncomingTeleport(t, fh, "local", nil)
	require.Equal(t, proto.AckAccepted, fedAckFor(t, fh.peer, d.ID).Status)
	rec := fedHuman(t, fh.f, http.MethodPost, "/v1/federation/bundle-offers/"+d.ID+"/import", map[string]any{"apply": true})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"reason":"group_default"`)
	row, err := db.GetFederationTeleport("in", fh.peer.id.ID(), d.ID)
	require.NoError(t, err)
	require.Equal(t, "landed", row.State)
	a, err := db.GetAgent(row.TargetAgent)
	require.NoError(t, err)
	sessions, err := db.FindSessionsByConvID(a.CurrentConvID)
	require.NoError(t, err)
	require.Equal(t, cwd, sessions[0].Cwd)
}
func TestFederationLandingAutomaticPolicyFallback(t *testing.T) {
	fh := newFedHarness(t)
	fh.f.HaveGroup("receiver")
	cwd := testutil.CanonicalTempDir(t)
	profile := &db.SpawnProfile{Name: "landing-fallback", Harness: "claude", Approval: "default"}
	_, err := db.CreateSpawnProfile(profile)
	require.NoError(t, err)
	policy := fedNodeProfile(t, fh, "fallback", db.FederationNodeProfileSpec{PeerGrants: []db.FederationPeerGrant{{Slug: agentd.PermAgentsTeleportReceive, Scope: "group=receiver"}}, TeleportLanding: &db.FederationTeleportLanding{Group: "receiver", Cwd: cwd, SpawnProfile: profile.Name, MaxLive: 1}})
	fedApplyNodeProfile(t, fh, policy)
	d := fedIncomingTeleport(t, fh, "local", nil)
	require.Equal(t, proto.AckAccepted, fedAckFor(t, fh.peer, d.ID).Status)
	var row *db.FederationTeleport
	fedEventually(t, "fallback teleport lands", func() bool {
		row, _ = db.GetFederationTeleport("in", fh.peer.id.ID(), d.ID)
		return row != nil && row.State == "landed"
	})
	a, err := db.GetAgent(row.TargetAgent)
	require.NoError(t, err)
	sessions, err := db.FindSessionsByConvID(a.CurrentConvID)
	require.NoError(t, err)
	require.Equal(t, cwd, sessions[0].Cwd)
}

func TestFederationLandingRecoversUndispatchedCheckout(t *testing.T) {
	fh := newFedHarness(t)
	fh.f.HaveGroup("team")
	fedReceiveAgents(t, fh, "team")
	fedJobRepo(t, fh)
	repo, err := db.GetFederationRepo("project")
	require.NoError(t, err)
	repo.Definition.URL = "https://127.0.0.1:1/project.git"
	require.NoError(t, db.SaveFederationRepo(repo))
	for _, dispatched := range []bool{false, true} {
		t.Run(map[bool]string{false: "undispatched", true: "uncertain_dispatched"}[dispatched], func(t *testing.T) {
			b := fedAgentBundle(t)
			b.Manifest.Agent.Paths.RepoURL = repo.Definition.URL
			d := fedAgentOffer(t, fh.peer, "team", b)
			require.Equal(t, proto.AckAccepted, fedAckFor(t, fh.peer, d.ID).Status)
			path := "/v1/federation/bundle-offers/" + d.ID + "/import"
			rec := fedHuman(t, fh.f, http.MethodPost, path, nil)
			require.Equal(t, 200, rec.Code, rec.Body.String())
			var reply landingFlowReply
			testharness.DecodeJSON(t, rec, &reply)
			reserved := db.NewAgentID()
			require.NoError(t, db.ReserveFederationBundleImport(fh.peer.id.ID(), d.ID, reserved))
			root := filepath.Dir(reply.Landing.Cwd)
			require.NoError(t, os.MkdirAll(root, 0700))
			marker := filepath.Join(root, "partial")
			require.NoError(t, os.WriteFile(marker, []byte("interrupted preparation"), 0600))
			if dispatched {
				require.NoError(t, db.SetFederationBundleLaunchLabel(reserved, "uncertain-worker"))
			}
			// The real list route runs daemon recovery; simulate its durable crash state
			// without killing the test process or replacing the import implementation.
			rec = fedHuman(t, fh.f, http.MethodGet, "/v1/federation/bundle-offers?direction=in", nil)
			require.Equal(t, 200, rec.Code, rec.Body.String())
			row, err := db.GetFederationBundleOffer("in", fh.peer.id.ID(), d.ID)
			require.NoError(t, err)
			if dispatched {
				require.Equal(t, reserved, row.ImportAgent)
				_, err = os.Stat(marker)
				require.NoError(t, err)
				rec = fedHuman(t, fh.f, http.MethodPost, path, map[string]any{"apply": true})
				require.Equal(t, 409, rec.Code)
				require.Contains(t, rec.Body.String(), "launch_reserved")
				return
			}
			require.Empty(t, row.ImportAgent)
			_, err = os.Stat(root)
			require.True(t, os.IsNotExist(err))
			rec = fedHuman(t, fh.f, http.MethodPost, path, map[string]any{"apply": true})
			require.Equal(t, 200, rec.Code, rec.Body.String())
			_, err = os.Stat(filepath.Join(reply.Landing.Cwd, "hello"))
			require.NoError(t, err)
		})
	}
}
