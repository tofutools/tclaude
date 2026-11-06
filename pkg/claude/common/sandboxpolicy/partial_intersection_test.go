package sandboxpolicy

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A group profile that allows the model endpoint while the global profile's
// list omits it: the intersection drops the endpoint, and the resolver must
// say which tier lost what instead of dropping it silently.
func TestResolveDisclosesPartialNetworkIntersection(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	effective, err := Resolve(Scopes{
		Global: &Profile{Name: "base", Network: &NetworkRules{
			Baseline: NetworkBaselineDeny,
			Allow: []NetworkAllowEntry{
				{Host: "git.example", Ports: []int{443}},
				{Host: "registry.example", Ports: []int{443}},
			},
		}},
		Group: &Profile{Name: "dev", Network: &NetworkRules{
			Baseline: NetworkBaselineDeny,
			Allow: []NetworkAllowEntry{
				{Host: "git.example", Ports: []int{443}},
				{Host: "api.anthropic.com", Ports: []int{443}},
			},
		}},
	})
	require.NoError(t, err)
	require.NotNil(t, effective.Network)
	assert.Equal(t, []NetworkAllowEntry{{Host: "git.example", Ports: []int{443}}},
		effective.Network.Allow, "the notice discloses; it must not change what is allowed")

	var notice *AccessNotice
	for i := range effective.AccessNotices {
		if effective.AccessNotices[i].Reason == AccessNoticeReasonPartialIntersection {
			notice = &effective.AccessNotices[i]
		}
	}
	require.NotNil(t, notice, "notices: %+v", effective.AccessNotices)
	assert.Equal(t, AccessNoticeClassComposition, notice.Class)
	assert.Equal(t, "network", notice.Axis)
	assert.Equal(t, []string{`global "base"`, `group "dev"`}, notice.Tiers)
	assert.Equal(t, []AccessNoticeLayer{
		{Tier: `global "base"`, NotKept: []NetworkAllowEntry{{Host: "registry.example", Ports: []int{443}}}},
		{Tier: `group "dev"`, NotKept: []NetworkAllowEntry{{Host: "api.anthropic.com", Ports: []int{443}}}},
	}, notice.Layers)
	assert.Contains(t, notice.Detail, `group "dev": api.anthropic.com:443`)
	assert.Contains(t, notice.Detail, `global "base": registry.example:443`)

	snapshot := NewSnapshot(effective, nil)
	path, digest, err := WriteSnapshotFile(t.TempDir(), snapshot)
	require.NoError(t, err)
	handedOff, err := ReadSnapshotFile(path, digest)
	require.NoError(t, err)
	assert.Contains(t, handedOff.Effective.AccessNotices, *notice,
		"a relaunch from the snapshot must still be able to name the layers")
}

func TestResolveNoPartialNoticeWhenListsAgreeOrOneTierLists(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	list := func(name string, hosts ...string) *Profile {
		allow := make([]NetworkAllowEntry, 0, len(hosts))
		for _, host := range hosts {
			allow = append(allow, NetworkAllowEntry{Host: host, Ports: []int{443}})
		}
		return &Profile{Name: name, Network: &NetworkRules{
			Baseline: NetworkBaselineDeny, Allow: allow,
		}}
	}
	for name, scopes := range map[string]Scopes{
		"same lists":  {Global: list("a", "x.example"), Group: list("b", "x.example")},
		"one list":    {Group: list("b", "x.example", "y.example")},
		"open global": {Global: &Profile{Name: "open", NetworkAccess: NetworkAccessInternet}, Group: list("b", "x.example")},
	} {
		effective, err := Resolve(scopes)
		require.NoError(t, err, name)
		for _, notice := range effective.AccessNotices {
			assert.NotEqual(t, AccessNoticeReasonPartialIntersection, notice.Reason, name)
		}
	}
}
