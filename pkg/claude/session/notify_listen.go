package session

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/common"
)

// notifyFocusActionKey / notifyFocusActionLabel are the single D-Bus
// notification action we register so a REAL click can be told apart from
// an auto-expiry. "default" is the freedesktop convention for "the
// notification body was activated" — KDE Plasma, GNOME Shell and dunst
// all invoke it on a click and render no visible button for it. The label
// is the human-readable half of the action pair (shown only by daemons
// that surface non-default actions as buttons).
const (
	notifyFocusActionKey   = "default"
	notifyFocusActionLabel = "Focus window"
)

// NotifyListenCmd returns a hidden command that sends a D-Bus notification
// and listens for action signals on the same connection. The notification
// and signal must use the same D-Bus connection because notification
// daemons send ActionInvoked signals only to the connection that created
// the notification. This runs as a detached background process spawned by
// sendLinuxClickable.
//
// It focuses the agent's window ONLY on a real click (ActionInvoked for
// our registered action) — never on NotificationClosed, which fires on
// auto-expiry. The previous implementation registered no actions and
// treated NotificationClosed as a click, so every notification
// force-activated the window a few seconds after it appeared.
func NotifyListenCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "notify-listen <session-id> <title> <body>",
		Short:  "Send notification and listen for click (internal)",
		Hidden: true,
		Args:   cobra.ExactArgs(3),
		Run: func(cmd *cobra.Command, args []string) {
			if err := runNotifyListen(args[0], args[1], args[2]); err != nil {
				slog.Error("notify-listen failed", "error", err, "module", "hooks")
				os.Exit(1)
			}
		},
	}
}

func runNotifyListen(sessionID, title, body string) error {
	conn, err := dbus.SessionBus()
	if err != nil {
		return fmt.Errorf("failed to connect to session bus: %w", err)
	}
	defer func() { _ = conn.Close() }()

	// Match BOTH ActionInvoked (a real click → focus) and
	// NotificationClosed (auto-expiry or dismissal → stop waiting, do NOT
	// focus). Register the matches BEFORE sending the notification so a
	// fast signal can't arrive before we are subscribed.
	for _, member := range []string{"ActionInvoked", "NotificationClosed"} {
		if err := conn.AddMatchSignal(
			dbus.WithMatchObjectPath("/org/freedesktop/Notifications"),
			dbus.WithMatchInterface("org.freedesktop.Notifications"),
			dbus.WithMatchMember(member),
		); err != nil {
			return fmt.Errorf("failed to add %s match: %w", member, err)
		}
	}

	signals := make(chan *dbus.Signal, 10)
	conn.Signal(signals)

	// Register a single "default" action so the notification is clickable
	// and a genuine click is reported back as ActionInvoked. Without an
	// action the daemon never emits ActionInvoked at all, so a click would
	// be undetectable — which is exactly why the old code latched onto
	// NotificationClosed and mistook auto-expiry for a click.
	obj := conn.Object("org.freedesktop.Notifications", "/org/freedesktop/Notifications")
	actions := []string{notifyFocusActionKey, notifyFocusActionLabel}

	call := obj.Call("org.freedesktop.Notifications.Notify", 0,
		"Claude Code",             // app_name
		uint32(0),                 // replaces_id
		"",                        // app_icon
		title,                     // summary
		body,                      // body
		actions,                   // actions — one default click-to-focus action
		map[string]dbus.Variant{}, // hints
		int32(-1),                 // expire_timeout (-1 = server default)
	)
	if err := call.Err; err != nil {
		return fmt.Errorf("notification failed: %w", err)
	}

	var notifID uint32
	if err := call.Store(&notifID); err != nil {
		return fmt.Errorf("failed to get notification ID: %w", err)
	}

	slog.Info("Notification sent, waiting for click", "notifID", notifID, "module", "hooks")

	// Wait for the user to click or dismiss the notification. The real
	// click window is the notification's on-screen lifetime (set by the
	// daemon via expire_timeout); the 5-minute cap is only a backstop so a
	// daemon that never emits a close can't leave this listener lingering.
	timeout := time.After(5 * time.Minute)
	for {
		select {
		case sig := <-signals:
			switch classifyNotifySignal(sig, notifID) {
			case notifyFocus:
				slog.Info("Notification clicked, focusing window", "notifID", notifID, "module", "hooks")
				focusSession(sessionID)
				return nil
			case notifyDone:
				slog.Debug("Notification closed without a click; not focusing", "notifID", notifID, "module", "hooks")
				return nil
			case notifyIgnore:
				// A signal for a different notification — the daemon
				// broadcasts ActionInvoked/NotificationClosed to every
				// listener on the bus, so we filter by notifID. Keep
				// waiting for ours.
			}
		case <-timeout:
			return nil
		}
	}
}

// notifyDecision is what runNotifyListen should do with a received signal.
type notifyDecision int

const (
	notifyIgnore notifyDecision = iota // not ours / irrelevant — keep waiting
	notifyFocus                        // user clicked our action — focus the window
	notifyDone                         // our notification closed with no click — stop
)

// classifyNotifySignal decides what to do with one D-Bus signal. It is the
// single home of the focus-vs-ignore rule, kept pure so the regression it
// guards is unit-testable without a live session bus: NotificationClosed
// fires on auto-expiry as well as on dismissal, so it must NEVER focus —
// only a real ActionInvoked for our registered action key does.
func classifyNotifySignal(sig *dbus.Signal, notifID uint32) notifyDecision {
	if sig == nil {
		return notifyDone
	}
	switch sig.Name {
	case "org.freedesktop.Notifications.ActionInvoked":
		// Body: (id uint32, action_key string). A genuine click.
		if len(sig.Body) >= 2 {
			id, idOK := sig.Body[0].(uint32)
			key, keyOK := sig.Body[1].(string)
			if idOK && keyOK && id == notifID && key == notifyFocusActionKey {
				return notifyFocus
			}
		}
		return notifyIgnore
	case "org.freedesktop.Notifications.NotificationClosed":
		// Body: (id uint32, reason uint32). Closing — INCLUDING auto-expiry
		// — must never focus. We only use it to stop waiting once it is our
		// own notification that closed.
		if len(sig.Body) >= 1 {
			if id, ok := sig.Body[0].(uint32); ok && id == notifID {
				return notifyDone
			}
		}
		return notifyIgnore
	}
	return notifyIgnore
}

// focusSession shells out to `tclaude session focus <id>` — the explicit,
// human-initiated focus path (a click on the notification IS such an
// action). Split out so runNotifyListen's select loop reads cleanly.
func focusSession(sessionID string) {
	clArgs := common.DetectArgs()
	focusArgs := append(clArgs[1:], "session", "focus", sessionID)
	focusCmd := exec.Command(clArgs[0], focusArgs...)
	_ = focusCmd.Run()
}
