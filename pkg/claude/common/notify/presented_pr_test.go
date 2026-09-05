package notify

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

func TestFormatPresentedPR(t *testing.T) {
	t.Run("full attribution: url leads the body, summary and group follow", func(t *testing.T) {
		title, body := formatPresentedPR("tclaude-worker", "tofutools",
			"https://github.com/tofutools/tclaude/pull/42", "notification on present-pr")
		assert.Equal(t, "Claude: pull request", title)
		assert.Equal(t, "tclaude-worker presented a pull request\n"+
			"https://github.com/tofutools/tclaude/pull/42\n"+
			"notification on present-pr\n— tofutools", body)
	})

	t.Run("unknown agent title falls back to a generic phrase", func(t *testing.T) {
		_, body := formatPresentedPR("", "", "https://example.com/pr/1", "")
		assert.Equal(t, "An agent presented a pull request\nhttps://example.com/pr/1", body)
	})

	t.Run("surrounding whitespace is trimmed on every field", func(t *testing.T) {
		_, body := formatPresentedPR("  worker  ", "  squad  ", "  https://example.com/pr/2  ", "  fix  ")
		assert.Equal(t, "worker presented a pull request\nhttps://example.com/pr/2\nfix\n— squad", body)
	})

	t.Run("an over-long summary is skipped whole, leaving the URL and group intact", func(t *testing.T) {
		const prURL = "https://example.com/pr/3"
		_, body := formatPresentedPR("worker", "squad", prURL, strings.Repeat("x", notifyBodyMaxLen+500))
		assert.LessOrEqual(t, len([]rune(body)), notifyBodyMaxLen)
		assert.Equal(t, "worker presented a pull request\n"+prURL+"\n— squad", body,
			"a summary that cannot fit whole is dropped, not cut mid-word")
	})

	// A presented URL may be up to maxAgentPRURLLen (2048) bytes while the
	// banner body caps at 1024 runes, so a long-but-valid URL must not be
	// the thing that gets cut — a half-URL is not actionable.
	t.Run("a long url survives whole; the trailers give way instead", func(t *testing.T) {
		longURL := "https://example.com/org/repo/pull/" + strings.Repeat("a", 900)
		_, body := formatPresentedPR("worker", "squad", longURL, strings.Repeat("s", 200))
		assert.LessOrEqual(t, len([]rune(body)), notifyBodyMaxLen)
		assert.Contains(t, body, longURL, "the whole URL is present, uncut")
		assert.NotContains(t, body, "sss", "the summary gave way to keep the URL whole")
		assert.Contains(t, body, "— squad", "the short group line still fits behind it")
	})

	// Nothing can rescue a URL longer than the banner itself; assert only
	// that the cap still holds rather than pretending otherwise.
	t.Run("a url longer than the whole banner is still capped", func(t *testing.T) {
		_, body := formatPresentedPR("worker", "squad",
			"https://example.com/"+strings.Repeat("a", 2000), "summary")
		assert.LessOrEqual(t, len([]rune(body)), notifyBodyMaxLen)
	})

	// An agent title is free text — a /rename can make it arbitrarily long
	// — so the attribution must not be allowed to push a perfectly short
	// URL past the cut. Losing who presented it still leaves an actionable
	// banner; losing the URL does not.
	t.Run("a huge agent title is trimmed to keep the url", func(t *testing.T) {
		const prURL = "https://example.com/pr/7"
		_, body := formatPresentedPR(strings.Repeat("n", notifyBodyMaxLen+500), "squad", prURL, "fix")
		assert.LessOrEqual(t, len([]rune(body)), notifyBodyMaxLen)
		assert.Contains(t, body, prURL, "the URL survives an over-long attribution")
		assert.True(t, strings.HasPrefix(body, "nnn"), "what fits of the attribution is kept")
	})
}

// SendPresentedPR self-gates so its callers stay dumb, and it must hold
// BOTH gates: the notifications master switch and the opt-in
// agent.present_pr_notification. Only both-on notifies — which is what
// keeps an existing config from starting to banner on upgrade.
//
// Observed through browser delivery so the assertion is a queue read
// rather than a platform send (the pattern setupDelivery establishes for
// the OS channel).
func TestSendPresentedPR_RequiresBothGates(t *testing.T) {
	for _, tc := range []struct {
		name          string
		master, optIn bool
		wantNotified  bool
	}{
		{"both off", false, false, false},
		{"master on, opt-in off (the default shape)", true, false, false},
		{"master off, opt-in on", false, true, false},
		{"both on", true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			db.ResetForTest()
			t.Cleanup(db.ResetForTest)
			require.NoError(t, config.Save(&config.Config{
				Notifications: &config.NotificationConfig{
					Enabled:  tc.master,
					Delivery: config.NotifyDeliveryBrowser,
				},
				Agent: &config.AgentConfig{PresentPRNotification: tc.optIn},
			}))

			SendPresentedPR("sess-1", "worker", "squad", "https://example.com/pr/9", "fix")

			queued := queuedTitles(t)
			if !tc.wantNotified {
				assert.Empty(t, queued, "the banner must stay silent unless both gates are on")
				return
			}
			require.Equal(t, []string{"Claude: pull request"}, queued)
			items, _, err := db.ListBrowserNotificationsSince(0)
			require.NoError(t, err)
			require.Len(t, items, 1)
			assert.Contains(t, items[0].Body, "https://example.com/pr/9",
				"the PR URL is what the human acts on")
			assert.Equal(t, "sess-1", items[0].SessionID,
				"the presenting agent's session rides along for click-to-focus")
		})
	}
}

// An agent block that has never seen the key must not notify: absence is
// the default-off state, so an upgrade cannot silently start bannering.
func TestSendPresentedPR_AbsentKeyStaysSilent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	db.ResetForTest()
	t.Cleanup(db.ResetForTest)
	require.NoError(t, config.Save(&config.Config{
		Notifications: &config.NotificationConfig{
			Enabled:  true,
			Delivery: config.NotifyDeliveryBrowser,
		},
	}))

	SendPresentedPR("sess-1", "worker", "squad", "https://example.com/pr/9", "fix")

	assert.Empty(t, queuedTitles(t), "no agent block at all means the feature is off")
}
