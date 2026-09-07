package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

const pendingImportMarker = "tclaude replacement backend pending offline import v1\n"

// ImportOffline keeps a new state directory unservable until its offline writer
// has durably published and verified the database. Retrying an existing target
// is delegated to the writer's exact receipt/semantic verification. The same
// lock as Serve excludes an active daemon throughout verification and completion.
func ImportOffline(ctx context.Context, dir string, write func(context.Context, string) error) error {
	if !filepath.IsAbs(dir) || write == nil {
		return errors.New("absolute import state directory and writer are required")
	}
	if _, err := os.Lstat(dir); errors.Is(err, os.ErrNotExist) {
		if err := initialize(dir, pendingImportMarker); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return errors.New("import state directory must be private and must not be a symlink")
	}
	lock, err := os.OpenFile(filepath.Join(dir, "daemon.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return errors.New("state directory is in use")
	}
	format, err := os.ReadFile(filepath.Join(dir, "FORMAT"))
	if err != nil {
		return err
	}
	if string(format) != pendingImportMarker && string(format) != marker {
		return errors.New("state directory is not an initialized offline import target")
	}
	if err := write(ctx, filepath.Join(dir, "backend.sqlite")); err != nil {
		return err
	}
	// Publish readiness only after the database callback succeeds. A crash before
	// this rename leaves the pending marker and is safe to retry explicitly.
	tmp, err := os.CreateTemp(dir, ".format-")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err = tmp.WriteString(marker); err != nil {
		_ = tmp.Close()
		return err
	}
	if err = tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp.Name(), filepath.Join(dir, "FORMAT")); err != nil {
		return err
	}
	parent, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer parent.Close()
	if err := parent.Sync(); err != nil {
		return fmt.Errorf("sync imported state readiness: %w", err)
	}
	return nil
}
