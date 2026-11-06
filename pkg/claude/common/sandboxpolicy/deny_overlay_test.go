package sandboxpolicy

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func accessAt(s Snapshot, path string) (Access, bool) {
	for _, grant := range s.Effective.Filesystem {
		if grant.GuestPath() == path {
			return grant.Access, true
		}
	}
	return "", false
}

func overlayNotice(s Snapshot) *AccessNotice {
	for i := range s.Effective.AccessNotices {
		if s.Effective.AccessNotices[i].Reason == AccessNoticeReasonDenyOverlayRemoved {
			return &s.Effective.AccessNotices[i]
		}
	}
	return nil
}

// A spawn-time deny covers its whole subtree: a more specific grant beneath
// it, which profile composition alone would keep, is removed and named.
func TestDenyOverlayRemovesGrantsBeneathIt(t *testing.T) {
	home, workspace, secrets := denyLineageDirs(t)
	nested := filepath.Join(secrets, "shared")
	require.NoError(t, os.MkdirAll(nested, 0o755))
	base := snapshotFor(t,
		FilesystemGrant{Path: home, Access: AccessRead},
		FilesystemGrant{Path: workspace, Access: AccessWrite},
		FilesystemGrant{Path: nested, Access: AccessWrite},
	)

	got, err := ApplyDenyOverlay(base, []string{secrets})
	require.NoError(t, err)
	assert.Equal(t, []string{secrets}, got.DenyOverlay)
	access, ok := accessAt(got, secrets)
	require.True(t, ok)
	assert.Equal(t, AccessDeny, access)
	_, ok = accessAt(got, nested)
	assert.False(t, ok, "the nested write grant is removed, not kept by specificity")
	access, _ = accessAt(got, home)
	assert.Equal(t, AccessRead, access, "a grant above the deny is narrowed by it, not removed")
	access, _ = accessAt(got, workspace)
	assert.Equal(t, AccessWrite, access, "a sibling is untouched")
	for _, shape := range ReopensUnderDeny(got.Effective.Filesystem) {
		assert.NotEqual(t, secrets, shape.Deny, "nothing reopens beneath the overlay: %v", shape.Reopen)
	}
	notice := overlayNotice(got)
	require.NotNil(t, notice, "the removal is named")
	assert.Equal(t, AccessNoticeClassComposition, notice.Class)
	assert.Contains(t, notice.Detail, "write "+nested)
	assert.Equal(t, []ProfileSource{{Scope: ScopeDenyOverlay, Profile: DenyOverlaySourceName}},
		got.Effective.Provenance.Filesystem[secrets])
	_, stillListed := got.Effective.Provenance.Filesystem[nested]
	assert.False(t, stillListed)

	again, err := ApplyDenyOverlay(got, nil)
	require.NoError(t, err)
	assert.Equal(t, got.Effective.Filesystem, again.Effective.Filesystem, "reapplying is a no-op")
	assert.Len(t, again.Effective.AccessNotices, len(got.Effective.AccessNotices), "and does not repeat the notice")

	// The overlay only narrows: the result fits inside what it was laid over.
	require.NoError(t, RequireContained(base, got))
	// A child keeping the overlay fits inside it, and one that dropped it does not.
	require.NoError(t, RequireContained(got, again))
	assert.Error(t, RequireContained(got, base))
}

// The overlay outlives the profiles it was laid over. A profile edit that later
// adds a grant beneath a denied path is removed again on the next application,
// so the edit cannot quietly reopen it.
func TestDenyOverlaySurvivesAProfileEditBeneathIt(t *testing.T) {
	home, workspace, secrets := denyLineageDirs(t)
	first, err := ApplyDenyOverlay(snapshotFor(t, FilesystemGrant{Path: home, Access: AccessRead}), []string{secrets})
	require.NoError(t, err)
	assert.Nil(t, overlayNotice(first), "nothing was removed, so nothing is named")

	edited := snapshotFor(t,
		FilesystemGrant{Path: home, Access: AccessRead},
		FilesystemGrant{Path: workspace, Access: AccessWrite},
		FilesystemGrant{Path: secrets, Access: AccessWrite},
	)
	edited.DenyOverlay = first.DenyOverlay
	relaunched, err := ApplyDenyOverlay(edited, nil)
	require.NoError(t, err)
	access, _ := accessAt(relaunched, secrets)
	assert.Equal(t, AccessDeny, access, "a grant of the denied path itself becomes the deny")
	access, _ = accessAt(relaunched, workspace)
	assert.Equal(t, AccessWrite, access)
}

// A launch that would open a path under the overlay, as its own workspace,
// Git or harness-state reopen, is refused and the path named.
func TestValidateDenyOverlayHeldNamesALaunchReopen(t *testing.T) {
	home, workspace, secrets := denyLineageDirs(t)
	require.NoError(t, ValidateDenyOverlayHeld(nil, []string{secrets}))
	require.NoError(t, ValidateDenyOverlayHeld([]string{secrets}, []string{home, workspace}),
		"a path above or beside the deny is not under it")
	err := ValidateDenyOverlayHeld([]string{secrets}, []string{workspace}, []string{filepath.Join(secrets, "state")})
	require.ErrorIs(t, err, ErrDenyOverlayReopened)
	assert.Contains(t, err.Error(), filepath.Join(secrets, "state")+" (under "+secrets+")")
}

func TestDenyOverlayNormalizesAndRefusesAnOmittedLaunch(t *testing.T) {
	home, _, secrets := denyLineageDirs(t)
	paths, err := NormalizeDenyOverlay([]string{filepath.Join(secrets, "a"), secrets, secrets + "/"})
	require.NoError(t, err)
	assert.Equal(t, []string{secrets}, paths, "a path beneath another denied path folds into it")
	_, err = NormalizeDenyOverlay([]string{"relative/path"})
	assert.Error(t, err)
	_, err = NormalizeDenyOverlay([]string{" "})
	assert.Error(t, err)

	_, err = ApplyDenyOverlay(OmittedProfilesSnapshot(), []string{home})
	assert.ErrorIs(t, err, ErrDenyOverlayUnenforced)
	unchanged, err := ApplyDenyOverlay(OmittedProfilesSnapshot(), nil)
	require.NoError(t, err, "no overlay asks nothing of an omitted launch")
	assert.True(t, unchanged.ProfilesOmitted)
}

// A remapped grant over an ancestor projects the denied subtree to a sandbox
// path the deny row does not cover, and a socket allowed beneath the deny
// stays connectable on a socket rule alone: both are removed and named.
func TestDenyOverlayRemovesARemappedAncestorAndSocketsBeneathIt(t *testing.T) {
	_, workspace, _ := denyLineageDirs(t)
	project := filepath.Join(workspace, "project")
	secrets := filepath.Join(project, "client-secrets")
	require.NoError(t, os.MkdirAll(secrets, 0o755))
	effective, err := Resolve(Scopes{Explicit: &Profile{Name: "p",
		Filesystem: []FilesystemGrant{
			{Path: workspace, Access: AccessWrite},
			{Path: project, Access: AccessRead, MountPath: "/srv/project"},
		},
		UnixSockets: &UnixSocketRules{Mode: AccessModeList, Allow: []SocketAllowEntry{
			{Path: filepath.Join(secrets, "agent.sock")}, {Path: filepath.Join(workspace, "ok.sock")},
		}},
	}})
	require.NoError(t, err)
	got, err := ApplyDenyOverlay(NewSnapshot(effective, nil), []string{secrets})
	require.NoError(t, err)
	_, ok := accessAt(got, "/srv/project")
	assert.False(t, ok, "the remap of the deny's ancestor is removed")
	access, _ := accessAt(got, workspace)
	assert.Equal(t, AccessWrite, access)
	require.NotNil(t, got.Effective.UnixSockets)
	assert.Equal(t, []SocketAllowEntry{{Path: filepath.Join(workspace, "ok.sock")}}, got.Effective.UnixSockets.Allow)
	notice := overlayNotice(got)
	require.NotNil(t, notice)
	assert.Contains(t, notice.Detail, "(mounted at /srv/project)")
	assert.Contains(t, notice.Detail, "unix socket "+filepath.Join(secrets, "agent.sock"))
	require.NoError(t, RequireContained(NewSnapshot(effective, nil), got))
}
