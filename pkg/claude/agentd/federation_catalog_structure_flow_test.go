package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

func TestFederation_CatalogTracksGroupsCreatedAfterTrust(t *testing.T) {
	fh := newFedHarness(t)
	setFedTrustLevel(t, fh, "unrestricted")
	latest := func() proto.CatalogPayload {
		var cat proto.CatalogPayload
		cats := fh.peer.envelopes(proto.KindCatalog)
		if len(cats) > 0 {
			require.NoError(t, cats[len(cats)-1].DecodePayload(&cat))
		}
		return cat
	}
	mutate := func(method, path string, body any, status int) {
		t.Helper()
		rec := fedHuman(t, fh.f, method, path, body)
		require.Equal(t, status, rec.Code, rec.Body.String())
	}
	// Trust is established while no groups exist. Every subsequent catalog must
	// arrive without a second trust/grant action or a manual catalog request.
	mutate("POST", "/v1/groups", map[string]any{"name": "reviewers"}, http.StatusCreated)
	fedEventually(t, "post-trust group catalog", func() bool { return len(latest().Groups) == 1 })
	require.Equal(t, "reviewers", latest().Groups[0].Name)
	require.Contains(t, latest().Groups[0].Caps, proto.CapRoster)

	const conv = "fed-catalog-late-reviewer"
	fh.f.HaveConvWithTitle(conv, "reviewer-1")
	mutate("POST", "/v1/groups/reviewers/members", map[string]any{"conv": conv, "role": "reviewer"}, http.StatusOK)
	fedEventually(t, "new roster member", func() bool { return len(latest().Groups[0].Members) == 1 })
	require.Equal(t, "reviewer-1", latest().Groups[0].Members[0].Name)
	mutate("PATCH", "/v1/groups/reviewers/members/"+conv, map[string]any{"role": "lead"}, http.StatusOK)
	fedEventually(t, "updated roster role", func() bool { return latest().Groups[0].Members[0].Role == "lead" })
	mutate("POST", "/v1/groups/reviewers/rename", map[string]any{"new_name": "renamed"}, http.StatusOK)
	fedEventually(t, "renamed catalog group", func() bool { return latest().Groups[0].Name == "renamed" })
	mutate("DELETE", "/v1/groups/renamed/members/"+conv, nil, http.StatusNoContent)
	fedEventually(t, "removed roster member", func() bool { return len(latest().Groups[0].Members) == 0 })
	mutate("POST", "/v1/groups/renamed/archive", nil, http.StatusOK)
	fedEventually(t, "archived group withdrawn", func() bool { return len(latest().Groups) == 0 })
	mutate("POST", "/v1/groups/renamed/unarchive", nil, http.StatusOK)
	fedEventually(t, "unarchived group restored", func() bool { return len(latest().Groups) == 1 })
	mutate("DELETE", "/v1/groups/renamed", nil, http.StatusNoContent)
	fedEventually(t, "deleted group withdrawn", func() bool { return len(latest().Groups) == 0 })

	// The two-minute safety refresh uses live DB structure too. A write outside
	// the request handlers must not need re-trust or a cached export payload.
	fh.f.HaveGroup("external-write")
	agentd.RefreshFederationCatalogsForTest()
	fedEventually(t, "periodic refresh rebuilds live groups", func() bool { return len(latest().Groups) == 1 && latest().Groups[0].Name == "external-write" })
}
