package hub

import (
	"bufio"
	"context"
	"encoding/json"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/noderun"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
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

func TestHubExecCrashFixture(t *testing.T) {
	path := os.Getenv("TCLAUDE_HUB_EXEC_CRASH_FIXTURE")
	if path == "" {
		return
	}
	_ = executeHubScript(context.Background(), path, "fixture", 30, os.Stdout, os.Stderr)
}
func TestHubExecGuardianReapsScriptAfterHubCrash(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("scripts refuse root")
	}
	st, _, _ := adminTestStore(t)
	path := filepath.Join(filepath.Dir(st.path), "crash.sh")
	require.NoError(t, os.WriteFile(path, []byte("echo $$; sleep 30"), 0600))
	exe, err := os.Executable()
	require.NoError(t, err)
	command := exec.Command(exe, "-test.run=^TestHubExecCrashFixture$")
	command.Env = append(os.Environ(), "TCLAUDE_HUB_EXEC_CRASH_FIXTURE="+path)
	pipe, err := command.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, command.Start())
	t.Cleanup(func() { _ = command.Process.Kill() })
	reader := bufio.NewReader(pipe)
	line, err := reader.ReadString('\n')
	require.NoError(t, err)
	pid, err := strconv.Atoi(strings.TrimSpace(line))
	require.NoError(t, err)
	require.NoError(t, command.Process.Kill())
	drained := make(chan error, 1)
	go func() { _, err := io.Copy(io.Discard, reader); drained <- err }()
	select {
	case err = <-drained:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("guardian retained script after hub crash")
	}
	require.Error(t, command.Wait())
	require.ErrorIs(t, syscall.Kill(pid, 0), syscall.ESRCH, "guardian must reap the script leader")
}
func TestHubExecFinishedAuditReconcilesIdempotently(t *testing.T) {
	st, id, _ := adminTestStore(t)
	jobID := strings.Repeat("a", 32)
	dir := filepath.Join(filepath.Dir(st.path), "hub-runs", jobID)
	require.NoError(t, os.MkdirAll(dir, 0700))
	raw, err := json.Marshal(noderun.Job{ID: jobID, Actor: id.ID(), State: "completed", ExitCode: 7, CreatedAt: time.Now(), FinishedAt: time.Now(), ScriptSHA256: "hash", ScriptBytes: 12})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "job.json"), raw, 0600))
	for range 2 {
		h, err := New(st, Config{})
		require.NoError(t, err)
		h.Close()
	}
	var count int
	require.NoError(t, st.db.QueryRow(`SELECT count(*) FROM hub_admin_audit WHERE request_id=? AND operation='exec' AND status=200`, jobID).Scan(&count))
	require.Equal(t, 1, count)
}

func TestHubExecGuardianDoesNotLeakControlPipes(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("scripts refuse root")
	}
	st, _, _ := adminTestStore(t)
	path := filepath.Join(filepath.Dir(st.path), "fds.sh")
	// Opening either private FD from a script would allow corrupting the result
	// or defeating lifetime supervision. The shell redirects errors away.
	require.NoError(t, os.WriteFile(path, []byte("if (printf forged >&4) 2>/dev/null; then exit 91; fi\nif (: <&3) 2>/dev/null; then exit 92; fi\nprintf isolated"), 0600))
	var out, stderr strings.Builder
	r := executeHubScript(context.Background(), path, "fixture", 2, &out, &stderr)
	require.Zero(t, r.ExitCode, r.Error)
	require.Equal(t, "isolated", out.String())
}

func TestHubExecOutcomeMarkerSurvivesAuditRetentionAndWriteFailure(t *testing.T) {
	st, id, _ := adminTestStore(t)
	jobID := strings.Repeat("b", 32)
	_, err := st.db.Exec(`CREATE TRIGGER fail_exec_audit BEFORE INSERT ON hub_admin_audit BEGIN SELECT RAISE(ABORT,'audit unavailable'); END`)
	require.NoError(t, err)
	require.Error(t, st.auditExecOutcome(id.ID(), jobID, `{"exit_code":7}`))
	var count int
	require.NoError(t, st.db.QueryRow(`SELECT count(*) FROM hub_exec_outcome_audits`).Scan(&count))
	require.Zero(t, count, "marker must not commit before its audit")
	_, err = st.db.Exec(`DROP TRIGGER fail_exec_audit`)
	require.NoError(t, err)
	require.NoError(t, st.auditExecOutcome(id.ID(), jobID, `{"exit_code":7}`))
	_, err = st.db.Exec(`DELETE FROM hub_admin_audit`)
	require.NoError(t, err)
	require.NoError(t, st.auditExecOutcome(id.ID(), jobID, `{"exit_code":7}`))
	require.NoError(t, st.db.QueryRow(`SELECT count(*) FROM hub_admin_audit`).Scan(&count))
	require.Zero(t, count, "retention must not resurrect old outcomes")
	require.NoError(t, st.db.QueryRow(`SELECT count(*) FROM hub_exec_outcome_audits`).Scan(&count))
	require.Equal(t, 1, count)
}
