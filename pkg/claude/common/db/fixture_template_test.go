package db

import (
	"database/sql"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// Migrated fixture databases are expensive to build: a fresh database at the
// head version runs every migration step (~1.5s on Linux, ~3x that on the
// macOS runners), and dozens of tests and table-driven subtests each built
// their own. Each fixture is now built once per test binary by its first
// caller into one file, and every caller gets a private byte copy of that
// file. The copy is the same database
// the build produced — schema, rows, sqlite_sequence high-water marks — so
// the tests observe exactly what a fresh build gives them.

var (
	fixtureTemplateMu   sync.Mutex
	fixtureTemplateDir  string
	fixtureTemplatePath = map[string]string{}
)

func TestMain(m *testing.M) {
	code := m.Run()
	if fixtureTemplateDir != "" {
		_ = os.RemoveAll(fixtureTemplateDir)
	}
	os.Exit(code)
}

// templatedFixtureDB opens a private copy of the fixture named name, building
// it with build on first use. build runs once per test binary, inside the
// first caller's test (so under that test's environment), against a
// connection opened like the callers' own.
func templatedFixtureDB(t *testing.T, name string, build func(d *sql.DB)) *sql.DB {
	t.Helper()
	src := fixtureTemplate(t, name, build)
	raw, err := os.ReadFile(src)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), name+".sqlite")
	require.NoError(t, os.WriteFile(path, raw, 0o600))
	d, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	return d
}

// fixtureTemplate returns the captured file of the fixture named name,
// building it first if no test has yet. The lock is released by defer so a
// failing build (require stops the test) cannot wedge later callers.
func fixtureTemplate(t *testing.T, name string, build func(d *sql.DB)) string {
	t.Helper()
	fixtureTemplateMu.Lock()
	defer fixtureTemplateMu.Unlock()
	if src, ok := fixtureTemplatePath[name]; ok {
		return src
	}
	if fixtureTemplateDir == "" {
		dir, err := os.MkdirTemp("", "tclaude-db-fixtures-")
		require.NoError(t, err)
		fixtureTemplateDir = dir
	}
	src := filepath.Join(fixtureTemplateDir, name+".sqlite")
	d, err := sql.Open("sqlite", src+"?_pragma=foreign_keys(1)")
	require.NoError(t, err)
	d.SetMaxOpenConns(1)
	build(d)
	// Closed, the file is the whole database: these connections use SQLite's
	// default rollback journal, never WAL. (VACUUM INTO is no substitute: it
	// rewrites sqlite_master in a new order, which the schema dumps compare.)
	require.NoError(t, d.Close(), "close fixture %s", name)
	fixtureTemplatePath[name] = src
	return src
}

// migratedTo builds a fixture at schema version upTo: the base schema plus
// every migration step up to and including it.
func migratedTo(t *testing.T, upTo int) func(d *sql.DB) {
	return func(d *sql.DB) {
		require.NoError(t, createSchema(d))
		for _, step := range migrationSteps {
			if step.version > upTo {
				break
			}
			require.NoErrorf(t, step.apply(d), "migration v%d", step.version)
		}
	}
}
