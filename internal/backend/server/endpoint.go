package server

import (
	"errors"
	"os"
	"path/filepath"
)

// AgentSocketDirectory is a stable, server-owned namespace containing only the
// replaceable API socket. A confined child may see this directory without
// seeing any of its private backend-state siblings.
func AgentSocketDirectory(state string) string { return filepath.Join(state, "api") }
func AgentSocketPath(state string) string {
	return filepath.Join(AgentSocketDirectory(state), "socket")
}

func prepareAPIEndpoint(state string) (string, error) {
	directory := AgentSocketDirectory(state)
	if err := os.Mkdir(directory, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	info, err := os.Lstat(directory)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return "", errors.New("API endpoint directory must be private and must not be a symlink")
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		if entry.Name() != "socket" {
			return "", errors.New("API endpoint directory contains an unexpected entry")
		}
	}
	// Keep the previous CLI/client endpoint working. Holding the state lock
	// proves an old socket here belongs to a stopped server; arbitrary files or
	// links are never removed. The relative link remains valid if state moves.
	legacy := filepath.Join(state, "api.sock")
	target := filepath.Join("api", "socket")
	if info, err := os.Lstat(legacy); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			existing, err := os.Readlink(legacy)
			if err != nil || existing != target {
				return "", errors.New("API compatibility path contains an unexpected link")
			}
			return AgentSocketPath(state), nil
		}
		if info.Mode()&os.ModeSocket == 0 {
			return "", errors.New("API compatibility path contains a non-socket file")
		}
		if err := os.Remove(legacy); err != nil {
			return "", err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err := os.Symlink(target, legacy); err != nil {
		return "", err
	}
	return AgentSocketPath(state), nil
}
