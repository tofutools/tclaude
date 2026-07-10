//go:build linux

package agentd

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCheckTrayPrerequisitesRejectsMissingSessionBus(t *testing.T) {
	missingSocket := filepath.Join(t.TempDir(), "missing-dbus.sock")
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path="+missingSocket)

	err := checkTrayPrerequisites()
	require.Error(t, err)
	require.Contains(t, err.Error(), "connect to session DBus")
}
