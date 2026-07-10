//go:build linux

package agentd

import (
	"fmt"

	"github.com/godbus/dbus/v5"
)

// checkTrayPrerequisites verifies the Linux session bus before entering
// fyne.io/systray. Use a private probe connection: dbus.SessionBus returns a
// process-wide shared connection that callers must not close.
func checkTrayPrerequisites() error {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return fmt.Errorf("connect to session DBus: %w", err)
	}
	_ = conn.Close()
	return nil
}
