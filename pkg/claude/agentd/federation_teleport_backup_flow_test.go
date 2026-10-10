package agentd_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/bundletransfer"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/testharness"
	"github.com/tofutools/tclaude/pkg/testutil"
)

func fedBackupPolicy(t *testing.T, recovery string) {
	t.Helper()
	_, err := config.Update(func(c *config.Config, e error) error {
		if e != nil {
			return e
		}
		if c.Federation.Teleport == nil {
			c.Federation.Teleport = &config.FederationTeleportConfig{}
		}
		c.Federation.Teleport.Backup = config.TeleportBackupConfig{RenewSeconds: 1, LeaseSeconds: 2, GraceSeconds: 1, DormantMax: 1, Recovery: recovery}
		return nil
	})
	require.NoError(t, err)
}
func fedPausedBackup(t *testing.T, fh *fedHarness) (string, bundletransfer.Descriptor) {
	t.Helper()
	aid := fedTeleportSource(t, fh)
	require.NoError(t, db.PutFederationCatalog(fh.peer.id.ID(), string(mustJSON(t, proto.CatalogPayload{AgentMoves: true, AgentTeleports: 1, TeleportBackups: true, Groups: []proto.CatalogGroup{}})), time.Now()))
	require.NoError(t, db.GrantAgentPermissionWithScope(moveSourceConv, agentd.PermSelfTeleport, `{"peer":["`+fh.peer.id.ID()+`"]}`, "test"))
	rec := fedSelfTeleport(t, fh, map[string]any{"peer": "bob", "group": "receiver", "keep_paused_backup": true})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var out struct {
		Offer struct {
			D bundletransfer.Descriptor `json:"offer"`
		} `json:"offer"`
	}
	testharness.DecodeJSON(t, rec, &out)
	d := out.Offer.D
	require.True(t, d.Teleport.KeepPausedBackup)
	require.Greater(t, d.Teleport.BackupRenewSeconds, 0)
	fedMoveConfirm(t, fh, d, d.SHA256)
	fedEventually(t, "source paused", func() bool {
		m, _ := db.GetFederationAgentMove("out", fh.peer.id.ID(), d.ID)
		return m != nil && m.State == "paused"
	})
	a, err := db.GetAgent(aid)
	require.NoError(t, err)
	require.True(t, a.Active(), "pause must retain identity and grants")
	return aid, d
}
func fedLeaseControl(t *testing.T, fh *fedHarness, d bundletransfer.Descriptor, op string, seq int64, extra map[string]any) {
	t.Helper()
	body := map[string]any{"op": op, "offer": d.ID, "agent": "agt_bobremote0000000000000000", "epoch": 1, "sequence": seq}
	for k, v := range extra {
		body[k] = v
	}
	env := fh.peer.envelope(proto.KindTeleportLease, proto.Endpoint{}, body)
	env.From.Agent = ""
	fh.peer.send(env)
}
func TestFederation_TeleportBackupPausedVisibilityAndResumeGuard(t *testing.T) {
	fh := newFedHarness(t)
	aid, d := fedPausedBackup(t, fh)
	rec := fedHuman(t, fh.f, http.MethodPost, "/v1/agent/"+moveSourceConv+"/resume", nil)
	require.Contains(t, rec.Body.String(), "paused teleport backup")
	rec = fedHuman(t, fh.f, http.MethodGet, "/v1/peers", nil)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "paused (teleported to")
	early := fedHuman(t, fh.f, http.MethodPost, "/v1/teleport/recover", map[string]any{"agent": aid})
	require.Equal(t, 409, early.Code, early.Body.String())
	// A duplicate cannot consume another dormant slot or export a second backup.
	rec = fedSelfTeleport(t, fh, map[string]any{"peer": "bob", "group": "receiver", "keep_paused_backup": true})
	require.Equal(t, 409, rec.Code, rec.Body.String())
	l, err := db.GetFederationTeleportLease("out", fh.peer.id.ID(), d.ID)
	require.NoError(t, err)
	require.Equal(t, "paused", l.State)
}
func TestFederation_TeleportBackupManualRenewalExpiryAndEpoch(t *testing.T) {
	fh := newFedHarness(t)
	fedBackupPolicy(t, "manual")
	aid, d := fedPausedBackup(t, fh)
	fedLeaseControl(t, fh, d, "renew", 1, nil)
	fedEventually(t, "renew observed", func() bool {
		l, _ := db.GetFederationTeleportLease("out", fh.peer.id.ID(), d.ID)
		return l != nil && l.Sequence == 1
	})
	fedLeaseControl(t, fh, d, "renew", 1, nil) // duplicate is inert
	fedEventuallyWithin(t, "manual recovery needed", 6*time.Second, func() bool {
		l, _ := db.GetFederationTeleportLease("out", fh.peer.id.ID(), d.ID)
		return l != nil && l.State == "recovery_needed"
	})
	a, err := db.GetAgent(aid)
	require.NoError(t, err)
	require.True(t, a.Active())
	rec := fedHuman(t, fh.f, http.MethodPost, "/v1/teleport/recover", map[string]any{"agent": aid})
	require.Equal(t, 202, rec.Code, rec.Body.String())
	fedEventually(t, "recovered once", func() bool {
		l, _ := db.GetFederationTeleportLease("out", fh.peer.id.ID(), d.ID)
		return l != nil && l.State == "recovered" && l.Epoch == 2
	})
	msgs, err := db.ListAgentMessagesForConv(moveSourceConv, 100)
	require.NoError(t, err)
	var warnings int
	for _, m := range msgs {
		if strings.Contains(m.Body, "remote copy may still be alive") {
			warnings++
		}
	}
	require.Equal(t, 1, warnings)
	fedLeaseControl(t, fh, d, "renew", 2, nil)
	fedEventually(t, "stale epoch superseded", func() bool {
		for _, e := range fh.peer.envelopes(proto.KindTeleportLease) {
			var f struct {
				Op    string `json:"op"`
				Epoch int64  `json:"epoch"`
			}
			if e.DecodePayload(&f) == nil && f.Op == "superseded" && f.Epoch == 2 {
				return true
			}
		}
		return false
	})
	rec = fedHuman(t, fh.f, http.MethodPost, "/v1/teleport/recover", map[string]any{"agent": aid})
	require.Equal(t, 409, rec.Code, rec.Body.String())
}
func TestFederation_TeleportBackupReportReturnsOriginalIdentity(t *testing.T) {
	fh := newFedHarness(t)
	aid, d := fedPausedBackup(t, fh)
	report := proto.NewEnvelopeID()
	fedLeaseControl(t, fh, d, "return", 0, map[string]any{"return_id": report, "findings": "Fixed the index; commit abc123."})
	fedEventually(t, "report resumed original", func() bool {
		l, _ := db.GetFederationTeleportLease("out", fh.peer.id.ID(), d.ID)
		return l != nil && l.State == "recovered" && l.ReturnID == report
	})
	a, err := db.GetAgent(aid)
	require.NoError(t, err)
	require.True(t, a.Active())
	require.Equal(t, moveSourceConv, a.CurrentConvID)
	rec := testharness.Serve(fh.f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/inbox", nil), moveSourceConv))
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var inbox []struct {
		ID int64 `json:"id"`
	}
	testharness.DecodeJSON(t, rec, &inbox)
	require.Len(t, inbox, 1)
	rec = testharness.Serve(fh.f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodGet, fmt.Sprintf("/v1/messages/%d", inbox[0].ID), nil), moveSourceConv))
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "Fixed the index")
	require.Contains(t, rec.Body.String(), "Arrival briefing — teleport home (paused backup return)")
	require.Contains(t, rec.Body.String(), "restored home identity and inbox")
	require.Contains(t, rec.Body.String(), aid)
	fedLeaseControl(t, fh, d, "return", 0, map[string]any{"return_id": report, "findings": "Fixed the index; commit abc123."})
	fedEventually(t, "report retry answered", func() bool { return len(fh.peer.envelopes(proto.KindTeleportLease)) >= 2 })
}

func TestFederation_TeleportBackupOriginRestartWaitsFullOnlineWindow(t *testing.T) {
	fh := newFedHarness(t)
	fedBackupPolicy(t, "auto")
	_, d := fedPausedBackup(t, fh)
	// A later default change must not invalidate timings negotiated by this lease.
	_, err := config.Update(func(c *config.Config, e error) error {
		if e != nil {
			return e
		}
		c.Federation.Teleport.Backup = config.TeleportBackupConfig{}
		return nil
	})
	require.NoError(t, err)
	rec := fedHuman(t, fh.f, http.MethodPost, "/v1/federation/config", map[string]any{"enabled": false})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	time.Sleep(3500 * time.Millisecond) // offline time exceeds the configured lease + grace
	l, err := db.GetFederationTeleportLease("out", fh.peer.id.ID(), d.ID)
	require.NoError(t, err)
	require.Equal(t, "paused", l.State)
	rec = fedHuman(t, fh.f, http.MethodPost, "/v1/federation/config", map[string]any{"enabled": true, "hub_url": fh.url, "name": "alice-box"})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	time.Sleep(1500 * time.Millisecond)
	l, err = db.GetFederationTeleportLease("out", fh.peer.id.ID(), d.ID)
	require.NoError(t, err)
	require.Equal(t, "paused", l.State, "offline wall time must not cause immediate recovery")
	fedEventuallyWithin(t, "auto recovery after online observation", 6*time.Second, func() bool {
		l, _ := db.GetFederationTeleportLease("out", fh.peer.id.ID(), d.ID)
		return l != nil && l.State == "recovered" && l.Epoch == 2
	})
}

func fedLandedBackup(t *testing.T, fh *fedHarness) (bundletransfer.Descriptor, *db.Agent) {
	t.Helper()
	fh.f.HaveGroup("receiver")
	fedReceiveAgents(t, fh, "receiver")
	d := fedIncomingTeleport(t, fh, "local", func(in *bundletransfer.TeleportIntent) {
		in.Clone = false
		in.KeepPausedBackup = true
		in.BackupRenewSeconds = 1
	})
	require.Equal(t, proto.AckAccepted, fedAckFor(t, fh.peer, d.ID).Status)
	rec := fedHuman(t, fh.f, http.MethodPost, "/v1/federation/bundle-offers/"+d.ID+"/import", map[string]any{"cwd": testutil.CanonicalTempDir(t), "apply": true})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	fedEventually(t, "incoming lease and confirmation", func() bool {
		l, _ := db.GetFederationTeleportLease("in", fh.peer.id.ID(), d.ID)
		m, _ := db.GetFederationAgentMove("in", fh.peer.id.ID(), d.ID)
		return l != nil && l.State == "active" && m != nil && m.State == "running"
	})
	l, err := db.GetFederationTeleportLease("in", fh.peer.id.ID(), d.ID)
	require.NoError(t, err)
	a, err := db.GetAgent(l.TargetAgent)
	require.NoError(t, err)
	require.NotNil(t, a)
	require.NoError(t, db.GrantAgentPermissionWithScope(a.CurrentConvID, agentd.PermSelfTeleport, `{"peer":["`+fh.peer.id.ID()+`"]}`, "test"))
	return d, a
}
func TestFederation_TeleportBackupRemoteKeepsWorkingAndSupersededStops(t *testing.T) {
	fh := newFedHarness(t)
	fedBackupPolicy(t, "auto")
	d, a := fedLandedBackup(t, fh)
	fedEventually(t, "remote lease renewal", func() bool { return len(fh.peer.envelopes(proto.KindTeleportLease)) > 0 })
	before, err := db.GetFederationTeleportLease("in", fh.peer.id.ID(), d.ID)
	require.NoError(t, err)
	rec := fedHuman(t, fh.f, http.MethodPost, "/v1/federation/config", map[string]any{"enabled": false})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	rec = fedHuman(t, fh.f, http.MethodPost, "/v1/federation/config", map[string]any{"enabled": true, "hub_url": fh.url, "name": "alice-box"})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	fedEventually(t, "renewal resumes after daemon runtime restart", func() bool {
		l, _ := db.GetFederationTeleportLease("in", fh.peer.id.ID(), d.ID)
		return l != nil && l.Sequence > before.Sequence
	})
	// No origin ever acknowledges. Missing renewal acknowledgements must not
	// stop the landed agent, even after a complete lease + grace.
	time.Sleep(3500 * time.Millisecond)
	rec = fedHuman(t, fh.f, http.MethodGet, "/v1/peers", nil)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var peers []struct {
		Agent  string `json:"agent_id"`
		Online bool   `json:"online"`
	}
	testharness.DecodeJSON(t, rec, &peers)
	var online bool
	for _, p := range peers {
		if p.Agent == a.AgentID {
			online = p.Online
		}
	}
	require.True(t, online, "origin outage must not self-fence remote")
	env := fh.peer.envelope(proto.KindTeleportLease, proto.Endpoint{}, map[string]any{"op": "superseded", "offer": d.ID, "agent": a.AgentID, "epoch": 2, "policy": "stop"})
	env.From.Agent = ""
	fh.peer.send(env)
	fedEventually(t, "stale remote stopped", func() bool {
		l, _ := db.GetFederationTeleportLease("in", fh.peer.id.ID(), d.ID)
		return l != nil && l.State == "superseded" && l.Epoch == 2
	})
	rec = fedHuman(t, fh.f, http.MethodPost, "/v1/agent/"+a.CurrentConvID+"/resume", nil)
	require.Contains(t, rec.Body.String(), "lease has ended")
}
func TestFederation_TeleportBackupHomeUsesDurableReportReturn(t *testing.T) {
	fh := newFedHarness(t)
	fedBackupPolicy(t, "auto")
	d, a := fedLandedBackup(t, fh)
	require.NoError(t, db.SetAgentPermissionOverride(a.CurrentConvID, agentd.PermSelfTeleport, db.PermEffectDeny, "test"))
	req := func(input map[string]any) *httptest.ResponseRecorder {
		return testharness.Serve(fh.f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/whoami/teleport", input), a.CurrentConvID))
	}
	rec := req(map[string]any{"peer": "bob", "group": "receiver"})
	require.Equal(t, 409, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "leased")
	rec = req(map[string]any{"home": true, "note": "Investigated and repaired the service."})
	require.Equal(t, 202, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "return_id")
	fedEventually(t, "stopped return with findings sent", func() bool {
		for _, env := range fh.peer.envelopes(proto.KindTeleportLease) {
			var f struct{ Op, Findings, Offer string }
			if env.DecodePayload(&f) == nil && f.Op == "return" && f.Offer == d.ID && strings.Contains(f.Findings, "repaired the service") {
				return true
			}
		}
		return false
	})
	l, err := db.GetFederationTeleportLease("in", fh.peer.id.ID(), d.ID)
	require.NoError(t, err)
	require.Equal(t, "stopped", l.State)
	// Retry the same report after a lost acknowledgement; a different report
	// must not replace already committed findings.
	report := func(body string) *httptest.ResponseRecorder {
		return testharness.Serve(fh.f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/whoami/teleport/report", map[string]any{"findings": body}), a.CurrentConvID))
	}
	require.Equal(t, 202, report(l.Findings).Code)
	require.Equal(t, 409, report("different").Code)
}

func TestFederation_TeleportBackupSupersededCloneStaysOnline(t *testing.T) {
	fh := newFedHarness(t)
	d, a := fedLandedBackup(t, fh)
	env := fh.peer.envelope(proto.KindTeleportLease, proto.Endpoint{}, map[string]any{"op": "superseded", "offer": d.ID, "agent": a.AgentID, "epoch": 2, "policy": "clone"})
	env.From.Agent = ""
	fh.peer.send(env)
	fedEventually(t, "retained clone", func() bool {
		l, _ := db.GetFederationTeleportLease("in", fh.peer.id.ID(), d.ID)
		return l != nil && l.State == "clone" && l.Epoch == 2
	})
	rec := fedHuman(t, fh.f, http.MethodGet, "/v1/peers", nil)
	var peers []struct {
		Agent  string `json:"agent_id"`
		Online bool   `json:"online"`
	}
	testharness.DecodeJSON(t, rec, &peers)
	var online bool
	for _, p := range peers {
		if p.Agent == a.AgentID {
			online = p.Online
		}
	}
	require.True(t, online)
	msgs, err := db.ListAgentMessagesForConv(a.CurrentConvID, 100)
	require.NoError(t, err)
	var warned bool
	for _, m := range msgs {
		if strings.Contains(m.Body, "independent clone") {
			warned = true
		}
	}
	require.True(t, warned)
}

func TestFederation_TeleportBackupLateReportAfterRecoveryDeliveredOnce(t *testing.T) {
	fh := newFedHarness(t)
	fedBackupPolicy(t, "auto")
	_, d := fedPausedBackup(t, fh)
	fedEventuallyWithin(t, "lease recovery before partitioned return", 6*time.Second, func() bool {
		l, _ := db.GetFederationTeleportLease("out", fh.peer.id.ID(), d.ID)
		return l != nil && l.State == "recovered"
	})
	id := proto.NewEnvelopeID()
	for i := 0; i < 2; i++ {
		fedLeaseControl(t, fh, d, "return", 0, map[string]any{"return_id": id, "findings": "Late findings: deployed build 123."})
	}
	fedEventually(t, "late findings recorded", func() bool {
		l, _ := db.GetFederationTeleportLease("out", fh.peer.id.ID(), d.ID)
		return l != nil && l.ReturnID == id
	})
	msgs, err := db.ListAgentMessagesForConv(moveSourceConv, 100)
	require.NoError(t, err)
	var reports int
	for _, m := range msgs {
		if strings.Contains(m.Body, "Late findings: deployed build 123.") {
			reports++
		}
	}
	require.Equal(t, 1, reports)
	l, err := db.GetFederationTeleportLease("out", fh.peer.id.ID(), d.ID)
	require.NoError(t, err)
	require.Equal(t, "recovered", l.State)
	require.Equal(t, int64(2), l.Epoch)
}
func TestFederation_TeleportBackupReturnWaitsForPersistedProcessAfterPaneLoss(t *testing.T) {
	fh := newFedHarness(t)
	d, a := fedLandedBackup(t, fh)
	child := exec.Command("sleep", "60")
	require.NoError(t, child.Start())
	t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })
	probe := exec.Command("ps", "-p", strconv.Itoa(child.Process.Pid), "-o", "lstart=")
	probe.Env = append(os.Environ(), "TZ=UTC", "LC_ALL=C")
	started, err := probe.Output()
	require.NoError(t, err)
	// Model restart after tmux lost the pane, with the old harness PID still
	// alive. The shutdown evidence was committed before the previous teardown.
	fh.f.Stop(a.CurrentConvID, true)
	// Flow deliberately neutralizes all host process probes. Restore a real
	// probe only for this test-owned child, keeping every other PID inert.
	restore := agentd.SetSoftExitEscalationProcessForTest(func(pid int) bool { return pid == child.Process.Pid && syscall.Kill(pid, 0) == nil }, nil)
	t.Cleanup(func() { agentd.ResetFederationForTest(); restore() })
	l, err := db.GetFederationTeleportLease("in", fh.peer.id.ID(), d.ID)
	require.NoError(t, err)
	l.State = "returning"
	l.ReturnID = proto.NewEnvelopeID()
	l.Findings = "Stopped after repair"
	l.ShutdownPID = child.Process.Pid
	l.ShutdownProcessStart = strings.TrimSpace(string(started))
	l.ShutdownConv = a.CurrentConvID
	won, err := db.TransitionFederationTeleportLease(*l, "")
	require.NoError(t, err)
	require.True(t, won)
	time.Sleep(2200 * time.Millisecond)
	l, err = db.GetFederationTeleportLease("in", fh.peer.id.ID(), d.ID)
	require.NoError(t, err)
	require.Equal(t, "returning", l.State, "missing pane is not proof of process exit")
	for _, env := range fh.peer.envelopes(proto.KindTeleportLease) {
		var f struct{ Op string }
		require.NoError(t, env.DecodePayload(&f))
		require.NotEqual(t, "return", f.Op)
	}
	require.NoError(t, child.Process.Kill())
	_ = child.Wait()
	fedEventually(t, "return after persisted PID really exits", func() bool {
		l, _ := db.GetFederationTeleportLease("in", fh.peer.id.ID(), d.ID)
		return l != nil && l.State == "stopped"
	})
}

func TestFederation_TeleportBackupUsesOriginCadenceWithDifferentReceiverDefaults(t *testing.T) {
	fh := newFedHarness(t)
	d, _ := fedLandedBackup(t, fh)
	// Local default is 30s; this origin requested 1s in the signed offer.
	fedEventuallyWithin(t, "origin cadence respected", 4*time.Second, func() bool {
		l, _ := db.GetFederationTeleportLease("in", fh.peer.id.ID(), d.ID)
		return l != nil && l.RenewSeconds == 1 && l.Sequence >= 2
	})
}

func TestFederation_TeleportBackupHomeWithoutNoteIncludesTranscriptTail(t *testing.T) {
	fh := newFedHarness(t)
	d, a := fedLandedBackup(t, fh)
	rec := testharness.Serve(fh.f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/whoami/teleport", map[string]any{"home": true}), a.CurrentConvID))
	require.Equal(t, 202, rec.Code, rec.Body.String())
	fedEventually(t, "default home findings", func() bool {
		l, _ := db.GetFederationTeleportLease("in", fh.peer.id.ID(), d.ID)
		return l != nil && l.State == "stopped" && strings.Contains(l.Findings, "Continue the test task.")
	})
	l, err := db.GetFederationTeleportLease("in", fh.peer.id.ID(), d.ID)
	require.NoError(t, err)
	require.Contains(t, l.Findings, "Transcript tail")
}
