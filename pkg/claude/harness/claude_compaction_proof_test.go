package harness

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClaudeCompactionProof_CompactedAfter(t *testing.T) {
	const boundary = `{"type":"system","subtype":"compact_boundary"}` + "\n"
	// A tool result quoting a boundary is transcript content, not a boundary.
	const quoted = `{"type":"user","message":"{\"type\":\"system\",\"subtype\":\"compact_boundary\"}"}` + "\n"
	path := filepath.Join(t.TempDir(), "conv.jsonl")
	proof := claudeCompactionProof{}

	require.NoError(t, os.WriteFile(path, []byte(boundary+quoted), 0o644))
	offset := int64(len(boundary))

	found, err := proof.CompactedAfter(path, offset)
	require.NoError(t, err)
	assert.False(t, found, "a boundary before the offset is an older compaction")

	require.NoError(t, os.WriteFile(path, []byte(boundary+quoted+`{"type":"system","subtype":"compact_boundary"}`), 0o644))
	found, err = proof.CompactedAfter(path, offset)
	require.NoError(t, err)
	assert.True(t, found, "a final line need not be newline-terminated")

	_, err = proof.CompactedAfter(path, 1<<30)
	assert.Error(t, err, "a transcript shorter than the offset cannot answer")

	_, err = proof.CompactedAfter(filepath.Join(t.TempDir(), "missing.jsonl"), 0)
	assert.Error(t, err)
}
