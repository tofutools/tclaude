package db

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// openRawMigrationDB opens a fresh SQLite file with the same PRAGMAs
// production uses (see Open in db.go), so migrate() runs the real chain under
// the same foreign-key semantics — not the bare connection the single-step
// migration tests use.
func openRawMigrationDB(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "migrate.sqlite")
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	d, err := sql.Open("sqlite", dsn)
	require.NoError(t, err, "open raw sqlite")
	t.Cleanup(func() { _ = d.Close() })
	return d
}

// TestMigrationStepsAreContiguous pins the migrationSteps table's shape: one
// entry per version from v2 up to currentVersion, strictly increasing and
// gap-free, ending exactly at head. A mis-numbered or forgotten entry (the
// classic hazard when two branches both add a migration) fails here instead of
// silently skipping a migration at runtime.
func TestMigrationStepsAreContiguous(t *testing.T) {
	require.NotEmpty(t, migrationSteps)
	assert.Equal(t, 2, migrationSteps[0].version, "chain starts at v2 (v0→v1 is createSchema)")
	assert.Equal(t, currentVersion, migrationSteps[len(migrationSteps)-1].version,
		"chain ends at currentVersion")
	for i, step := range migrationSteps {
		assert.Equal(t, i+2, step.version, "migrationSteps[%d] version", i)
		require.NotNil(t, step.apply, "migrationSteps[%d] apply func", i)
	}
	assert.Len(t, migrationSteps, currentVersion-1, "one step per version bump v1→…→head")
}

// TestMigrate_ReporterFiresForFreshDB runs the full chain against a brand-new
// DB and asserts the reporter sees begin(0 → head), an Applying+Applied pair
// for every version 2..head in order, and a single Done(head) — with
// AlreadyCurrent staying silent because there was real work. A second migrate()
// on the now-current DB fires ONLY AlreadyCurrent(head) and none of the
// begin/apply/done bookends — a no-op restart announces itself but migrates
// nothing.
func TestMigrate_ReporterFiresForFreshDB(t *testing.T) {
	d := openRawMigrationDB(t)

	var applying, applied []int
	var beginFrom, beginTo, doneTo, beginCalls, doneCalls, failedCalls int
	var alreadyVer, alreadyHead, alreadyCalls int
	SetMigrationReporter(&MigrationReporter{
		AlreadyCurrent: func(v, head int) { alreadyVer, alreadyHead = v, head; alreadyCalls++ },
		Begin:          func(from, to int) { beginFrom, beginTo = from, to; beginCalls++ },
		Applying:       func(v int) { applying = append(applying, v) },
		Applied:        func(v int) { applied = append(applied, v) },
		Failed:         func(int, error) { failedCalls++ },
		Done:           func(to int) { doneTo = to; doneCalls++ },
	})
	t.Cleanup(func() { SetMigrationReporter(nil) })

	require.NoError(t, migrate(d))

	var want []int
	for v := 2; v <= currentVersion; v++ {
		want = append(want, v)
	}
	assert.Equal(t, 0, beginFrom, "fresh DB reports starting version 0")
	assert.Equal(t, currentVersion, beginTo)
	assert.Equal(t, currentVersion, doneTo)
	assert.Equal(t, 1, beginCalls, "Begin fires once")
	assert.Equal(t, 1, doneCalls, "Done fires once")
	assert.Equal(t, 0, failedCalls, "no Failed on a clean run")
	assert.Equal(t, 0, alreadyCalls, "AlreadyCurrent stays silent when there is real work")
	assert.Equal(t, want, applying, "Applying fires per version, in order")
	assert.Equal(t, want, applied, "Applied fires per version, in order")

	// Second pass: DB is at head, so migrate() applies nothing and fires ONLY
	// AlreadyCurrent(head) — the no-op-restart signal — with every bookend
	// staying quiet.
	applying, applied = nil, nil
	beginCalls, doneCalls, alreadyCalls, alreadyVer, alreadyHead = 0, 0, 0, 0, 0
	require.NoError(t, migrate(d))
	assert.Empty(t, applying, "no migrations applied on a current DB")
	assert.Empty(t, applied)
	assert.Equal(t, 0, beginCalls, "Begin does not fire when there is no work")
	assert.Equal(t, 0, doneCalls, "Done does not fire when there is no work")
	assert.Equal(t, 1, alreadyCalls, "AlreadyCurrent fires once on a no-op restart")
	assert.Equal(t, currentVersion, alreadyVer, "AlreadyCurrent reports the DB's actual version")
	assert.Equal(t, currentVersion, alreadyHead, "AlreadyCurrent reports the binary's head version")

	// Sanity: the DB really did reach head.
	assert.Equal(t, currentVersion, schemaVersion(d))
}

// TestMigrate_ReporterAlreadyCurrentForPastHeadDB covers the pathological path
// where the DB's schema is NEWER than this binary (a newer tclaude wrote it,
// then an older one ran): migrate() applies nothing and fires AlreadyCurrent
// once, reporting the DB's ACTUAL version (> head) alongside the binary's head
// so the consumer can flag the anomaly instead of reassuring the operator.
func TestMigrate_ReporterAlreadyCurrentForPastHeadDB(t *testing.T) {
	d := openRawMigrationDB(t)
	// Drive the DB to head first (a real, fully-migrated DB), then bump its
	// recorded schema_version past what this binary knows.
	require.NoError(t, migrate(d))
	pastHead := currentVersion + 3
	_, err := d.Exec(`UPDATE schema_version SET version = ?`, pastHead)
	require.NoError(t, err)

	var applying []int
	var beginCalls, doneCalls, alreadyCalls, alreadyVer, alreadyHead int
	SetMigrationReporter(&MigrationReporter{
		AlreadyCurrent: func(v, head int) { alreadyVer, alreadyHead = v, head; alreadyCalls++ },
		Begin:          func(int, int) { beginCalls++ },
		Applying:       func(v int) { applying = append(applying, v) },
		Done:           func(int) { doneCalls++ },
	})
	t.Cleanup(func() { SetMigrationReporter(nil) })

	require.NoError(t, migrate(d), "a DB past head is a no-op, not an error")
	assert.Empty(t, applying, "no migrations applied when the DB is past head")
	assert.Equal(t, 0, beginCalls, "Begin does not fire for a past-head DB")
	assert.Equal(t, 0, doneCalls, "Done does not fire for a past-head DB")
	assert.Equal(t, 1, alreadyCalls, "AlreadyCurrent fires once for a past-head DB")
	assert.Equal(t, pastHead, alreadyVer, "AlreadyCurrent reports the DB's actual (past-head) version")
	assert.Equal(t, currentVersion, alreadyHead, "AlreadyCurrent reports the binary's head version")
	// The migration must not have rewound the DB's version.
	assert.Equal(t, pastHead, schemaVersion(d))
}

// TestMigrate_NilReporterIsSilentAndSucceeds guards the CLI default: with no
// reporter installed migrate() must still drive the DB to head without
// panicking on any of the nil-safe report* helpers.
func TestMigrate_NilReporterIsSilentAndSucceeds(t *testing.T) {
	SetMigrationReporter(nil)
	d := openRawMigrationDB(t)
	require.NoError(t, migrate(d))
	assert.Equal(t, currentVersion, schemaVersion(d))
}

// TestMigrate_ReporterFailedFiresAndAborts swaps in a chain whose second step
// fails, then asserts migrate() reports the failing version, returns its
// error, and does NOT report Done (the chain aborted).
func TestMigrate_ReporterFailedFiresAndAborts(t *testing.T) {
	boom := errors.New("boom")
	orig := migrationSteps
	migrationSteps = []migrationStep{
		{2, func(db *sql.DB) error {
			_, err := db.Exec(`UPDATE schema_version SET version = 2`)
			return err
		}},
		{3, func(*sql.DB) error { return boom }},
	}
	t.Cleanup(func() { migrationSteps = orig })

	d := openRawMigrationDB(t)

	var appliedOK, failedVer, doneCalls int
	var failedErr error
	SetMigrationReporter(&MigrationReporter{
		Applied: func(int) { appliedOK++ },
		Failed:  func(v int, err error) { failedVer, failedErr = v, err },
		Done:    func(int) { doneCalls++ },
	})
	t.Cleanup(func() { SetMigrationReporter(nil) })

	err := migrate(d)
	require.ErrorIs(t, err, boom, "migrate() propagates the failing migration's error")
	assert.Equal(t, 1, appliedOK, "the one migration before the failure reported Applied")
	assert.Equal(t, 3, failedVer, "Failed names the failing version")
	require.ErrorIs(t, failedErr, boom)
	assert.Equal(t, 0, doneCalls, "Done never fires when the chain aborts")
	// The DB is left at the last version that actually committed.
	assert.Equal(t, 2, schemaVersion(d))
}
