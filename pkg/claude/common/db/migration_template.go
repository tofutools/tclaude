package db

import (
	"database/sql"
	"os"
	"path/filepath"
	"sync"

	"github.com/tofutools/tclaude/pkg/common"
)

// Test-only migration template.
//
// A fresh database is built by createSchema + replaying the entire
// v0->vN migration chain. On pure-Go sqlite (modernc) that costs ~290ms,
// and the test suite opens a brand-new database per test (each test resets
// $HOME + the singleton), so that 290ms is paid hundreds of times and
// dominates total wall-clock.
//
// These helpers cache the first fully-migrated *empty* database produced in
// a test process and let sibling tests seed their db file from that snapshot,
// so migrate() sees the current version and returns immediately. Net effect:
// the migration chain runs once per `go test` process instead of once per
// test. The mechanism is inert in production — the gate (testTemplateEnabled)
// is only flipped by ResetForTest, which is documented test-only.
var (
	testTemplateMu      sync.Mutex
	testTemplateEnabled bool
	testTemplateBytes   []byte
)

// enableMigrationTemplate arms the template fast-path for the current
// process. Called from ResetForTest so every test that resets the DB opts in
// transparently.
func enableMigrationTemplate() {
	testTemplateMu.Lock()
	testTemplateEnabled = true
	testTemplateMu.Unlock()
}

// legacySourcesPresent reports whether the importLegacyData inputs exist under
// the .tclaude dir. When they do we must run the real createSchema +
// importLegacyData path (see TestLegacyImport), never the template short-cut.
func legacySourcesPresent(tcDir string) bool {
	for _, sub := range []string{"claude-sessions", "notify-state"} {
		if fi, err := os.Stat(filepath.Join(tcDir, sub)); err == nil && fi.IsDir() {
			return true
		}
	}
	return false
}

// maybeSeedFromTemplate writes the cached migrated snapshot to dbPath when one
// is available and safe to use, so the subsequent migrate() is a no-op.
// Returns true when it seeded (and thus capture should be skipped).
func maybeSeedFromTemplate(dbPath string) bool {
	testTemplateMu.Lock()
	defer testTemplateMu.Unlock()
	if !testTemplateEnabled || len(testTemplateBytes) == 0 {
		return false
	}
	// Legacy import inputs live at the tclaude ROOT (~/.tclaude/claude-sessions,
	// ~/.tclaude/notify-state), one level above the DB's data/ dir. Check there,
	// not filepath.Dir(dbPath) (which is ~/.tclaude/data).
	if legacySourcesPresent(common.TclaudeDir()) {
		return false
	}
	if _, err := os.Stat(dbPath); err == nil {
		return false // a real db is already present; don't clobber it
	}
	if err := os.WriteFile(dbPath, testTemplateBytes, 0644); err != nil {
		return false
	}
	return true
}

// maybeCaptureTemplate snapshots a freshly-migrated empty database into the
// process-wide template cache via VACUUM INTO (which yields a self-contained
// single-file copy regardless of WAL state). No-op unless the fast-path is
// armed, the cache is empty, and no legacy data was imported into this db.
func maybeCaptureTemplate(d *sql.DB) {
	testTemplateMu.Lock()
	defer testTemplateMu.Unlock()
	if !testTemplateEnabled || len(testTemplateBytes) > 0 {
		return
	}
	if legacySourcesPresent(common.TclaudeDir()) {
		return
	}
	tmp, err := os.CreateTemp("", "tclaude-dbtmpl-*.sqlite")
	if err != nil {
		return
	}
	tmpPath := tmp.Name()
	_ = tmp.Close()
	_ = os.Remove(tmpPath) // VACUUM INTO requires the target not to exist
	if _, err := d.Exec("VACUUM INTO ?", tmpPath); err != nil {
		_ = os.Remove(tmpPath)
		return
	}
	b, err := os.ReadFile(tmpPath)
	_ = os.Remove(tmpPath)
	if err == nil {
		testTemplateBytes = b
	}
}
