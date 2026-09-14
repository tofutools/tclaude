package db

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
)

// PinNetworkSyncDatabase opens the launcher's existing private database before
// a supervisor uses any repository functions. In particular, OpenCode gives
// its outer process a filtered HOME; deriving authority from that HOME would
// create an unrelated empty database. This path is trusted launch metadata,
// never received through the sandbox's agent-facing API.
func PinNetworkSyncDatabase(path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("network sync requires an absolute database path")
	}
	stateMu.Lock()
	defer stateMu.Unlock()
	if dbReady {
		if initErr != nil {
			return initErr
		}
		if filepath.Clean(globalDBPath) != filepath.Clean(path) {
			return fmt.Errorf("network sync database differs from initialized database")
		}
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("network sync database is not a regular file")
	}
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=rw&_txlock=immediate&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"}).String()
	d, err := sql.Open("sqlite", dsn)
	if err != nil {
		return err
	}
	if version := schemaVersion(d); version != currentVersion {
		_ = d.Close()
		return fmt.Errorf("network sync database schema %d differs from supervisor schema %d", version, currentVersion)
	}
	globalDB, globalDBPath, dbReady, initErr = d, path, true, nil
	return nil
}
