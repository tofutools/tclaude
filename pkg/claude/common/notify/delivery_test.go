package notify

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// setupDelivery gives the test its own HOME (so both the config file and
// the SQLite store are private to it) and writes a notifications block
// with the requested delivery channel.
//
// The OS channel is observed through notification_command: it is the one
// OS path that is deterministic across platforms, and dispatch treats it
// exactly like the platform notifier it replaces. The command touches a
// marker file, so "did the OS channel fire?" is a file-existence check.
func setupDelivery(t *testing.T, delivery string) (marker string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	db.ResetForTest()

	marker = filepath.Join(home, "os-channel-fired")
	require.NoError(t, config.Save(&config.Config{
		Notifications: &config.NotificationConfig{
			Enabled:             true,
			Delivery:            delivery,
			NotificationCommand: []string{"sh", "-c", "touch " + marker},
		},
	}))
	return marker
}

func osChannelFired(t *testing.T, marker string) bool {
	t.Helper()
	_, err := os.Stat(marker)
	return err == nil
}

func queuedTitles(t *testing.T) []string {
	t.Helper()
	items, _, err := db.ListBrowserNotificationsSince(0)
	require.NoError(t, err)
	titles := make([]string, 0, len(items))
	for _, n := range items {
		titles = append(titles, n.Title)
	}
	return titles
}

func TestDeliveryOSIsTheDefaultAndSkipsTheBrowserQueue(t *testing.T) {
	marker := setupDelivery(t, "") // absent delivery — the legacy config shape

	Send("sess-1", "Idle", "/tmp/proj", "My conversation")

	assert.True(t, osChannelFired(t, marker), "an unset delivery must keep notifying the desktop")
	assert.Empty(t, queuedTitles(t), "nothing is queued for the browser unless asked")
}

func TestDeliveryBrowserQueuesInsteadOfNotifyingTheDesktop(t *testing.T) {
	marker := setupDelivery(t, config.NotifyDeliveryBrowser)

	Send("sess-1", "Idle", "/tmp/proj", "My conversation")

	assert.False(t, osChannelFired(t, marker), "browser delivery must not also raise a desktop banner")
	require.Equal(t, []string{"Claude: Idle"}, queuedTitles(t))

	items, _, err := db.ListBrowserNotificationsSince(0)
	require.NoError(t, err)
	require.Len(t, items, 1)
	assert.Equal(t, "sess-1", items[0].SessionID, "the session rides along for click-to-focus")
	assert.Contains(t, items[0].Body, "proj")
}

func TestDeliveryBothReachesEveryChannel(t *testing.T) {
	marker := setupDelivery(t, config.NotifyDeliveryBoth)

	Send("sess-1", "Idle", "/tmp/proj", "My conversation")

	assert.True(t, osChannelFired(t, marker))
	assert.Equal(t, []string{"Claude: Idle"}, queuedTitles(t))
}

func TestDeliveryBrowserStillHonoursTheMasterSwitch(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	db.ResetForTest()
	require.NoError(t, config.Save(&config.Config{
		Notifications: &config.NotificationConfig{
			Enabled:  false,
			Delivery: config.NotifyDeliveryBrowser,
		},
	}))

	// Every gate above dispatch is unchanged by delivery: a disabled block
	// still notifies nowhere, browser included.
	OnStateTransition("sess-1", "conv-1", "working", "idle", "/tmp/proj", "My conversation", "claude")

	assert.Empty(t, queuedTitles(t))
}

func TestDeliveryConfigValidatesTheChannel(t *testing.T) {
	bad := config.Validate(&config.Config{
		Notifications: &config.NotificationConfig{Enabled: true, Delivery: "carrier-pigeon"},
	})
	require.NotEmpty(t, bad)
	assert.Contains(t, bad[0], "notifications.delivery")

	for _, ok := range []string{"", config.NotifyDeliveryOS, config.NotifyDeliveryBrowser, config.NotifyDeliveryBoth} {
		good := config.Validate(&config.Config{
			Notifications: &config.NotificationConfig{Enabled: true, Delivery: ok},
		})
		assert.Empty(t, good, "delivery %q must be accepted", ok)
	}
}
