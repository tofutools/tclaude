package agentd

import "testing"

// The peer dashboard's control lists are resolved through
// fedExplicitGroupGrant, which ignores slugs that are not federation peer
// slugs. Every control grant must therefore be a known peer slug, or the
// dashboard would silently never enable its control.
func TestPeerControlGrantsAreKnownPeerSlugs(t *testing.T) {
	for name, slugs := range map[string][]string{
		"peerGroupControlGrants": peerGroupControlGrants,
		"peerAgentControlGrants": peerAgentControlGrants,
		// peerGroupGrants also asks about the spawn launch settings.
		"spawn settings": {PermGroupsMembersSpawnCwd, PermGroupsMembersSpawnPrompts, PermGroupsMembersSpawnSandbox, PermGroupsMembersSpawnAll},
	} {
		for _, slug := range slugs {
			if _, ok := federationPeerSlugs[slug]; !ok {
				t.Errorf("%s: %q is not in federationPeerSlugs", name, slug)
			}
		}
	}
}
