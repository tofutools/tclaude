package agentd_test

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/notify"
	"github.com/tofutools/tclaude/pkg/testharness"
)

type browserNotifyPayload struct {
	Cursor        int64                    `json:"cursor"`
	Notifications []db.BrowserNotification `json:"notifications"`
}

// pinBrowserNotifyOrigin gives the dashboard mux a loopback base URL so
// the flow harness stamps a matching Origin on each request — without it
// every dashboard call is a 403 on the CSRF pin, not on anything this
// file is testing.
func pinBrowserNotifyOrigin(t *testing.T) {
	t.Helper()
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
}

func fetchBrowserNotifications(t *testing.T, mux http.Handler, query string) browserNotifyPayload {
	t.Helper()
	rec := testharness.Serve(mux, testharness.JSONRequest(t, http.MethodGet, "/api/browser-notifications"+query, nil))
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var out browserNotifyPayload
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	return out
}

// Scenario: the whole browser-delivery path as a dashboard tab sees it —
// config says "deliver via browser", a real state transition runs the
// production notify path, and the dashboard's poll endpoint hands the
// banner over exactly once per cursor.
//
// This is the remote/sandboxed case the feature exists for: no desktop
// notifier is involved anywhere below.
func TestBrowserNotifications_TransitionReachesTheDashboardPoll(t *testing.T) {
	f := newFlow(t)
	pinBrowserNotifyOrigin(t)

	cfg, err := config.Load()
	require.NoError(t, err)
	cfg.Notifications.Enabled = true
	cfg.Notifications.Delivery = config.NotifyDeliveryBrowser
	require.NoError(t, config.Save(cfg))

	const conv = "brow-aaaa-bbbb-cccc-dddd"
	f.HaveConvWithTitle(conv, "worker")

	mux := agentd.BuildDashboardHandlerForTest()

	// A tab opening before anything happened adopts the head and is shown
	// no backlog.
	start := fetchBrowserNotifications(t, mux, "")
	assert.Empty(t, start.Notifications, "a fresh tab never replays a backlog")

	notify.OnStateTransition("sess-brow-1", conv, "working", "awaiting_permission", f.TestCwd("proj"), "worker", "claude")

	got := fetchBrowserNotifications(t, mux, "?since=0")
	require.Len(t, got.Notifications, 1)
	assert.Equal(t, "Claude: Awaiting permission", got.Notifications[0].Title)
	assert.Equal(t, "sess-brow-1", got.Notifications[0].SessionID)
	assert.Contains(t, got.Notifications[0].Body, "proj")
	assert.Positive(t, got.Cursor)

	// Polling again from the returned cursor is empty — one banner, once.
	assert.Empty(t, fetchBrowserNotifications(t, mux, "?since="+strconv.FormatInt(got.Cursor, 10)).Notifications)
}

// Scenario: delivery is left at the default, so the browser queue must
// stay empty even though notifications are enabled and firing. The knob
// is what routes them, not the master switch.
func TestBrowserNotifications_DefaultDeliveryQueuesNothing(t *testing.T) {
	f := newFlow(t)
	pinBrowserNotifyOrigin(t)
	enableNotificationsForTest(t) // enabled, delivery unset, no-op OS command

	const conv = "brnd-aaaa-bbbb-cccc-dddd"
	f.HaveConvWithTitle(conv, "worker")

	notify.OnStateTransition("sess-brnd-1", conv, "working", "idle", f.TestCwd("proj"), "worker", "claude")

	mux := agentd.BuildDashboardHandlerForTest()
	assert.Empty(t, fetchBrowserNotifications(t, mux, "?since=0").Notifications)
}

// Scenario: a per-agent mute silences the browser channel too. Delivery
// picks WHERE a notification goes, never WHETHER — every gate above
// dispatch still runs.
func TestBrowserNotifications_MutedAgentIsSilentInTheBrowserToo(t *testing.T) {
	f := newFlow(t)
	pinBrowserNotifyOrigin(t)

	cfg, err := config.Load()
	require.NoError(t, err)
	cfg.Notifications.Enabled = true
	cfg.Notifications.Delivery = config.NotifyDeliveryBrowser
	require.NoError(t, config.Save(cfg))

	const conv = "brmu-aaaa-bbbb-cccc-dddd"
	f.HaveConvWithTitle(conv, "worker")
	f.HaveEnrolledAgent(conv)
	require.NoError(t, db.SetConvNotifyPref(conv, db.NotifyPrefOff))

	notify.OnStateTransition("sess-brmu-1", conv, "working", "idle", f.TestCwd("proj"), "worker", "claude")

	mux := agentd.BuildDashboardHandlerForTest()
	assert.Empty(t, fetchBrowserNotifications(t, mux, "?since=0").Notifications)
}

// Scenario: a malformed cursor is a clean 400, not a silent reset that
// would replay the whole queue.
func TestBrowserNotifications_RejectsABadCursor(t *testing.T) {
	newFlow(t)
	pinBrowserNotifyOrigin(t)
	mux := agentd.BuildDashboardHandlerForTest()
	rec := testharness.Serve(mux, testharness.JSONRequest(t, http.MethodGet, "/api/browser-notifications?since=abc", nil))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}
