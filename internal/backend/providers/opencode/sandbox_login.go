//go:build linux || darwin

package opencode

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"
)

// Seed only a newly allocated native state. Continuation retains its own login,
// including intentional logout; neither ambient credential refresh nor retry
// overwrites a native session's independently refreshed credentials.
func seedSandboxLogin(nativeData, stateRoot string) error {
	if nativeData == "" {
		return nil
	}
	source, err := os.OpenRoot(nativeData)
	if os.IsNotExist(err) {
		return nil // A user who has not logged in has no credential to seed.
	}
	if err != nil {
		return fmt.Errorf("open OpenCode native login directory: %w", err)
	}
	defer func() { _ = source.Close() }()
	state, err := os.OpenRoot(stateRoot)
	if err != nil {
		return err
	}
	defer func() { _ = state.Close() }()
	destination, err := state.OpenRoot(filepath.Join("data", "opencode"))
	if err != nil {
		return err
	}
	defer func() { _ = destination.Close() }()
	for _, name := range []string{"auth.json", "mcp-auth.json"} {
		if err := seedSandboxLoginFile(source, destination, name); err != nil {
			return fmt.Errorf("seed OpenCode %s: %w", name, err)
		}
	}
	return nil
}

func seedSandboxLoginFile(source, destination *os.Root, name string) error {
	if info, err := destination.Lstat(name); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("existing private login is not a regular file")
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	// NONBLOCK lets us refuse FIFOs before a writer appears; NOFOLLOW prevents
	// an exact login filename from turning into access to unrelated user data.
	input, err := source.OpenFile(name, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = input.Close() }()
	info, err := input.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("native login is not a regular file")
	}
	// Publish a complete private copy without replacing an existing login.
	// An interrupted copy must never become the credential used on a retry.
	temporary := ".login-" + uuid.NewString()
	output, err := destination.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer func() { _ = destination.Remove(temporary) }()
	_, copyErr := io.Copy(output, input)
	if copyErr == nil {
		copyErr = output.Sync()
	}
	closeErr := output.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if err := destination.Link(temporary, name); err != nil {
		if !os.IsExist(err) {
			return err
		}
		info, statErr := destination.Lstat(name)
		if statErr != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("private login changed during initialization")
		}
	}
	return nil
}
