package agentd

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Every conferrable slug is a real local gate the registry maps to a live
// peer grant, so the ceiling's lookups never fall through to "nothing to
// check".
func TestPeerCeilingSlugsResolve(t *testing.T) {
	for slug := range peerCeilingSlugs {
		require.NotEmpty(t, findPermissionSlug(slug), "%s is a local permission", slug)
		via, err := peerCeilingVia(slug)
		require.NoError(t, err, slug)
		require.NotEmpty(t, via, slug)
		for _, v := range via {
			require.False(t, fedPeerSlugRequiresGroup(v), "%s: an unscoped row could never be judged", slug)
		}
	}
}

func findPermissionSlug(slug string) string {
	for _, p := range permissionRegistry {
		if p.Slug == slug {
			return p.Slug
		}
	}
	return ""
}

// A pending grant covers nothing, should one ever be listed.
func TestPeerCeilingRefusesAPendingGrant(t *testing.T) {
	require.True(t, slugFederationReach[PermGroupsMembersMorph].Pending, "pick another pending group slug")
	peerCeilingSlugs[PermGroupsMembersMorph] = true
	defer delete(peerCeilingSlugs, PermGroupsMembersMorph)
	_, err := peerCeilingVia(PermGroupsMembersMorph)
	require.Error(t, err)
}
