package agentd_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

func fedSelectableProfile(t *testing.T, name, model string) {
	t.Helper()
	_, err := db.CreateSpawnProfile(&db.SpawnProfile{Name: name, Harness: "claude", Model: model, Effort: "high", ModelProxy: "private@unadvertised", InitialMessage: "private-profile-context"})
	require.NoError(t, err)
}
func fedSelectableGrant(t *testing.T, fh *fedHarness, names ...string) {
	t.Helper()
	rec := fedHuman(t, fh.f, http.MethodPost, "/v1/federation/grants", map[string]any{"peer": "bob", "slug": agentd.PermGroupsMembersSpawn, "scope": "group=team", "spawn_policy": map[string]any{"allowed_profiles": names, "max_live": 8}})
	require.Equal(t, 200, rec.Code, rec.Body.String())
}
func TestFederation_SelectableProfilesCatalogAndReceiverLaunch(t *testing.T) {
	fh := newFedHarness(t)
	fh.f.HaveGroup("team")
	fedSelectableProfile(t, "group-default", "haiku")
	fedSelectableProfile(t, "reviewer", "sonnet")
	_, err := db.SetAgentGroupDefaultProfile("team", "group-default")
	require.NoError(t, err)
	fedSelectableGrant(t, fh, "reviewer")
	fedEventually(t, "safe selectable profile catalog", func() bool {
		cats := fh.peer.envelopes(proto.KindCatalog)
		if len(cats) == 0 {
			return false
		}
		var cat proto.CatalogPayload
		if cats[len(cats)-1].DecodePayload(&cat) != nil || len(cat.Groups) != 1 || len(cat.Groups[0].SpawnProfiles) != 1 {
			return false
		}
		p := cat.Groups[0].SpawnProfiles[0]
		require.Equal(t, proto.CatalogSpawnProfile{Name: "reviewer", Harness: "claude", Model: "sonnet", Effort: "high"}, p)
		raw, e := json.Marshal(cat)
		require.NoError(t, e)
		require.NotContains(t, string(raw), "private-profile-context")
		require.NotContains(t, string(raw), "private@unadvertised")
		require.NotContains(t, string(raw), "group-default")
		return true
	})
	var births atomic.Int32
	previous := agentd.Spawn
	agentd.Spawn = &fedProfileBirthSpawner{inner: previous, check: func(a clcommon.SpawnArgs) {
		index := births.Add(1)
		require.Equal(t, "claude", a.Harness)
		require.Equal(t, "off", a.ModelProxy, "explicit local credentials override the selected profile gateway")
		if index == 1 {
			require.Equal(t, "sonnet", a.Model)
		} else {
			require.Equal(t, "haiku", a.Model)
		}
	}}
	t.Cleanup(func() { agentd.Spawn = previous })
	for _, profile := range []string{"reviewer", ""} {
		env := fh.peer.envelope(proto.KindSpawnReq, proto.Endpoint{}, proto.SpawnRequestPayload{Group: "team", Profile: profile, Brief: "review", Credentials: "local"})
		fh.peer.send(env)
		require.Equal(t, proto.AckAccepted, fedAckFor(t, fh.peer, env.ID).Status)
		fedEventually(t, "selected profile worker", func() bool {
			rows, e := db.ListFederationSpawnRequests(100)
			if e != nil {
				return false
			}
			for _, row := range rows {
				if row.EnvelopeID == env.ID {
					require.Equal(t, profile, row.Profile)
					return row.Status == db.FedSpawnApproved
				}
			}
			return false
		})
	}
	require.EqualValues(t, 2, births.Load())
	for _, profile := range []string{"group-default", "unknown"} {
		env := fh.peer.envelope(proto.KindSpawnReq, proto.Endpoint{}, proto.SpawnRequestPayload{Group: "team", Profile: profile, Brief: "review"})
		fh.peer.send(env)
		ack := fedAckFor(t, fh.peer, env.ID)
		require.Equal(t, proto.AckRefused, ack.Status)
		require.Equal(t, "profile_not_allowed", ack.Code)
	}
	require.EqualValues(t, 2, births.Load())
}

func TestFederation_SelectableProfileRecheckedAfterFailedLaunch(t *testing.T) {
	fh := newFedHarness(t)
	fh.f.HaveGroup("team")
	fedSelectableProfile(t, "reviewer", "sonnet")
	fedSelectableGrant(t, fh, "reviewer")
	rec := fedHuman(t, fh.f, http.MethodPost, "/v1/federation/grants", map[string]any{"peer": "bob", "slug": agentd.PermGroupsMembersSpawn, "scope": "group=team", "spawn_policy": map[string]any{"allowed_profiles": []string{"reviewer"}, "harness": "missing-harness", "max_live": 8}})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	env := fh.peer.envelope(proto.KindSpawnReq, proto.Endpoint{}, proto.SpawnRequestPayload{Group: "team", Profile: "reviewer", Brief: "review", Credentials: "local"})
	fh.peer.send(env)
	require.Equal(t, proto.AckAccepted, fedAckFor(t, fh.peer, env.ID).Status)
	var pending *db.FederationSpawnRequest
	fedEventually(t, "failed selected-profile request", func() bool {
		rows, e := db.ListFederationSpawnRequests(100)
		if e != nil {
			return false
		}
		for _, row := range rows {
			if row.EnvelopeID == env.ID && row.Status == db.FedSpawnPending && row.Reason != "" {
				pending = row
				return true
			}
		}
		return false
	})
	fedSelectableGrant(t, fh)
	rec = fedHuman(t, fh.f, http.MethodPost, fmt.Sprintf("/v1/federation/spawn-requests/%d/approve", pending.ID), map[string]any{})
	require.Equal(t, 409, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "profile_not_allowed")
	require.NoError(t, db.PutFederationCatalog(fh.peer.id.ID(), string(mustJSON(t, proto.CatalogPayload{Groups: []proto.CatalogGroup{{Name: "remote", Caps: []string{proto.CapSpawn}, SpawnProfiles: []proto.CatalogSpawnProfile{{Name: "reviewer", Harness: "claude"}}}}})), time.Now()))
	for _, tc := range []struct {
		profile string
		status  int
	}{{"reviewer", 200}, {"unknown", 403}} {
		rec = fedHuman(t, fh.f, http.MethodPost, "/v1/federation/spawn-requests", map[string]any{"peer": "bob", "group": "remote", "profile": tc.profile, "brief": "review"})
		require.Equal(t, tc.status, rec.Code, rec.Body.String())
	}
}
