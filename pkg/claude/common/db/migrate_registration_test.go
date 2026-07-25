package db

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// migrateFuncPattern matches a migration's declaration and its two version
// numbers: `func migrateV155toV156(db *sql.DB) error {`.
var migrateFuncPattern = regexp.MustCompile(`(?m)^func (migrateV(\d+)toV(\d+))\(`)

// TestEveryMigrationIsRegistered pins the invariant TestMigrationStepsAreContiguous
// cannot see: every migrateV{n-1}toV{n} function that EXISTS in this package is
// actually wired into migrationSteps.
//
// The contiguity test checks the chain against itself, so a chain that is
// internally consistent passes even when a migration has fallen out of it
// entirely. That is not hypothetical — it nearly shipped. Two branches each
// appended a migration and each wrote the textually identical registration line
// `{156, migrateV155toV156},`; git merged the two lines into one with no
// conflict, and the renamed function from the second branch was left defined,
// compiled, tested in isolation, and never called. The chain read 2..156 with
// currentVersion 156: contiguous, gap-free, and wrong. An orphaned migration
// function is the one signature that bug leaves behind, so that is what this
// looks for.
//
// It reads the package's own source rather than a hand-maintained list, so a new
// migration is covered the moment it is written.
func TestEveryMigrationIsRegistered(t *testing.T) {
	registered := map[string]int{}
	for _, step := range migrationSteps {
		name := runtime.FuncForPC(reflect.ValueOf(step.apply).Pointer()).Name()
		// runtime reports the fully-qualified name; keep the bare func name.
		if idx := strings.LastIndex(name, "."); idx >= 0 {
			name = name[idx+1:]
		}
		registered[name] = step.version
	}

	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	found := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Join(".", entry.Name()))
		require.NoError(t, err)
		for _, m := range migrateFuncPattern.FindAllStringSubmatch(string(src), -1) {
			name, from, to := m[1], m[2], m[3]
			found++
			version, ok := registered[name]
			assert.Truef(t, ok,
				"%s is defined in %s but never registered in migrationSteps — it will never run. "+
					"Add {%s, %s} to the chain (this is what a silently-merged duplicate registration line looks like).",
				name, entry.Name(), to, name)
			if ok {
				assert.Equalf(t, to, strconv.Itoa(version),
					"%s is registered at version %d but its name says it advances to v%s", name, version, to)
				assert.Equalf(t, from, strconv.Itoa(version-1),
					"%s is registered at version %d, so it must advance FROM v%d, not v%s",
					name, version, version-1, from)
			}
		}
	}
	assert.Equal(t, len(migrationSteps), found,
		"every registered step should correspond to exactly one migrateVxxxtoVyyy declaration in this package")
}
