//go:build linux || darwin

package host

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestObservationSpoolDrainsOnlyCompletedAttemptEvents(t *testing.T) {
	root := filepath.Join(t.TempDir(), "private")
	spool, err := PrepareObservationSpool(root)
	require.NoError(t, err)
	requirePermissions(t, spool.Directory(), 0o700)

	require.NoError(t, os.WriteFile(filepath.Join(spool.Directory(), ".event-pending"), []byte("partial"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(spool.Directory(), "event-002"), []byte("second"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(spool.Directory(), "event-001"), []byte("first"), 0o600))

	first := time.Unix(1000, 0).UTC()
	second := first.Add(time.Second)
	require.NoError(t, os.Chtimes(filepath.Join(spool.Directory(), "event-001"), first, first))
	require.NoError(t, os.Chtimes(filepath.Join(spool.Directory(), "event-002"), second, second))

	events, err := spool.ReadPending()
	require.NoError(t, err)
	require.Equal(t, []ObservationSpoolEvent{
		{Order: "event-001", Payload: []byte("first"), RecordedAt: first},
		{Order: "event-002", Payload: []byte("second"), RecordedAt: second},
	}, events)
	require.FileExists(t, filepath.Join(spool.Directory(), "event-001"))
	require.NoError(t, spool.Acknowledge(events[0].Order))
	require.NoFileExists(t, filepath.Join(spool.Directory(), "event-001"))
	events, err = spool.ReadPending()
	require.NoError(t, err)
	require.Equal(t, []ObservationSpoolEvent{{Order: "event-002", Payload: []byte("second"), RecordedAt: second}}, events)
	require.NoError(t, spool.Acknowledge(events[0].Order))
	require.FileExists(t, filepath.Join(spool.Directory(), ".event-pending"))

	recovered, err := RecoverObservationSpool(root, spool.Directory())
	require.NoError(t, err)
	require.NoError(t, recovered.Remove())
	require.NoDirExists(t, spool.Directory())
}

func TestObservationSpoolRejectsCrossAttemptAndSymlinkEvents(t *testing.T) {
	root := filepath.Join(t.TempDir(), "private")
	spool, err := PrepareObservationSpool(root)
	require.NoError(t, err)

	foreign, err := PrepareObservationSpool(root)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(foreign.Directory(), "event-foreign"), []byte("foreign"), 0o600))
	events, err := spool.ReadPending()
	require.NoError(t, err)
	require.Empty(t, events)

	target := filepath.Join(t.TempDir(), "payload")
	require.NoError(t, os.WriteFile(target, []byte("forged"), 0o600))
	require.NoError(t, os.Symlink(target, filepath.Join(spool.Directory(), "event-symlink")))
	_, err = spool.ReadPending()
	require.ErrorContains(t, err, "bounded regular file")
}
