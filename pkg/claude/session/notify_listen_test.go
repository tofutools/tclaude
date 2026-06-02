package session

import (
	"testing"

	"github.com/godbus/dbus/v5"
)

// classifyNotifySignal is the gate that decides whether a received D-Bus
// signal focuses the agent's window. The regression it guards: the old
// notify-listen treated NotificationClosed (which fires on AUTO-EXPIRY,
// ~7s after a notification appears) as a click and force-activated the
// window. The fix focuses ONLY on a real ActionInvoked for our registered
// "default" action — never on any close.
func TestClassifyNotifySignal(t *testing.T) {
	const ourID = uint32(42)

	sig := func(name string, body ...interface{}) *dbus.Signal {
		return &dbus.Signal{Name: name, Body: body}
	}
	const (
		actionInvoked = "org.freedesktop.Notifications.ActionInvoked"
		closed        = "org.freedesktop.Notifications.NotificationClosed"
	)

	cases := []struct {
		name string
		sig  *dbus.Signal
		want notifyDecision
	}{
		{
			// THE regression: a close of our notification must not focus.
			// reason 1 == expired (auto-expiry), the case that caused the
			// focus-steal.
			name: "NotificationClosed on auto-expiry → done, never focus",
			sig:  sig(closed, ourID, uint32(1)),
			want: notifyDone,
		},
		{
			name: "NotificationClosed by user dismissal → done, never focus",
			sig:  sig(closed, ourID, uint32(2)),
			want: notifyDone,
		},
		{
			name: "real click on our default action → focus",
			sig:  sig(actionInvoked, ourID, notifyFocusActionKey),
			want: notifyFocus,
		},
		{
			name: "ActionInvoked for a different action key → ignore",
			sig:  sig(actionInvoked, ourID, "some-other-action"),
			want: notifyIgnore,
		},
		{
			name: "ActionInvoked for a different notification id → ignore",
			sig:  sig(actionInvoked, uint32(999), notifyFocusActionKey),
			want: notifyIgnore,
		},
		{
			name: "NotificationClosed for a different notification id → ignore",
			sig:  sig(closed, uint32(999), uint32(1)),
			want: notifyIgnore,
		},
		{
			name: "unrelated signal → ignore",
			sig:  sig("org.freedesktop.Notifications.SomethingElse", ourID),
			want: notifyIgnore,
		},
		{
			name: "nil signal (channel closed) → done",
			sig:  nil,
			want: notifyDone,
		},
		{
			name: "malformed ActionInvoked (missing key) → ignore",
			sig:  sig(actionInvoked, ourID),
			want: notifyIgnore,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyNotifySignal(tc.sig, ourID); got != tc.want {
				t.Fatalf("classifyNotifySignal = %d, want %d", got, tc.want)
			}
		})
	}
}
