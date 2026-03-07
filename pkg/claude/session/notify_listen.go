package session

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/godbus/dbus/v5"
	"github.com/spf13/cobra"
)

// NotifyListenCmd returns a hidden command that sends a D-Bus notification and
// listens for action signals on the same connection. The notification and signal
// must use the same D-Bus connection because notification daemons send ActionInvoked
// signals only to the connection that created the notification.
// This runs as a detached background process spawned by sendLinuxClickable.
func NotifyListenCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "notify-listen <session-id> <title> <body>",
		Short:  "Send notification and listen for click (internal)",
		Hidden: true,
		Args:   cobra.ExactArgs(3),
		Run: func(cmd *cobra.Command, args []string) {
			SetupHookLogging()

			if err := runNotifyListen(args[0], args[1], args[2]); err != nil {
				slog.Error("notify-listen failed", "error", err)
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
	defer conn.Close()

	// Set up signal listeners BEFORE sending the notification to avoid races
	if err := conn.AddMatchSignal(
		dbus.WithMatchObjectPath("/org/freedesktop/Notifications"),
		dbus.WithMatchInterface("org.freedesktop.Notifications"),
		dbus.WithMatchMember("NotificationClosed"),
	); err != nil {
		return fmt.Errorf("failed to add NotificationClosed match: %w", err)
	}

	signals := make(chan *dbus.Signal, 10)
	conn.Signal(signals)

	// Send the notification on this same connection
	obj := conn.Object("org.freedesktop.Notifications", "/org/freedesktop/Notifications")
	var actions []string

	call := obj.Call("org.freedesktop.Notifications.Notify", 0,
		"Claude Code",             // app_name
		uint32(0),                 // replaces_id
		"",                        // app_icon
		title,                     // summary
		body,                      // body
		actions,                   // no actions
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

	slog.Info("Notification sent, waiting for callback", "notifID", notifID)

	// Wait up to 5 minutes for an action or close
	timeout := time.After(5 * time.Minute)
	for {
		select {
		case sig := <-signals:
			if sig == nil {
				return nil
			}
			switch sig.Name {
			case "org.freedesktop.Notifications.NotificationClosed":
				if len(sig.Body) >= 1 {
					if id, ok := sig.Body[0].(uint32); ok && id == notifID {
						slog.Info("Notification clicked", "notifID", notifID)
						clArgs := common.DetectArgs()
						focusArgs := append(clArgs[1:], "session", "focus", sessionID)
						focusCmd := exec.Command(clArgs[0], focusArgs...)
						_ = focusCmd.Run()
						return nil
					}
				}
			}
		case <-timeout:
			return nil
		}
	}
}
