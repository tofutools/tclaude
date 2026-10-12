package db

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestManagedInstructionStoreOverwriteAndIntegrity(t *testing.T) {
	setupTestDB(t)
	digest, err := PutManagedInstruction(0, "rule", "safety", []byte("keep operator constraints"), false)
	require.NoError(t, err)
	_, err = PutManagedInstruction(0, "rule", "safety", []byte("replacement"), false)
	require.Error(t, err)
	rows, err := ListManagedInstructions(0, "rule")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, digest, rows[0].Digest)
	_, err = PutManagedInstruction(0, "rule", "safety", []byte("replacement"), true)
	require.NoError(t, err)
	rows, err = ListManagedInstructions(0, "rule")
	require.NoError(t, err)
	require.Equal(t, []byte("replacement"), rows[0].Content)
	d, err := Open()
	require.NoError(t, err)
	_, err = d.Exec(`UPDATE managed_instruction_blobs SET content=? WHERE digest=?`, []byte("tampered"), rows[0].Digest)
	require.NoError(t, err)
	_, err = ListManagedInstructions(0, "rule")
	require.ErrorContains(t, err, "digest mismatch")
	_, err = PutManagedInstruction(0, "skill", "../escape", []byte("x"), false)
	require.Error(t, err)
	_, err = PutManagedInstruction(0, "rule", "big", make([]byte, ManagedInstructionItemLimit+1), false)
	require.Error(t, err)
	removed, err := DeleteManagedInstruction(0, "rule", "safety")
	require.NoError(t, err)
	require.True(t, removed)
	rows, err = ListManagedInstructions(0, "rule")
	require.NoError(t, err)
	require.Empty(t, rows)
}
func TestManagedInstructionLaunchSnapshotFrozen(t *testing.T) {
	setupTestDB(t)
	require.NoError(t, SaveManagedInstructionLaunch(ManagedInstructionLaunch{AgentID: "agent", Body: "original rules", Resources: "[]", Root: "/private/launch"}))
	_, err := PutManagedInstruction(0, "rule", "new", []byte("new rules"), false)
	require.NoError(t, err)
	snapshot, err := GetManagedInstructionLaunch("agent")
	require.NoError(t, err)
	require.Equal(t, "original rules", snapshot.Body)
}
