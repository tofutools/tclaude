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

// TestEveryMigrationFileIsRegisteredExactlyOnce is the guard for the failure
// that took main down on 2026-08-08, and for the quieter one underneath it.
//
// # What happened
//
// Two merged PRs each claimed migration v195, in files neither PR shared:
// migrate_v195_reset_copilot_usage_snapshots.go and
// migrate_v195_permission_scopes.go. Git reported no conflict, because neither
// branch touched the other's file. Both were green before either merged.
//
// The loud defect was a compile error from two functions named
// migrateV194toV195, and it is the only reason anyone looked.
//
// THE QUIET DEFECT IS THE ONE THIS TEST IS FOR. The registration list carried a
// SINGLE {195, ...} entry for two migrations. Even with the name collision
// resolved, one of the two schema changes would never have run — silently, on a
// user's database, with no error anywhere. [TestMigrationStepsAreContiguous]
// does not see it: the list is still dense, still strictly increasing, and still
// ends at currentVersion, because the problem is not a gap in the list but a
// migration that never reached it.
//
// # Why the file names are the subject
//
// Because the list cannot be its own witness. Asking "is every entry in the list
// present" is answered by the list. The population that must be accounted for
// lives on disk — one file per version, by a convention this package has
// followed for 124 versions — so the file set is the independent evidence, and
// the question is whether the list covers it.
//
// # What it asserts
//
//   - NO TWO FILES CLAIM THE SAME VERSION. This is the incident, exactly. Two
//     files numbered v195 means one of them is not in the chain, whatever the
//     list looks like.
//   - Every file's version is registered. Catches a migration added at a version
//     the list has not been extended to — the forgotten-bump shape.
//   - A file numbered N declares migrateV{N-1}toV{N}. Without this a file could
//     claim a free number while its function implements another, and both checks
//     above would pass while the migration ran at the wrong point in the chain.
//
// # What it cannot see
//
//   - A migration defined outside the naming convention. Versions below v73 pre-
//     date it and several share files, so the file→list direction is the only one
//     asserted; the reverse would fail on history rather than on a defect.
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
			"v%d is claimed by %d files (%v). Two migrations at one version means "+
				"the registration list can hold only one of them, so the other never "+
				"runs — silently, on a real database. This is the 2026-08-08 incident "+
				"exactly: git reports no conflict because neither branch touches the "+
				"other's file. The later-merged change renumbers",
			version, len(files), files)

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
// what its name says. A file named v198 whose function is migrateV196toV197
// would satisfy the first and still be wrong — it would run at the wrong point,
// or twice, depending on what else moved.
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
