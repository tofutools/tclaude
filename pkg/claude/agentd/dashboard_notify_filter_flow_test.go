package agentd_test

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/notify"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// noopNotifyCommand returns a do-nothing external command for the test
// notification path. It must execute the full production dispatch (so the
// suppression guards in OnStateTransition are exercised for real) WITHOUT
// touching anything under HOME.
//
// Specifically it must NOT be the Go toolchain. Every `go <subcmd>`
// invocation writes telemetry counter files into <UserConfigDir>/go/telemetry
// and can fork a *detached* telemetry child that flushes asynchronously. The
// flow harness points HOME at a per-test t.TempDir(), so that child races the
// temp dir's RemoveAll cleanup and trips "directory not empty" — a flaky test
// failure (macOS-prone, where UserConfigDir is $HOME/Library/Application
// Support). A bare no-op binary writes nothing under HOME, so there is no
// race. The command's exit status is irrelevant: OnStateTransition stamps the
// cooldown (what the tests assert on) regardless of whether dispatch's command
// succeeds, so even a not-found binary keeps the assertions valid.
func noopNotifyCommand() []string {
	if runtime.GOOS == "windows" {
		return []string{"cmd", "/c", "exit", "0"}
	}
	return []string{"true"}
}

// enableNotificationsForTest writes a tclaude config (into the flow
// world's temp HOME) with OS notifications enabled and the delivery
// routed to a harmless no-op command, so OnStateTransition runs the
// full production path without popping real desktop notifications on
// the machine running the tests.
func enableNotificationsForTest(t *testing.T) {
	t.Helper()
	cfg, err := config.Load()
	require.NoError(t, err, "load default config")
	cfg.Notifications.Enabled = true
	cfg.Notifications.NotificationCommand = noopNotifyCommand()
	require.NoError(t, config.Save(cfg), "save test config")
}

// notified reports whether a full OnStateTransition pass for the given
// session actually fired: the send path's last step records the
// cooldown stamp via db.SetNotifyTime, so its presence is the
// observable "a notification went out" marker — and its absence the
// "filtered out" one. Each assertion uses a fresh session ID so the
// 5s cooldown never bleeds between steps.
func notified(t *testing.T, sessionID, convID string) bool {
	t.Helper()
	return notifiedTransition(t, sessionID, convID, "working", "idle")
}

func notifiedTransition(t *testing.T, sessionID, convID, from, to string) bool {
	t.Helper()
	notify.OnStateTransition(sessionID, convID, from, to, "/tmp/x", "worker", "claude")
	_, found, err := db.GetNotifyTime(sessionID)
	require.NoError(t, err, "GetNotifyTime(%s)", sessionID)
	return found
}

// Scenario: a self-transition never notifies, even with notifications
// fully enabled and no cooldown in play (every step uses a fresh
// session ID, so the only thing standing between the call and a banner
// is the from==to guard).
//
// The production sequences this models:
//   - Claude Code's ~60s idle timer fires Notification(idle_prompt) on a
//     session that already went idle via Stop — without the guard the
//     wildcard {from:"*", to:"idle"} rule matched the idle→idle re-stamp
//     and the human got a duplicate "Idle" banner a minute after the
//     real one (the cooldown, default 5s, was long expired);
//   - a late SessionEnd hook landing after the reaper already stamped
//     the session exited (exited→exited), well past the cooldown window.
func TestNotificationFilters_SelfTransitionNeverNotifies(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFlow(t)
		enableNotificationsForTest(t)

		const conv = "self-aaaa-bbbb-cccc-dddd"
		f.HaveConvWithTitle(conv, "worker")

		require.True(t, notified(t, "sess-real-idle", conv),
			"control: working→idle fires, so a missing banner below is the guard, not a broken harness")

		assert.False(t, notifiedTransition(t, "sess-idle-restamp", conv, "idle", "idle"),
			"idle_prompt re-stamp of an already-idle session must not re-notify")
		assert.False(t, notifiedTransition(t, "sess-exit-resweep", conv, "exited", "exited"),
			"reaper re-observing an announced exit must not re-notify")
		assert.False(t, notifiedTransition(t, "sess-perm-repeat", conv, "awaiting_permission", "awaiting_permission"),
			"repeated permission prompt must not re-notify")
	})
}

// Scenario: the notification-filter ladder end to end — the dashboard
// bells' write endpoints, the snapshot fields they re-render from, and
// the actual notify.OnStateTransition suppression each state implies:
//
//   - baseline: enabled config, unmuted group → member notifies;
//   - PATCH /api/groups/{name} {notify_enabled:false} → member muted;
//   - POST /api/agents/{conv}/notify {mode:"on"} → overrides the mute;
//   - {mode:"off"} → silent even in an unmuted group;
//   - {mode:"inherit"} → back to following the group;
//   - POST /api/notifications {enabled:false} → everything silent,
//     including agents with a forced-on pref.
func TestNotificationFilters_GroupAndAgentBells(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		restoreURL := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
		t.Cleanup(restoreURL)

		f := newFlow(t)
		enableNotificationsForTest(t)

		const conv = "noti-aaaa-bbbb-cccc-dddd"
		f.HaveConvWithTitle(conv, "worker")
		f.HaveAliveSession(conv, "spwn-noti", "tmux-noti", "/tmp/x")
		f.HaveEnrolledAgent(conv)
		f.HaveGroup("alpha")
		f.HaveMember("alpha", conv)

		mux := agentd.BuildDashboardHandlerForTest()

		// Baseline: everything on → the member notifies and the snapshot
		// says so.
		pre := fetchDashSnapshot(t, mux)
		require.True(t, pre.NotificationsEnabled, "config enabled → snapshot.notifications_enabled")
		require.Len(t, pre.Groups, 1)
		assert.True(t, pre.Groups[0].NotifyEnabled, "fresh group notifies")
		require.Len(t, pre.Groups[0].Members, 1)
		assert.True(t, pre.Groups[0].Members[0].NotifyEffective, "member effective on")
		assert.Empty(t, pre.Groups[0].Members[0].Notify, "no per-agent override yet")
		assert.True(t, notified(t, "sess-base", conv), "baseline notification fires")

		// Mute the group via the header bell's PATCH.
		r := testharness.JSONRequest(t, http.MethodPatch, "/api/groups/alpha",
			map[string]any{"notify_enabled": false})
		rec := testharness.Serve(mux, r)
		require.Equal(t, http.StatusOK, rec.Code, "PATCH notify_enabled body=%s", rec.Body.String())

		muted := fetchDashSnapshot(t, mux)
		assert.False(t, muted.Groups[0].NotifyEnabled, "group muted in snapshot")
		assert.False(t, muted.Groups[0].Members[0].NotifyEffective, "member effective off via group")
		assert.False(t, notified(t, "sess-gmute", conv), "group mute suppresses")

		// Force the agent ON via the member bell — overrides the mute.
		r = testharness.JSONRequest(t, http.MethodPost, "/api/agents/"+conv+"/notify",
			map[string]any{"mode": "on"})
		rec = testharness.Serve(mux, r)
		require.Equal(t, http.StatusOK, rec.Code, "POST notify on body=%s", rec.Body.String())

		forced := fetchDashSnapshot(t, mux)
		assert.Equal(t, "on", forced.Groups[0].Members[0].Notify, "pref surfaces on the member row")
		assert.True(t, forced.Groups[0].Members[0].NotifyEffective, "forced-on beats the group mute")
		agentRow := findAgent(forced.Agents, conv)
		require.NotNil(t, agentRow, "agent row present")
		assert.Equal(t, "on", agentRow.Notify, "pref surfaces on the agents view too")
		assert.True(t, notified(t, "sess-forceon", conv), "forced-on notifies despite group mute")

		// Per-agent OFF: silent even after the group is unmuted again.
		r = testharness.JSONRequest(t, http.MethodPatch, "/api/groups/alpha",
			map[string]any{"notify_enabled": true})
		require.Equal(t, http.StatusOK, testharness.Serve(mux, r).Code)
		r = testharness.JSONRequest(t, http.MethodPost, "/api/agents/"+conv+"/notify",
			map[string]any{"mode": "off"})
		require.Equal(t, http.StatusOK, testharness.Serve(mux, r).Code)

		agentOff := fetchDashSnapshot(t, mux)
		assert.True(t, agentOff.Groups[0].NotifyEnabled, "group unmuted again")
		assert.Equal(t, "off", agentOff.Groups[0].Members[0].Notify)
		assert.False(t, agentOff.Groups[0].Members[0].NotifyEffective, "agent off wins over an unmuted group")
		assert.False(t, notified(t, "sess-aoff", conv), "per-agent off suppresses")

		// An unknown mode is a client error — 400, not a mislabelled 500.
		r = testharness.JSONRequest(t, http.MethodPost, "/api/agents/"+conv+"/notify",
			map[string]any{"mode": "bogus"})
		require.Equal(t, http.StatusBadRequest, testharness.Serve(mux, r).Code, "invalid mode rejected")

		// Inherit clears the override.
		r = testharness.JSONRequest(t, http.MethodPost, "/api/agents/"+conv+"/notify",
			map[string]any{"mode": "inherit"})
		require.Equal(t, http.StatusOK, testharness.Serve(mux, r).Code)
		inherit := fetchDashSnapshot(t, mux)
		assert.Empty(t, inherit.Groups[0].Members[0].Notify, "override dropped")
		assert.True(t, inherit.Groups[0].Members[0].NotifyEffective, "back to inheriting the unmuted group")
		assert.True(t, notified(t, "sess-inherit", conv), "inherit + unmuted group notifies")

		// The master switch: off silences everything — even a forced-on
		// agent (the global toggle sits ABOVE the per-agent prefs).
		r = testharness.JSONRequest(t, http.MethodPost, "/api/agents/"+conv+"/notify",
			map[string]any{"mode": "on"})
		require.Equal(t, http.StatusOK, testharness.Serve(mux, r).Code)
		r = testharness.JSONRequest(t, http.MethodPost, "/api/notifications",
			map[string]any{"enabled": false})
		rec = testharness.Serve(mux, r)
		require.Equal(t, http.StatusOK, rec.Code, "POST /api/notifications body=%s", rec.Body.String())

		globalOff := fetchDashSnapshot(t, mux)
		assert.False(t, globalOff.NotificationsEnabled, "snapshot reflects the master off")
		assert.False(t, notified(t, "sess-globaloff", conv), "master off silences a forced-on agent")

		// And back on via the same endpoint (the GET answers the bell's
		// current state).
		r = testharness.JSONRequest(t, http.MethodPost, "/api/notifications",
			map[string]any{"enabled": true})
		require.Equal(t, http.StatusOK, testharness.Serve(mux, r).Code)
		r = testharness.JSONRequest(t, http.MethodGet, "/api/notifications", nil)
		rec = testharness.Serve(mux, r)
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Contains(t, rec.Body.String(), `"enabled":true`)
		assert.True(t, notified(t, "sess-globalon", conv), "master back on notifies again")
	})
}

// Scenario: the per-TYPE notification selector (the top-bar bell popover,
// backed by GET/POST /api/notifications). Unchecking a type silences only
// that destination state's banner; the other types still notify. The gate
// is banner-only — it runs in OnStateTransition, well after the hook
// callback has already recorded the new status, so the event tclaude
// relies on is untouched (the brief's "still capture the events"). The
// human-message knob round-trips through the same endpoint.
func TestNotificationFilters_PerType(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		restoreURL := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
		t.Cleanup(restoreURL)

		f := newFlow(t)
		enableNotificationsForTest(t)

		const conv = "ptyp-aaaa-bbbb-cccc-dddd"
		f.HaveConvWithTitle(conv, "worker")
		f.HaveEnrolledAgent(conv)

		mux := agentd.BuildDashboardHandlerForTest()

		getState := func() map[string]any {
			r := testharness.JSONRequest(t, http.MethodGet, "/api/notifications", nil)
			rec := testharness.Serve(mux, r)
			require.Equal(t, http.StatusOK, rec.Code, "GET /api/notifications body=%s", rec.Body.String())
			var out map[string]any
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
			return out
		}
		postState := func(body map[string]any) map[string]any {
			r := testharness.JSONRequest(t, http.MethodPost, "/api/notifications", body)
			rec := testharness.Serve(mux, r)
			require.Equal(t, http.StatusOK, rec.Code, "POST /api/notifications body=%s", rec.Body.String())
			var out map[string]any
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
			return out
		}

		// GET surfaces the master switch, the per-type checklist (all five
		// defaults on) and the human-message intent (default on).
		state := getState()
		assert.Equal(t, true, state["enabled"], "master on after enableNotificationsForTest")
		types, _ := state["types"].(map[string]any)
		require.NotNil(t, types, "types map present")
		for _, ty := range []string{"idle", "awaiting_permission", "awaiting_input", "error", "exited"} {
			assert.Equal(t, true, types[ty], "default type %q on", ty)
		}
		assert.Equal(t, true, state["human_messages"], "human messages default on")

		// Baseline: an exit transition notifies.
		assert.True(t, notifiedTransition(t, "pt-exit-1", conv, "working", "exited"),
			"exit notifies before the type is unchecked")

		// Uncheck "exited": the response echoes it off, the others stay on.
		after := postState(map[string]any{"types": map[string]any{"exited": false}})
		afterTypes, _ := after["types"].(map[string]any)
		assert.Equal(t, false, afterTypes["exited"], "exited unchecked in the response")
		assert.Equal(t, true, afterTypes["idle"], "idle still checked")

		// Only the exit banner is silenced — a still-checked type notifies.
		assert.False(t, notifiedTransition(t, "pt-exit-2", conv, "working", "exited"),
			"exit no longer notifies once unchecked")
		assert.True(t, notifiedTransition(t, "pt-idle-1", conv, "working", "idle"),
			"a still-checked type keeps notifying")

		// Re-check it.
		postState(map[string]any{"types": map[string]any{"exited": true}})
		assert.True(t, notifiedTransition(t, "pt-exit-3", conv, "working", "exited"),
			"exit notifies again after re-checking")

		// An unknown type is a client error, not a silent no-op.
		bad := testharness.JSONRequest(t, http.MethodPost, "/api/notifications",
			map[string]any{"types": map[string]any{"bogus": false}})
		require.Equal(t, http.StatusBadRequest, testharness.Serve(mux, bad).Code, "unknown type rejected")

		// An empty body (no recognised field) is also a 400.
		empty := testharness.JSONRequest(t, http.MethodPost, "/api/notifications", map[string]any{})
		require.Equal(t, http.StatusBadRequest, testharness.Serve(mux, empty).Code, "no-field body rejected")

		// The human-message knob round-trips through the same endpoint.
		off := postState(map[string]any{"human_messages": false})
		assert.Equal(t, false, off["human_messages"], "human messages off echoed")
		assert.Equal(t, false, getState()["human_messages"], "and persisted")
	})
}

// A corrupt config.json must NOT be silently overwritten with defaults by
// a notification write — the popover refuses with a 409, leaving the
// Config tab to own recovery (mirrors handleDashboardSlopVolumesPost).
func TestNotificationsAPI_RefusesCorruptConfig(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		restoreURL := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
		t.Cleanup(restoreURL)

		newFlow(t) // points HOME at an isolated temp dir
		const corrupt = "{ not valid json"
		require.NoError(t, os.MkdirAll(filepath.Dir(config.ConfigPath()), 0o755))
		require.NoError(t, os.WriteFile(config.ConfigPath(), []byte(corrupt), 0o600))

		mux := agentd.BuildDashboardHandlerForTest()
		r := testharness.JSONRequest(t, http.MethodPost, "/api/notifications",
			map[string]any{"enabled": false})
		rec := testharness.Serve(mux, r)
		require.Equal(t, http.StatusConflict, rec.Code,
			"corrupt config → 409, not a silent default-overwrite; body=%s", rec.Body.String())

		// The corrupt file is left untouched — the write refused rather than
		// clobbering it with defaults-plus-the-toggle.
		data, err := os.ReadFile(config.ConfigPath())
		require.NoError(t, err)
		assert.Equal(t, corrupt, string(data), "corrupt config left as-is")
	})
}

// Scenario: the per-agent / per-group filters also gate the
// notify-human OS banner (PR #300). A muted sender's message still
// lands in the Messages tab — only the desktop notification is
// skipped — and unmuting restores it. The recorder seam sits BEHIND
// the AllowedForConv gate, so what it observes is exactly what the
// human's desktop would.
func TestNotificationFilters_GateNotifyHumanBanner(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFlow(t)

		const conv = "mute-1111-2222-3333-4444"
		f.HaveConvWithTitle(conv, "muted-po")
		f.HaveGroup("gamma")
		f.HaveMember("gamma", conv)
		require.NoError(t, db.GrantAgentPermission(conv, agentd.PermHumanNotify, "test"))

		var banners []string
		t.Cleanup(agentd.SetHumanMessageNotifierForTest(
			func(_, fromTitle, _, _, _ string) { banners = append(banners, fromTitle) }))
		t.Cleanup(agentd.WaitForBackgroundForTest)

		send := func(body string) {
			t.Helper()
			r := agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost,
				"/v1/notify-human", map[string]any{"body": body}), conv)
			rec := testharness.Serve(f.Mux, r)
			require.Equal(t, http.StatusOK, rec.Code, "notify-human body=%s", rec.Body.String())
			agentd.WaitForBackgroundForTest()
		}

		// Muted sender: persisted, not bannered.
		require.NoError(t, db.SetConvNotifyPref(conv, db.NotifyPrefOff))
		send("while muted")
		assert.Empty(t, banners, "a muted sender's ping must skip the OS banner")
		msgs, err := db.ListHumanMessages()
		require.NoError(t, err)
		require.Len(t, msgs, 1, "the message itself still lands in the Messages tab")

		// Unmuted: the banner fires again.
		require.NoError(t, db.SetConvNotifyPref(conv, db.NotifyPrefInherit))
		send("after unmute")
		assert.Equal(t, []string{"muted-po"}, banners, "an unmuted sender banners normally")
	})
}

// Scenario: the CLI verb's transport — PATCH /v1/groups/{name} with
// {notify_enabled} (what `tclaude agent groups set-notifications`
// sends) flips the switch for a human-authenticated caller, and the
// muted state surfaces in GET /v1/groups as notify_muted (what
// `groups ls` renders as 🔕).
func TestNotificationFilters_V1GroupPatch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFlow(t)

		f.HaveGroup("beta")
		r := agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPatch,
			"/v1/groups/beta", map[string]any{"notify_enabled": false}))
		rec := testharness.Serve(f.Mux, r)
		require.Equal(t, http.StatusOK, rec.Code, "PATCH /v1/groups/beta body=%s", rec.Body.String())
		assert.Contains(t, rec.Body.String(), `"notify_enabled":false`)

		g, err := db.GetAgentGroupByName("beta")
		require.NoError(t, err)
		require.NotNil(t, g)
		assert.False(t, g.NotifyEnabled, "mute persisted via /v1")

		r = agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/groups", nil))
		rec = testharness.Serve(f.Mux, r)
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Contains(t, rec.Body.String(), `"notify_muted":true`, "groups ls surfaces the mute")
	})
}
