package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestConvIndex_GitBranchStartupRoundtrip locks in the schema-v32
// git_branch_startup column: it is written by UpsertConvIndex and read
// back by GetConvIndex. Being the immutable launch branch, it survives
// a rescan that moves the last-wins git_branch forward — the two
// columns answer "where did it start" vs "where is it now". (That the
// column exists at all also proves migrateV31toV32 applied — an
// unmigrated DB fails the first Upsert.)
func TestConvIndex_GitBranchStartupRoundtrip(t *testing.T) {
	setupTestDB(t)

	convID := "22222222-aaaa-bbbb-cccc-222222222222"
	require.NoError(t, UpsertConvIndex(&ConvIndexRow{
		ConvID:           convID,
		ProjectDir:       "/tmp/proj",
		FullPath:         "/tmp/proj/" + convID + ".jsonl",
		CustomTitle:      "worker",
		GitBranch:        "main",
		GitBranchStartup: "main",
		IndexedAt:        time.Now(),
	}), "UpsertConvIndex (initial)")

	row, err := GetConvIndex(convID)
	require.NoError(t, err)
	require.NotNil(t, row)
	assert.Equal(t, "main", row.GitBranch, "current branch")
	assert.Equal(t, "main", row.GitBranchStartup, "startup branch")

	// A later rescan: the agent ran `git checkout -b feature-x`, so the
	// last-wins git_branch moves forward — but git_branch_startup, the
	// branch the first turn was stamped with, must NOT.
	require.NoError(t, UpsertConvIndex(&ConvIndexRow{
		ConvID:           convID,
		ProjectDir:       "/tmp/proj",
		FullPath:         "/tmp/proj/" + convID + ".jsonl",
		CustomTitle:      "worker",
		GitBranch:        "feature-x",
		GitBranchStartup: "main",
		IndexedAt:        time.Now(),
	}), "UpsertConvIndex (rescan after branch switch)")

	row, err = GetConvIndex(convID)
	require.NoError(t, err)
	require.NotNil(t, row)
	assert.Equal(t, "feature-x", row.GitBranch, "git_branch tracks the current branch")
	assert.Equal(t, "main", row.GitBranchStartup, "git_branch_startup is the immutable launch branch")

	// The list surfaces carry the column too.
	all, err := ListAllConvIndex()
	require.NoError(t, err)
	var found *ConvIndexRow
	for _, r := range all {
		if r.ConvID == convID {
			found = r
		}
	}
	require.NotNil(t, found, "conv missing from ListAllConvIndex")
	assert.Equal(t, "main", found.GitBranchStartup, "ListAllConvIndex carries git_branch_startup")
}
