package server

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/providers"
	"golang.org/x/sys/unix"
)

func TestOfflineImportRemainsUnservableUntilVerified(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "import")
	failed := errors.New("publication failed")
	err := ImportOffline(ctx, dir, func(context.Context, string) error { return failed })
	require.ErrorIs(t, err, failed)
	data, err := os.ReadFile(filepath.Join(dir, "FORMAT"))
	require.NoError(t, err)
	require.Equal(t, pendingImportMarker, string(data))
	require.ErrorContains(t, Serve(ctx, dir, providers.NewRegistry()), "not initialized")
	err = ImportOffline(ctx, dir, func(_ context.Context, path string) error {
		require.Equal(t, filepath.Join(dir, "backend.sqlite"), path)
		return os.WriteFile(path, []byte("verified fixture"), 0600)
	})
	require.NoError(t, err)
	data, err = os.ReadFile(filepath.Join(dir, "FORMAT"))
	require.NoError(t, err)
	require.Equal(t, marker, string(data))
	// A receipt verifier can refuse subsequent target changes; it is never
	// converted to success merely because readiness already exists.
	require.ErrorIs(t, ImportOffline(ctx, dir, func(context.Context, string) error { return failed }), failed)
	lock, err := os.OpenFile(filepath.Join(dir, "daemon.lock"), os.O_RDWR, 0600)
	require.NoError(t, err)
	defer lock.Close()
	require.NoError(t, unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB))
	called := false
	require.ErrorContains(t, ImportOffline(ctx, dir, func(context.Context, string) error { called = true; return nil }), "in use")
	require.False(t, called)
}
