package paneinput

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testutil"
)

func TestHumanInputGuardRechecksBeforeSubmit(t *testing.T) {
	t.Setenv("HOME", testutil.CanonicalTempDir(t))
	db.ResetForTest()
	t.Cleanup(db.ResetForTest)
	row := &db.SessionRow{ID: "guard", TmuxSession: "guard", Status: "idle", CreatedAt: time.Now(), UpdatedAt: time.Now()}
	require.NoError(t, db.SaveSession(row))
	var keys []string
	err := InjectTextAndSubmit("guard:0.0", "/compact", Options{
		Run: func(args ...string) error {
			if args[0] == "send-keys" {
				keys = append(keys, args[len(args)-1])
				row.Status = "awaiting_permission"
				row.UpdatedAt = time.Now()
				return db.SaveSession(row)
			}
			return nil
		}, SettleDelaySet: true,
	})
	require.ErrorIs(t, err, ErrAwaitingHumanInput)
	require.Equal(t, []string{"/compact"}, keys, "a dialog raised after text must receive no submit Enter")
}

func TestHumanInputGuardScreenAndCancellation(t *testing.T) {
	t.Setenv("HOME", testutil.CanonicalTempDir(t))
	db.ResetForTest()
	t.Cleanup(db.ResetForTest)
	row := &db.SessionRow{ID: "guard", TmuxSession: "guard", Status: "idle", CreatedAt: time.Now(), UpdatedAt: time.Now()}
	require.NoError(t, db.SaveSession(row))
	called := false
	opts := Options{Run: func(...string) error { called = true; return nil }, SettleDelaySet: true,
		CapturePane: func(string) (string, error) { return "Do you want to proceed?\n1. Yes\nEsc to cancel", nil },
	}
	require.ErrorIs(t, InjectTextAndSubmit("guard:0.0", "/rename work", opts), ErrAwaitingHumanInput)
	require.False(t, called, "screen check closes the hook-lag window without any keystrokes")
	row.Status = "awaiting_input"
	require.NoError(t, db.SaveSession(row))
	require.NoError(t, SendKeys("guard:0.0", opts, "Escape", "C-c"))
	require.True(t, called, "safe cancellation stays available")
	called = false
	opts.AnswerHumanInput = true
	require.NoError(t, SendKeys("guard:0.0", opts, "Enter"))
	require.True(t, called, "explicitly authorized dialog answers are preserved")
	require.False(t, ScreenAwaitsHumanInput("model says: do you want to proceed?"))
}

func TestHumanInputGuardSubmittedTurnDoesNotRetry(t *testing.T) {
	t.Setenv("HOME", testutil.CanonicalTempDir(t))
	db.ResetForTest()
	t.Cleanup(db.ResetForTest)
	row := &db.SessionRow{ID: "guard", TmuxSession: "guard", Status: "idle", CreatedAt: time.Now(), UpdatedAt: time.Now()}
	require.NoError(t, db.SaveSession(row))
	var keys []string
	err := InjectTextAndSubmit("guard:0.0", "nudge", Options{
		SettleDelaySet: true,
		Run: func(args ...string) error {
			if args[0] == "send-keys" {
				key := args[len(args)-1]
				keys = append(keys, key)
				if key == "Enter" {
					row.Status = "awaiting_permission"
					return db.SaveSession(row)
				}
			}
			return nil
		},
	})
	require.NoError(t, err, "the submitted turn must be acknowledged rather than replayed")
	require.Equal(t, []string{"nudge", "Enter"}, keys, "the workaround Enter must not answer the new dialog")
}
