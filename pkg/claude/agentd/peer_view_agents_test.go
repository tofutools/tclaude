package agentd

import "testing"

// Every agent peer view route is a peer view rule the receiver serves under
// a grant, never one only unrestricted trust holds: a typo would leave the
// route refused, and an unrestrictedOnly route must never reach an agent.
func TestAgentPeerViewRoutesAreGrantedPeerViewRules(t *testing.T) {
	rules := peerViewRules()
	for pattern, route := range agentPeerViewRoutes {
		rule, ok := rules[pattern]
		if !ok {
			t.Errorf("%s is not a peer view rule", pattern)
			continue
		}
		if rule.unrestrictedOnly || rule.requires == "local_only" || (rule.serve == nil && rule.write == nil) {
			t.Errorf("%s is not a granted peer view rule", pattern)
		}
		if len(route.slugs) == 0 && route.fields == nil {
			t.Errorf("%s names no slug", pattern)
		}
		if p, _, ok := matchAgentPeerViewRoute(methodOf(pattern), samplePath(pattern)); !ok || p != pattern {
			t.Errorf("%s does not match its own path (got %q)", pattern, p)
		}
	}
}

func methodOf(pattern string) string {
	for i := range pattern {
		if pattern[i] == ' ' {
			return pattern[:i]
		}
	}
	return ""
}

func samplePath(pattern string) string {
	path := pattern[len(methodOf(pattern))+1:]
	out := []byte{}
	for i := 0; i < len(path); i++ {
		if path[i] == '{' {
			for path[i] != '}' {
				i++
			}
			out = append(out, 'x')
			continue
		}
		out = append(out, path[i])
	}
	return string(out)
}
