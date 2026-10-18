package config

import (
	"context"
	"os"
	"sync"
	"time"
)

// shared is the in-memory config a long-running daemon reads on hot paths
// (permission checks, per-request feature flags) instead of re-reading and
// re-parsing config.json each time. WatchShared keeps it fresh; without a
// watcher LoadShared is plain Load.
var shared struct {
	mu       sync.Mutex
	watchers int
	cfg      *Config
	err      error
	path     string
	info     os.FileInfo
}

// LoadShared returns the watched config. The result is shared between
// callers and must be treated as read-only; use Load or Update to change it.
func LoadShared() (*Config, error) {
	shared.mu.Lock()
	if shared.watchers == 0 {
		shared.mu.Unlock()
		return Load()
	}
	defer shared.mu.Unlock()
	if shared.cfg == nil {
		reloadSharedLocked()
	}
	return shared.cfg, shared.err
}

// WatchShared makes LoadShared serve an in-memory config until ctx ends,
// re-stating config.json every interval and reloading it only when it was
// replaced or changed. In-process saves refresh it at once.
func WatchShared(ctx context.Context, interval time.Duration) {
	shared.mu.Lock()
	shared.watchers++
	reloadSharedLocked()
	shared.mu.Unlock()
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				shared.mu.Lock()
				if shared.watchers--; shared.watchers == 0 {
					shared.cfg, shared.err, shared.info = nil, nil, nil
				}
				shared.mu.Unlock()
				return
			case <-t.C:
				shared.mu.Lock()
				if sharedStaleLocked() {
					reloadSharedLocked()
				}
				shared.mu.Unlock()
			}
		}
	}()
}

// invalidateShared drops the cached config after an in-process save, so the
// next LoadShared re-reads it instead of waiting for the poller.
func invalidateShared() {
	shared.mu.Lock()
	shared.cfg = nil
	shared.mu.Unlock()
}

func sharedStaleLocked() bool {
	path := ConfigPath()
	// A failed read (fd pressure, a half-written edit) is retried every poll
	// rather than serving defaults until the file next changes.
	if path != shared.path || shared.err != nil {
		return true
	}
	info, err := os.Stat(path)
	if err != nil || shared.info == nil {
		// Created, removed, or unreadable (retry until it reads again).
		return (err == nil) != (shared.info != nil) || err != nil && !os.IsNotExist(err)
	}
	// A save renames a new file into place, which SameFile catches even when
	// the size and timestamp happen to match.
	return !os.SameFile(info, shared.info) || !info.ModTime().Equal(shared.info.ModTime()) || info.Size() != shared.info.Size()
}

func reloadSharedLocked() {
	shared.path = ConfigPath()
	shared.info, _ = os.Stat(shared.path)
	shared.cfg, shared.err = Load()
}
