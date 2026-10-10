package hub

import (
	"context"
	"encoding/json"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHubExecBootstrapDelegationAndLocalSwitch(t *testing.T) {
	st, id, token := adminTestStore(t)
	require.NoError(t, st.ClaimAdmin(id.ID(), id.Pub, token, time.Now()))
	require.False(t, st.hasAdminCapability(id.ID(), "hub.exec"))
	other, err := proto.NewIdentity()
	require.NoError(t, err)
	require.NoError(t, st.Admit(other.ID()))
	require.NoError(t, st.RecordSeen(other.ID(), other.Pub, "other", "test", time.Now()))
	requireAdminCode(t, st.SetAdmin(other.ID(), id.ID(), []string{"hub.exec"}), "elevated_capability")
	require.NoError(t, st.GrantExec(id.ID()))
	require.NoError(t, st.SetAdmin(other.ID(), id.ID(), []string{"hub.exec"}))
	require.True(t, st.hasAdminCapability(other.ID(), "hub.exec"))
	enabled, source, err := st.scriptSwitch(nil)
	require.NoError(t, err)
	require.False(t, enabled)
	require.Equal(t, "config", source)
	path := filepath.Join(filepath.Dir(st.path), "hub-config.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"accept_remote_scripts":true}`), 0600))
	enabled, _, err = st.scriptSwitch(nil)
	require.NoError(t, err)
	require.True(t, enabled)
	off := false
	enabled, source, err = st.scriptSwitch(&off)
	require.NoError(t, err)
	require.False(t, enabled)
	require.Equal(t, "flag", source)
	require.NoError(t, os.Chmod(path, 0644))
	_, _, err = st.scriptSwitch(nil)
	require.Error(t, err)
	require.NoError(t, os.Remove(path))
	require.NoError(t, os.Symlink(st.ClaimPath(), path))
	_, _, err = st.scriptSwitch(nil)
	require.Error(t, err)
}
func TestHubExecAuditRedactionAndPaging(t *testing.T) {
	st, id, token := adminTestStore(t)
	require.NoError(t, st.ClaimAdmin(id.ID(), id.Pub, token, time.Now()))
	h, err := New(st, Config{})
	require.NoError(t, err)
	defer h.Close()
	raw, _ := json.Marshal(map[string]any{"script": "hub-host-secret", "script_sha256": "hash", "script_bytes": 15})
	require.NoError(t, st.AuditAdmin(id.ID(), "fixture", "exec", 202, string(raw)))
	value, err := h.auditRows(&conn{id: id.ID()}, adminParams{})
	require.NoError(t, err)
	encoded, _ := json.Marshal(value)
	require.NotContains(t, string(encoded), "hub-host-secret")
	require.Contains(t, string(encoded), `"redacted":true`)
	require.NoError(t, st.GrantExec(id.ID()))
	value, err = h.auditRows(&conn{id: id.ID()}, adminParams{})
	require.NoError(t, err)
	encoded, _ = json.Marshal(value)
	require.Contains(t, string(encoded), "hub-host-secret")
	_, err = h.auditRows(&conn{id: id.ID()}, adminParams{MaxEntries: 201})
	requireAdminCode(t, err, "max_entries")
}
func TestHubExecRunnerTimeoutAndOutput(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("actual hub scripts require a non-root service user")
	}
	st, _, _ := adminTestStore(t)
	script := filepath.Join(filepath.Dir(st.path), "fixture.sh")
	require.NoError(t, os.WriteFile(script, []byte("printf live; printf warning >&2; exit 7"), 0600))
	var out, stderr strings.Builder
	r := executeHubScript(context.Background(), script, "fixture", 2, &out, &stderr)
	require.Equal(t, 7, r.ExitCode)
	require.Equal(t, "live", out.String())
	require.Equal(t, "warning", stderr.String())
	require.NoError(t, os.WriteFile(script, []byte("sleep 30 & wait"), 0600))
	r = executeHubScript(context.Background(), script, "fixture", 1, &out, &stderr)
	require.True(t, r.TimedOut)
	require.NotZero(t, r.ExitCode)
}
