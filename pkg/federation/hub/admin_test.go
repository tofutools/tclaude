package hub

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/testutil"
)

func adminTestStore(t *testing.T) (*Store, *proto.Identity, string) {
	t.Helper()
	st, err := OpenStore(filepath.Join(testutil.CanonicalTempDir(t), "hub.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	id, err := proto.NewIdentity()
	require.NoError(t, err)
	require.NoError(t, st.Admit(id.ID()))
	require.NoError(t, st.RecordSeen(id.ID(), id.Pub, "admin", "test", time.Now()))
	token, err := st.PrepareAdminClaim(false, time.Now())
	require.NoError(t, err)
	return st, id, token
}
func requireAdminCode(t *testing.T, err error, code string) {
	t.Helper()
	var e *AdminError
	require.ErrorAs(t, err, &e)
	require.Equal(t, code, e.Code)
}
func TestHubAdminClaimPrivateSingleUseAndRestart(t *testing.T) {
	st, id, token := adminTestStore(t)
	info, err := os.Stat(st.ClaimPath())
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0600), info.Mode().Perm())
	same, err := st.PrepareAdminClaim(false, time.Now())
	require.NoError(t, err)
	require.Equal(t, token, same)
	require.NoError(t, os.Remove(st.ClaimPath()))
	renewed, err := st.PrepareAdminClaim(false, time.Now())
	require.NoError(t, err)
	require.NotEqual(t, token, renewed)
	requireAdminCode(t, st.ClaimAdmin(id.ID(), id.Pub, token, time.Now()), "claim_invalid")
	require.NoError(t, st.ClaimAdmin(id.ID(), id.Pub, renewed, time.Now()))
	require.NoFileExists(t, st.ClaimPath())
	requireAdminCode(t, st.ClaimAdmin(id.ID(), id.Pub, renewed, time.Now()), "claim_used")
	next, err := st.PrepareAdminClaim(false, time.Now().Add(48*time.Hour))
	require.NoError(t, err)
	require.Empty(t, next, "admins must survive restarts")
	caps, err := st.AdminCapabilities(id.ID())
	require.NoError(t, err)
	require.ElementsMatch(t, proto.HubAdminBootstrapCapabilities, caps)
	generation, err := st.AdminGeneration()
	require.NoError(t, err)
	reset, err := st.PrepareAdminClaim(true, time.Now())
	require.NoError(t, err)
	require.NotEmpty(t, reset)
	current, err := st.AdminGeneration()
	require.NoError(t, err)
	require.NotEqual(t, generation, current)
	caps, err = st.AdminCapabilities(id.ID())
	require.NoError(t, err)
	require.Empty(t, caps)
}
func TestHubAdminExpiredAndSymlinkClaims(t *testing.T) {
	st, id, token := adminTestStore(t)
	requireAdminCode(t, st.ClaimAdmin(id.ID(), id.Pub, token, time.Now().Add(25*time.Hour)), "claim_expired")
	renewed, err := st.PrepareAdminClaim(false, time.Now().Add(25*time.Hour))
	require.NoError(t, err)
	require.NotEqual(t, token, renewed)
	require.NoError(t, os.Remove(st.ClaimPath()))
	other := filepath.Join(filepath.Dir(st.ClaimPath()), "secret")
	require.NoError(t, os.WriteFile(other, []byte(renewed), 0600))
	require.NoError(t, os.Symlink(other, st.ClaimPath()))
	_, err = st.PrepareAdminClaim(false, time.Now())
	require.Error(t, err)
}
func TestHubAdminLastManagerAndRecoveryGuard(t *testing.T) {
	st, id, token := adminTestStore(t)
	require.NoError(t, st.ClaimAdmin(id.ID(), id.Pub, token, time.Now()))
	next, _ := proto.NewIdentity()
	require.NoError(t, st.Admit(next.ID()))
	require.NoError(t, st.RecordSeen(next.ID(), next.Pub, "next", "", time.Now()))
	for _, err := range []error{st.RemoveAdmin(id.ID()), st.SetAdmin(id.ID(), id.ID(), []string{"hub.health.read"}), st.Revoke(id.ID()), st.RevokeOldIdentity(id.ID(), time.Now()), st.RecoverIdentity(id.ID(), next.ID(), time.Now())} {
		requireAdminCode(t, err, "last_admin")
	}
	require.Error(t, st.SetAdmin(next.ID(), id.ID(), []string{"hub.exec"}))
	require.NoError(t, st.SetAdmin(next.ID(), id.ID(), []string{"hub.admins.manage", "hub.health.read"}))
	require.NoError(t, st.RecoverIdentity(id.ID(), next.ID(), time.Now()))
	caps, err := st.AdminCapabilities(id.ID())
	require.NoError(t, err)
	require.Empty(t, caps)
	caps, err = st.AdminCapabilities(next.ID())
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"hub.admins.manage", "hub.health.read"}, caps, "recovery never grants predecessor capabilities")
}
func TestHubAdminReplayPersistsAndGenerationReset(t *testing.T) {
	st, id, token := adminTestStore(t)
	require.NoError(t, st.ClaimAdmin(id.ID(), id.Pub, token, time.Now()))
	generation, err := st.AdminGeneration()
	require.NoError(t, err)
	req := &proto.HubAdminRequest{ID: proto.NewEnvelopeID(), Generation: generation, ExpiresAt: time.Now().Add(time.Minute)}
	require.NoError(t, st.AuthorizeAdminRequest(id.ID(), id.Pub, req, "hub.settings.manage"))
	second, err := OpenStore(st.path)
	require.NoError(t, err)
	defer second.Close()
	requireAdminCode(t, second.AuthorizeAdminRequest(id.ID(), id.Pub, req, "hub.settings.manage"), "replay")
	_, err = st.PrepareAdminClaim(true, time.Now())
	require.NoError(t, err)
	req.ID = proto.NewEnvelopeID()
	requireAdminCode(t, st.AuthorizeAdminRequest(id.ID(), id.Pub, req, ""), "admin_generation")
}
func TestHubAdminRotationCarriesExactCapabilities(t *testing.T) {
	st, id, token := adminTestStore(t)
	require.NoError(t, st.ClaimAdmin(id.ID(), id.Pub, token, time.Now()))
	next, _ := proto.NewIdentity()
	now := time.Now()
	rotation, err := proto.NewRotation(id, next, "", 1, now, time.Second)
	require.NoError(t, err)
	require.NoError(t, st.ObserveRotations([]proto.Rotation{rotation}, id.ID(), now, time.Second))
	require.NoError(t, st.ObserveRotations([]proto.Rotation{rotation}, next.ID(), now.Add(2*time.Second), time.Second))
	oldCaps, err := st.AdminCapabilities(id.ID())
	require.NoError(t, err)
	require.Empty(t, oldCaps)
	caps, err := st.AdminCapabilities(next.ID())
	require.NoError(t, err)
	require.ElementsMatch(t, proto.HubAdminBootstrapCapabilities, caps)
	requireAdminCode(t, st.RemoveAdmin(next.ID()), "last_admin")
}
func TestHubAdminSettingsLiveAndPersisted(t *testing.T) {
	st, _, _ := adminTestStore(t)
	h, err := New(st, Config{MaxConnections: 7, FlagSettings: []string{"max_connections"}})
	require.NoError(t, err)
	defer h.Close()
	value := int64(2)
	require.NoError(t, st.PatchSettings(map[string]*int64{"max_connections": &value}))
	h.RefreshPolicy()
	require.Equal(t, 2, h.config().MaxConnections)
	settings, err := h.Settings()
	require.NoError(t, err)
	require.Equal(t, "db", settings["max_connections"].Source)
	require.True(t, settings["max_connections"].FlagOverridden)
	require.EqualValues(t, 7, settings["max_connections"].Boot)
	require.Error(t, st.PatchSettings(map[string]*int64{"accept_remote_scripts": &value}))
	require.Error(t, st.PatchSettings(map[string]*int64{"stream_idle_seconds": &value}))
	require.NoError(t, st.PatchSettings(map[string]*int64{"max_connections": nil}))
	h.RefreshPolicy()
	require.Equal(t, 7, h.config().MaxConnections)
	require.NoError(t, st.PatchSettings(map[string]*int64{"max_connections": &value}))
	restarted, err := New(st, Config{MaxConnections: 8})
	require.NoError(t, err)
	defer restarted.Close()
	require.Equal(t, 2, restarted.config().MaxConnections)
}
func TestHubAdminLogsBoundedAndRedacted(t *testing.T) {
	st, _, token := adminTestStore(t)
	h, err := New(st, Config{})
	require.NoError(t, err)
	defer h.Close()
	h.log.Warn("failed "+token, "token", token)
	for range 300 {
		h.log.Info("safe\x1b[31m\nmessage")
	}
	result, err := h.adminLogTail(adminParams{MaxEntries: 200})
	require.NoError(t, err)
	raw, err := json.Marshal(result)
	require.NoError(t, err)
	require.NotContains(t, string(raw), token)
	require.NotContains(t, string(raw), "\\u001b")
	require.Less(t, len(raw), 256<<10)
}

func TestHubAdminRetiredOrConflictedKeysNeverCountAsManagers(t *testing.T) {
	for _, state := range []string{"accepted", "conflict", "revoked", "recovered"} {
		t.Run(state, func(t *testing.T) {
			st, id, token := adminTestStore(t)
			require.NoError(t, st.ClaimAdmin(id.ID(), id.Pub, token, time.Now()))
			stranded, _ := proto.NewIdentity()
			require.NoError(t, st.Admit(stranded.ID()))
			require.NoError(t, st.RecordSeen(stranded.ID(), stranded.Pub, "stranded", "", time.Now()))
			require.NoError(t, st.SetAdmin(stranded.ID(), id.ID(), []string{"hub.admins.manage"}))
			_, err := st.db.Exec(`INSERT INTO identity_rotations(old_instance,new_instance,statement,state,received_at,accept_after) VALUES(?,'','{}',?,?,?)`, stranded.ID(), state, ts(time.Now()), ts(time.Now()))
			require.NoError(t, err)
			// Admission alone cannot make a retired/conflicted key usable again.
			require.NoError(t, st.Admit(stranded.ID()))
			requireAdminCode(t, st.SetAdmin(stranded.ID(), id.ID(), []string{"hub.admins.manage"}), "instance")
			requireAdminCode(t, st.RemoveAdmin(id.ID()), "last_admin")
			requireAdminCode(t, st.Revoke(id.ID()), "last_admin")
			generation, err := st.AdminGeneration()
			require.NoError(t, err)
			request := &proto.HubAdminRequest{ID: proto.NewEnvelopeID(), Generation: generation, ExpiresAt: time.Now().Add(time.Minute)}
			requireAdminCode(t, st.AuthorizeAdminRequest(stranded.ID(), stranded.Pub, request, "hub.admins.manage"), "not_admitted")
		})
	}
}

func TestHubAdminLogPagesFitRPCByteLimit(t *testing.T) {
	st, _, _ := adminTestStore(t)
	h, err := New(st, Config{})
	require.NoError(t, err)
	defer h.Close()
	for range 250 {
		h.log.Info(strings.Repeat("<", 2048))
	}
	page, err := h.adminLogTail(adminParams{MaxEntries: 200})
	require.NoError(t, err)
	raw, err := json.Marshal(page)
	require.NoError(t, err)
	require.LessOrEqual(t, len(raw), proto.MaxAdminResult)
	next := page.(map[string]any)["next_cursor"].(string)
	require.NotEmpty(t, next)
	second, err := h.adminLogTail(adminParams{MaxEntries: 200, Cursor: next})
	require.NoError(t, err)
	require.NotEmpty(t, second.(map[string]any)["entries"])
}

func TestHubAdminBootstrapNeverImpliesElevatedCapabilities(t *testing.T) {
	oldCaps := proto.HubAdminCapabilities
	const elevated = "hub.test.elevated"
	proto.HubAdminCapabilities = append(append([]string{}, oldCaps...), elevated)
	proto.HubAdminElevatedCapabilities[elevated] = true
	t.Cleanup(func() { proto.HubAdminCapabilities = oldCaps; delete(proto.HubAdminElevatedCapabilities, elevated) })
	st, id, token := adminTestStore(t)
	require.NoError(t, st.ClaimAdmin(id.ID(), id.Pub, token, time.Now()))
	caps, err := st.AdminCapabilities(id.ID())
	require.NoError(t, err)
	require.ElementsMatch(t, proto.HubAdminBootstrapCapabilities, caps)
	require.NotContains(t, caps, elevated)
	next, _ := proto.NewIdentity()
	require.NoError(t, st.Admit(next.ID()))
	require.NoError(t, st.RecordSeen(next.ID(), next.Pub, "next", "", time.Now()))
	requireAdminCode(t, st.SetAdmin(next.ID(), id.ID(), []string{elevated}), "elevated_capability")
	// Only separately established authority permits delegation of an elevated cap.
	_, err = st.db.Exec(`INSERT INTO hub_admin_capabilities VALUES(?,?)`, id.ID(), elevated)
	require.NoError(t, err)
	require.NoError(t, st.SetAdmin(next.ID(), id.ID(), []string{elevated}))
	caps, err = st.AdminCapabilities(next.ID())
	require.NoError(t, err)
	require.Equal(t, []string{elevated}, caps)
}
func TestHubAdminClaimRefusesPreexistingAdminsEvenWithUnusedToken(t *testing.T) {
	st, id, token := adminTestStore(t)
	require.NoError(t, st.SetAdmin(id.ID(), id.ID(), []string{"hub.admins.manage"}))
	requireAdminCode(t, st.ClaimAdmin(id.ID(), id.Pub, token, time.Now()), "claim_used")
	var consumed bool
	require.NoError(t, st.db.QueryRow(`SELECT consumed FROM hub_admin_claim WHERE singleton=1`).Scan(&consumed))
	require.False(t, consumed, "a refused claim changes no token or grant state")
	caps, err := st.AdminCapabilities(id.ID())
	require.NoError(t, err)
	require.Equal(t, []string{"hub.admins.manage"}, caps)
}
