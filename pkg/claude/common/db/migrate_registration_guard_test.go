package db

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// migrationFilePattern matches the one file-naming convention this package uses
// for a schema migration: migrate_v<version>_<what_it_does>.go.
var migrationFilePattern = regexp.MustCompile(`^migrate_v(\d+)_.*\.go$`)

// TestEveryMigrationFileIsRegisteredExactlyOnce keeps migration FILE NAMES
// honest, so that the version a PR appears to claim is the version it claims.
//
// # Read this before trusting it: what it is NOT
//
// It is not the guard against a migration silently failing to run.
// [TestEveryMigrationIsRegistered] is, and it got there first — it scans this
// package for every migrateV{n-1}toV{n} DECLARATION and pins each to its own
// registered version, which is the check that actually tracks runtime behaviour.
// File names have no runtime effect whatsoever: chain position comes from
// migrationSteps plus the function's own version, and nothing in SQLite has ever
// read a Go file name.
//
// So this test can fail on a migration chain that is completely correct, and it
// is MEANT to. A file named migrate_v197_*.go that declares migrateV197toV198,
// registered at 198, runs perfectly and trips both guards here. That is the
// intended behaviour and not a false positive: renumbering a migration means
// renaming its file too, because the file name is what the next person reads.
//
// # What it is for
//
// On 2026-08-08 two merged PRs each claimed v195, in files neither PR shared:
// migrate_v195_reset_copilot_usage_snapshots.go and
// migrate_v195_permission_scopes.go. Git reported no conflict, because neither
// branch touched the other's file. Both were green before either merged, and
// main stopped compiling on two functions named migrateV194toV195.
//
// A compile error is the LOUDEST possible outcome and it is the only reason
// anyone looked. The failure mode this guards is the same collision noticed
// EARLIER and by a person: two files bearing the same version number is the
// visible signature of the collision, readable in a PR's file list before any
// of it is merged, and it survives the two branches having renamed their
// functions apart. That is a reviewer-facing benefit, not a runtime one, and
// stating it as a runtime one — which an earlier draft of this comment did —
// misrepresents what a green run here proves.
//
// # Why the file set is the subject
//
// Because the list cannot be its own witness. Asking "is every entry in the list
// present" is answered by the list. The file set is evidence that exists
// independently of it, so the question worth asking is whether the list covers
// the files.
//
// # What it asserts
//
//   - NO TWO FILES CLAIM THE SAME VERSION — the readable signature of the
//     2026-08-08 collision.
//   - Every file's version appears exactly once in migrationSteps. Given
//     contiguity this is largely subsumed by [TestEveryMigrationIsRegistered];
//     it is kept as a cheap backstop, not because it is load-bearing.
//
// # What it cannot see
//
//   - A migration outside the naming convention — and this is a FORWARD gap, not
//     just a historical one. Nothing forces a new migration into the convention:
//     a migrate_v199.go with no trailing section does not match the pattern at
//     all, and two migrations declared in one conventionally-named file are
//     invisible because the second version has no file of its own. The
//     convention holds for 124 files spanning v73..v197, with v87 already an
//     exception inside that range (migrateV86toV87 lives in migrate.go). Only
//     the file→list direction is asserted; the reverse would fail on history.
//   - Two PRs in flight. This catches the collision on whichever PR merges
//     SECOND, and only if CI re-runs against the post-merge base. A check whose
//     subject is the repository is invalidated by a merge into its base, and no
//     test in the branch can fix that — it is a merge-queue question.
func TestEveryMigrationFileIsRegisteredExactlyOnce(t *testing.T) {
	entries, err := os.ReadDir(".")
	require.NoError(t, err)

	filesByVersion := map[int][]string{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || strings.HasSuffix(name, "_test.go") {
			continue
		}
		match := migrationFilePattern.FindStringSubmatch(name)
		if match == nil {
			continue
		}
		version, err := strconv.Atoi(match[1])
		require.NoErrorf(t, err, "parsing the version out of %s", name)
		filesByVersion[version] = append(filesByVersion[version], name)
	}

	// The positive control. Every assertion below is over a set built by
	// scanning, so a rename of the convention would empty it and leave this
	// passing vacuously about nothing.
	require.NotEmpty(t, filesByVersion,
		"no migrate_v<N>_*.go files found at all; either the naming convention "+
			"changed and this guard needs to change with it, or it is watching nothing")

	registered := map[int]int{}
	for _, step := range migrationSteps {
		registered[step.version]++
	}

	for version, files := range filesByVersion {
		assert.Lenf(t, files, 1,
			"v%d is claimed by %d files (%v). This is the readable signature of the "+
				"2026-08-08 collision: git reports no conflict, because neither branch "+
				"touches the other's file, so nothing before this says the two PRs picked "+
				"the same number. FIX: the change that merged SECOND takes the next free "+
				"version — rename its file to migrate_v<next>_<what_it_does>.go, rename "+
				"its function to migrateV<next-1>toV<next>, move its migrationSteps entry "+
				"to the end of the list, and bump currentVersion to match. Renaming the "+
				"function alone leaves two files bearing one number, which is what the "+
				"next person reading the PR has to go on",
			version, len(files), files)

		// Largely subsumed by TestEveryMigrationIsRegistered, which pins each
		// migration FUNCTION to its registered version. Kept as a cheap backstop
		// on a different population (files rather than declarations), not because
		// it is the load-bearing check here.
		assert.Equalf(t, 1, registered[version],
			"v%d has a migration file (%v) but appears %d times in migrationSteps. A "+
				"migration that is not registered exactly once does not run exactly "+
				"once, and nothing else reports it: the schema simply differs from what "+
				"the code expects, on whichever databases took that path",
			version, files, registered[version])
	}
}

// TestEveryMigrationFileDeclaresItsOwnStep ties a migration file's NAME to the
// function inside it, so a file cannot claim one version while implementing
// another.
//
// Separate from the registration check because it answers a different question.
// That one asks whether the chain covers every file; this asks whether a file is
// what its name says.
//
// As with its sibling, the stake is READABILITY AND NOT RUNTIME. A file named
// v198 whose function is migrateV196toV197 runs at whatever point the function's
// own version puts it, correctly, because the file name is inert —
// [TestEveryMigrationIsRegistered] already pins function name against registered
// version, and that is the pairing SQLite ends up caring about. What breaks is
// the reviewer: the file name is what a PR's file list shows, so a file whose
// name and function disagree makes a version collision unreadable to the person
// best placed to catch it before it merges.
//
// It cannot see a migration whose function name is right and whose BODY belongs
// to another version. Nothing structural can; that is what the migration's own
// test is for.
func TestEveryMigrationFileDeclaresItsOwnStep(t *testing.T) {
	entries, err := os.ReadDir(".")
	require.NoError(t, err)

	var checked int
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || strings.HasSuffix(name, "_test.go") {
			continue
		}
		match := migrationFilePattern.FindStringSubmatch(name)
		if match == nil {
			continue
		}
		version, err := strconv.Atoi(match[1])
		require.NoErrorf(t, err, "parsing the version out of %s", name)

		source, err := os.ReadFile(name)
		require.NoError(t, err)
		parsed, err := parser.ParseFile(token.NewFileSet(), name, source, 0)
		require.NoError(t, err)

		want := fmt.Sprintf("migrateV%dtoV%d", version-1, version)
		var declares bool
		for _, decl := range parsed.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == want {
				declares = true
			}
		}
		assert.Truef(t, declares,
			"%s is named for v%d but does not declare %s. The file name is what a "+
				"reviewer reads to see which version a PR claims, so a file whose name "+
				"and function disagree makes the collision this package's guards exist "+
				"to catch invisible to the person best placed to catch it",
			name, version, want)
		checked++
	}

	require.NotZero(t, checked, "no migration files were checked; this guard is watching nothing")
}
