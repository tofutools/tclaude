package agentd

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

func TestPeerScopeSeparatesLocalAndFederatedActions(t *testing.T) {
	remote := ActionContext{RemotePeer: "inst_peer", RemoteGroup: "builders"}
	for _, tc := range []struct {
		raw    string
		action ActionContext
		allow  bool
	}{
		{"", remote, false},
		{`{"group":["builders"]}`, remote, false},
		{`{"peer":["inst_peer"]}`, remote, true},
		{`{"peer":["inst_peer/builders"]}`, remote, true},
		{`{"peer":["inst_peer/other"]}`, remote, false},
		{`{"peer":["inst_other"]}`, remote, false},
		{`{"peer":["inst_peer/builders"]}`, ActionContext{Group: "builders"}, false},
		{`{"peer":["inst_peer"]}`, ActionContext{RemotePeer: "inst_peer", RemoteGroup: "other"}, true},
	} {
		v := permVerdict{Resolution: permAllow, ScopeJSON: []string{tc.raw}}
		require.Equal(t, tc.allow, evalPermissionScope(v, "caller", tc.action).Satisfied, "%s %+v", tc.raw, tc.action)
	}
	// Unscoped grants in another membership must not absorb peer constraints.
	v := permVerdict{Resolution: permAllow, ScopeJSON: []string{"", `{"peer":["inst_peer/other"]}`}}
	require.False(t, evalPermissionScope(v, "caller", remote).Satisfied)
}

func TestRemotePermissionIgnoresLocalSourcesAndStructuralAuthority(t *testing.T) {
	const slug = PermGroupsMembersSpawn
	src := permSources{resolvable: true,
		sudo:     map[string]sudoPermSource{slug: {ID: 1}},
		override: map[string]overridePermSource{slug: {Effect: db.PermEffectGrant}},
		group:    map[string][]string{slug: {"", `{"group":["builders"]}`, `{"peer":["inst_peer/builders"]}`}},
	}
	v := resolveRemotePermissionVerdictFrom(src, slug)
	require.Equal(t, permSourceGroup, v.Source)
	require.Equal(t, []string{`{"peer":["inst_peer/builders"]}`}, v.ScopeJSON)
	allowed, _ := permissionVerdictAllowsAction(v, "caller", slug, ActionContext{RemotePeer: "inst_peer", RemoteGroup: "builders"})
	require.True(t, allowed)
	// A deny suppresses both remote grants and local ownership.
	src.override[slug] = overridePermSource{Effect: db.PermEffectDeny}
	v = resolveRemotePermissionVerdictFrom(src, slug)
	require.Equal(t, permDeny, v.Resolution)
	allowed, _ = permissionVerdictAllowsAction(v, "caller", slug, ActionContext{RemotePeer: "inst_peer", RemoteGroup: "builders", structuralGroup: "builders"})
	require.False(t, allowed)
	allowed, _ = permissionVerdictAllowsAction(permVerdict{Resolution: permUndecided}, "caller", slug, ActionContext{RemotePeer: "inst_peer", structuralGroup: "builders"})
	require.False(t, allowed)
}
