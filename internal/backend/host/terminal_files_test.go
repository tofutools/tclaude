package host

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

type filePermit struct {
	execution model.ExecutionID
	operation model.OperationID
	consume   func() error
}

func (p filePermit) ExecutionID() model.ExecutionID { return p.execution }
func (p filePermit) OperationID() model.OperationID { return p.operation }
func (p filePermit) Consume(context.Context) error  { return p.consume() }
func TestTerminalFileStagePublishesOnlyAfterPermitAndNeverOverwrites(t *testing.T) {
	root := t.TempDir()
	ctx := context.Background()
	calls := 0
	in := ports.StageTerminalFileRequest{ExecutionID: "execution", OperationID: "operation", Filename: "picture.PNG", Content: []byte("supplied content")}
	in.Permit = filePermit{execution: in.ExecutionID, operation: in.OperationID, consume: func() error {
		calls++
		_, err := os.Stat(filepath.Join(root, "uploads", "execution-operation.png"))
		require.ErrorIs(t, err, os.ErrNotExist)
		return errors.New("revoked")
	}}
	result, err := StageTerminalFile(ctx, root, in.ExecutionID, in)
	require.Error(t, err)
	require.Equal(t, ports.EffectRefused, result.Disposition)
	require.Equal(t, 1, calls)
	entries, err := os.ReadDir(filepath.Join(root, "uploads"))
	require.NoError(t, err)
	require.Empty(t, entries)
	in.Permit = filePermit{execution: in.ExecutionID, operation: in.OperationID, consume: func() error { calls++; return nil }}
	result, err = StageTerminalFile(ctx, root, in.ExecutionID, in)
	require.NoError(t, err)
	require.Equal(t, ports.EffectAccepted, result.Disposition)
	data, err := os.ReadFile(result.NativePath)
	require.NoError(t, err)
	require.Equal(t, in.Content, data)
	info, err := os.Stat(result.NativePath)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0600), info.Mode().Perm())
	in.Content = []byte("replacement")
	denied, err := StageTerminalFile(ctx, root, in.ExecutionID, in)
	require.ErrorIs(t, err, os.ErrExist)
	require.Equal(t, ports.EffectRefused, denied.Disposition)
	data, err = os.ReadFile(result.NativePath)
	require.NoError(t, err)
	require.Equal(t, "supplied content", string(data))
	entries, err = os.ReadDir(filepath.Join(root, "uploads"))
	require.NoError(t, err)
	require.Len(t, entries, 1)
}
func TestTerminalFileStageRejectsSymlinkRootAndWrongExecution(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	require.NoError(t, os.Symlink(outside, filepath.Join(root, "uploads")))
	consumed := false
	in := ports.StageTerminalFileRequest{ExecutionID: "execution", OperationID: "operation", Filename: "a.txt", Content: []byte("a"), Permit: filePermit{execution: "execution", operation: "operation", consume: func() error { consumed = true; return nil }}}
	result, err := StageTerminalFile(context.Background(), root, in.ExecutionID, in)
	require.Error(t, err)
	require.Equal(t, ports.EffectRefused, result.Disposition)
	require.False(t, consumed)
	entries, err := os.ReadDir(outside)
	require.NoError(t, err)
	require.Empty(t, entries)
	_, err = StageTerminalFile(context.Background(), t.TempDir(), "another", in)
	require.Error(t, err)
	require.False(t, consumed)
}
