package sandboxpolicy

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestResolverFilesystemConflictReachesTheInodeEveryWayAGrantCan is TCL-883's
// core: the Unix-socket axis was never the only route to a resolver socket.
// Each case here is a distinct way a filesystem grant reaches the same inode,
// and each must refuse; the negative cases below bound the refusal so it stays
// a security rule rather than a path-shaped superstition.
func TestResolverFilesystemConflictReachesTheInodeEveryWayAGrantCan(t *testing.T) {
	for _, testCase := range []struct {
		name             string
		grants           []FilesystemGrant
		expectedSelector string
	}{
		{
			name: "the socket path itself",
			grants: []FilesystemGrant{
				{Path: "/run/systemd/resolve/io.systemd.Resolve", Access: AccessRead},
			},
			expectedSelector: "/run/systemd/resolve/io.systemd.Resolve",
		},
		{
			name: "the directory the socket lives in",
			grants: []FilesystemGrant{
				{Path: "/run/systemd/resolve", Access: AccessRead},
			},
			expectedSelector: "/run/systemd/resolve",
		},
		{
			name: "a distant ancestor",
			grants: []FilesystemGrant{
				{Path: "/run", Access: AccessRead},
			},
			expectedSelector: "/run",
		},
		{
			name: "an unnormalized spelling of an ancestor",
			grants: []FilesystemGrant{
				{Path: "/run/systemd/../systemd/resolve/", Access: AccessRead},
			},
			expectedSelector: "/run/systemd/../systemd/resolve/",
		},
		{
			// The remap changes WHERE the socket appears inside the namespace,
			// never WHICH inode is bound. Reporting the host spelling is what
			// makes the refusal actionable: that is the row the operator edits.
			name: "a grant remapped to an innocent-looking sandbox path",
			grants: []FilesystemGrant{
				{
					Path:      "/run/systemd/resolve",
					MountPath: "/opt/vendor-runtime",
					Access:    AccessRead,
				},
			},
			expectedSelector: "/run/systemd/resolve",
		},
		{
			// The whole point of stating the mode is not part of "reaches it":
			// read-only governs filesystem operations, and connect(2) on a unix
			// socket is not one of them.
			name: "a read-only grant, because read-only does not stop connect(2)",
			grants: []FilesystemGrant{
				{Path: "/var/lib/sss/pipes", Access: AccessRead},
			},
			expectedSelector: "/var/lib/sss/pipes",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			selector, resolver, found := NetworkEngineResolverFilesystemConflict(
				NetworkEngineProxy, testCase.grants)
			require.True(t, found,
				"a grant reaching a known resolver socket must refuse the proxy engine")
			assert.Equal(t, testCase.expectedSelector, selector,
				"the refusal must name the authored row, verbatim, so it can be edited")
			assert.Contains(t, KnownResolverSocketPaths(), resolver)
		})
	}
}

// TestResolverFilesystemConflictLeavesInnocentPoliciesAlone is the
// anti-overreach half. A refusal that fired on everything would be useless as a
// signal and would make the proxy engine unusable.
func TestResolverFilesystemConflictLeavesInnocentPoliciesAlone(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		engine NetworkEngine
		grants []FilesystemGrant
	}{
		{
			name:   "a grant that covers no known resolver",
			engine: NetworkEngineProxy,
			grants: []FilesystemGrant{
				{Path: "/home/agent/workspace", Access: AccessWrite},
				{Path: "/run/user/1000/podman", Access: AccessRead},
			},
		},
		{
			// A sibling directory whose name merely shares a prefix is not an
			// ancestor. Without the separator this would match /run/systemd*.
			name:   "a sibling directory sharing a name prefix",
			engine: NetworkEngineProxy,
			grants: []FilesystemGrant{
				{Path: "/run/systemd/resolve-cache", Access: AccessRead},
			},
		},
		{
			// The packet gateway's DNS broker holds name authority WITH a
			// resolver present, so nothing is taken away from it here.
			name:   "the same grant under the packet engine",
			engine: NetworkEnginePacket,
			grants: []FilesystemGrant{
				{Path: "/run/systemd/resolve", Access: AccessRead},
			},
		},
		{
			name:   "the same grant when no engine is deployed",
			engine: NetworkEngineUnset,
			grants: []FilesystemGrant{
				{Path: "/run/systemd/resolve", Access: AccessRead},
			},
		},
		{
			name:   "an explicit deny of the resolver directory",
			engine: NetworkEngineProxy,
			grants: []FilesystemGrant{
				{Path: "/run/systemd/resolve", Access: AccessDeny},
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, _, found := NetworkEngineResolverFilesystemConflict(
				testCase.engine, testCase.grants)
			assert.False(t, found)
		})
	}
}

// TestResolverFilesystemConflictHonorsMountPlanShadowing keeps the check
// consistent with the renderer it describes. Entries fold by GUEST path and the
// most specific wins, so a deny that lands on the resolver's own guest position
// really does hide the socket — and one that lands elsewhere really does not.
func TestResolverFilesystemConflictHonorsMountPlanShadowing(t *testing.T) {
	shadowed := []FilesystemGrant{
		{Path: "/run", Access: AccessRead},
		{Path: "/run/systemd/resolve", Access: AccessDeny},
		{Path: "/run/nscd", Access: AccessDeny},
		{Path: "/var/lib/sss/pipes", Access: AccessDeny},
		{Path: "/var/run", Access: AccessDeny},
	}
	_, _, found := NetworkEngineResolverFilesystemConflict(
		NetworkEngineProxy, shadowed)
	assert.False(t, found,
		"a deny at the resolver's own guest position shadows the broader grant")

	// The same deny cannot rescue a REMAPPED grant: the deny keeps host-path
	// semantics (deny + mount_path is a profile-layer error), so it never lands
	// on the guest position the remap created, and the socket is reachable
	// there under its new name.
	remapped := append([]FilesystemGrant(nil), shadowed...)
	remapped = append(remapped, FilesystemGrant{
		Path:      "/run/systemd",
		MountPath: "/opt/systemd",
		Access:    AccessRead,
	})
	selector, resolver, found := NetworkEngineResolverFilesystemConflict(
		NetworkEngineProxy, remapped)
	require.True(t, found,
		"a deny at the host position does not shadow a remapped guest position")
	assert.Equal(t, "/run/systemd", selector)
	assert.Contains(t, resolver, "io.systemd.Resolve")

	// A LESS specific deny does not win over a more specific grant, for the
	// same reason the renderer would not let it.
	broadDeny := []FilesystemGrant{
		{Path: "/run", Access: AccessDeny},
		{Path: "/run/systemd/resolve", Access: AccessRead},
	}
	selector, _, found = NetworkEngineResolverFilesystemConflict(
		NetworkEngineProxy, broadDeny)
	require.True(t, found)
	assert.Equal(t, "/run/systemd/resolve", selector)
}

// TestBothResolverAxesShareOneList is the no-forking gate. The two refusals
// exist because two authored axes reach the same inodes; if a future resolver
// were added to only one of them, the other axis would silently admit it.
func TestBothResolverAxesShareOneList(t *testing.T) {
	known := KnownResolverSocketPaths()
	require.NotEmpty(t, known)
	for _, path := range known {
		_, _, socketFound := NetworkEngineResolverSocketConflict(
			NetworkEngineProxy,
			UnixSocketRules{
				Mode:  AccessModeList,
				Allow: []SocketAllowEntry{{Path: path}},
			},
		)
		assert.Truef(t, socketFound,
			"%s must be refused on the unix_sockets axis", path)

		_, _, filesystemFound := NetworkEngineResolverFilesystemConflict(
			NetworkEngineProxy,
			[]FilesystemGrant{{Path: path, Access: AccessRead}},
		)
		assert.Truef(t, filesystemFound,
			"%s must be refused on the filesystem axis", path)
	}
}

// TestResolverFilesystemRefusalNamesItsRemedy holds the refusal to the
// capability-phrasing rule: it says which capability is missing, which authored
// row takes it away, and what the operator can do instead.
func TestResolverFilesystemRefusalNamesItsRemedy(t *testing.T) {
	refusal := NetworkEngineResolverFilesystemRefusal(
		"/run", "/run/systemd/resolve/io.systemd.Resolve")
	assert.Contains(t, refusal, "missing capability proxy_engine_name_authority")
	assert.Contains(t, refusal, "/run")
	assert.Contains(t, refusal, "/run/systemd/resolve/io.systemd.Resolve")
	assert.Contains(t, refusal, "narrow that grant")
	assert.Contains(t, refusal, "Packet filter engine")
	assert.Contains(t, refusal, "read-only does not stop connect(2)",
		"the refusal must explain why a read-only grant is not a remedy")
}

// TestEffectiveAxesCarryTheFilesystem is the seam test. The refusal above can
// only reach the capability table if the axes that table reads actually carry
// the effective filesystem; before TCL-883 they carried the two access axes
// alone, which is why this conflict had no seam to be checked at.
func TestEffectiveAxesCarryTheFilesystem(t *testing.T) {
	grants := []FilesystemGrant{
		{Path: "/run/systemd/resolve", Access: AccessRead},
	}
	axes, err := EffectiveAccessAxes(EffectiveProfile{
		Filesystem: grants,
		Network: &NetworkRules{
			Mode:   AccessModeList,
			Allow:  []NetworkAllowEntry{{Domain: "example.com"}},
			Engine: NetworkEngineProxy,
		},
	})
	require.NoError(t, err)
	assert.Equal(t, grants, axes.Filesystem)

	planned, err := PlannedEffectiveAccessAxes(EffectiveProfile{
		Filesystem: grants,
		Network: &NetworkRules{
			Mode:   AccessModeList,
			Allow:  []NetworkAllowEntry{{Domain: "example.com"}},
			Engine: NetworkEngineProxy,
		},
	})
	require.NoError(t, err)
	assert.Equal(t, grants, planned.Filesystem,
		"the planned axes the renderer reads must see the filesystem too")

	// The derivation copies rather than aliases, so a consumer that rewrites
	// its axes cannot reach back into the profile it was derived from.
	axes.Filesystem[0].Path = "/mutated"
	assert.Equal(t, "/run/systemd/resolve", grants[0].Path)
}
