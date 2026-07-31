package filefollow

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTailInitialOffsetAlignsWithinBoundedWindow(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tail.jsonl")
	prefix := bytes.Repeat([]byte{'x'}, 256)
	content := append(prefix, '\n')
	content = append(content, []byte("first-tail\nsecond-tail\n")...)
	require.NoError(t, os.WriteFile(path, content, 0o600))

	file, err := os.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, file.Close()) })
	info, err := file.Stat()
	require.NoError(t, err)
	offset, err := TailInitialOffset(32)(file, info)
	require.NoError(t, err)
	_, err = file.Seek(offset, io.SeekStart)
	require.NoError(t, err)
	remaining, err := io.ReadAll(file)
	require.NoError(t, err)

	assert.Equal(t, int64(len(prefix)+1), offset)
	assert.Equal(t, "first-tail\nsecond-tail\n", string(remaining))
}

func TestTailInitialOffsetReturnsEOFWhenWindowContainsNoBoundary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "one-record.jsonl")
	require.NoError(t, os.WriteFile(path, bytes.Repeat([]byte{'x'}, 1024), 0o600))
	file, err := os.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, file.Close()) })
	info, err := file.Stat()
	require.NoError(t, err)

	offset, err := TailInitialOffset(64)(file, info)
	require.NoError(t, err)
	assert.Equal(t, info.Size(), offset)
}

type testFold struct {
	Values    []string
	Oversized int
}

func newTestFollower(maxRecord int) *Follower[testFold] {
	return New(Config[testFold]{
		NewState: func(string, int64) testFold { return testFold{} },
		CloneState: func(state testFold) testFold {
			state.Values = slices.Clone(state.Values)
			return state
		},
		Scan: func(r io.Reader, _ string, state *testFold, strict bool) (int64, bool, error) {
			return ScanLines(r, LineConfig{MaxRecordBytes: maxRecord}, func(line Line) bool {
				if line.Oversized {
					state.Oversized++
					return true
				}
				var value string
				if json.Unmarshal(line.Data, &value) != nil {
					return false
				}
				state.Values = append(state.Values, value)
				return true
			}, strict)
		},
	})
}

func writeRecords(t *testing.T, path string, records ...string) {
	t.Helper()
	var data []byte
	for _, record := range records {
		encoded, err := json.Marshal(record)
		require.NoError(t, err)
		data = append(data, encoded...)
		data = append(data, '\n')
	}
	require.NoError(t, os.WriteFile(path, data, 0o600))
}

func appendRaw(t *testing.T, path, raw string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = f.WriteString(raw)
	require.NoError(t, err)
	require.NoError(t, f.Close())
}

func TestFollowerUnchangedAndAppendReadOnlyNewPayload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	writeRecords(t, path, "one", "two")
	follower := newTestFollower(1024)

	info, err := os.Stat(path)
	require.NoError(t, err)
	first, err := follower.RefreshWithInfo(path, info)
	require.NoError(t, err)
	assert.True(t, first.Rebuilt)
	assert.Equal(t, []string{"one", "two"}, first.State.Values)

	unchanged, err := follower.Refresh(path)
	require.NoError(t, err)
	assert.True(t, unchanged.Unchanged)
	assert.Zero(t, unchanged.ReadBytes)

	appendBytes := "\"three\"\n"
	appendRaw(t, path, appendBytes)
	appended, err := follower.Refresh(path)
	require.NoError(t, err)
	assert.False(t, appended.Rebuilt)
	assert.Equal(t, int64(len(appendBytes)), appended.ReadBytes)
	assert.Equal(t, []string{"one", "two", "three"}, appended.State.Values)
	assert.Equal(t, Stats{
		Refreshes:    3,
		Unchanged:    1,
		Rebuilds:     1,
		Appends:      1,
		PayloadBytes: first.ReadBytes + int64(len(appendBytes)),
	}, follower.Stats())
}

func TestFollowerRetriesIncompleteTailAfterNewline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	writeRecords(t, path, "one")
	appendRaw(t, path, "\"part")
	follower := newTestFollower(1024)

	first, err := follower.Refresh(path)
	require.NoError(t, err)
	assert.Equal(t, []string{"one"}, first.State.Values)
	cursor, ok := follower.Checkpoint()
	require.True(t, ok)
	assert.Equal(t, int64(len("\"one\"\n")), cursor.Offset)

	appendRaw(t, path, "ial\"\n")
	second, err := follower.Refresh(path)
	require.NoError(t, err)
	assert.Equal(t, []string{"one", "partial"}, second.State.Values)
	assert.Equal(t, int64(len("\"partial\"\n")), second.ReadBytes)
}

func TestFollowerRebuildsWhenUncommittedTailShrinks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	writeRecords(t, path, "one")
	appendRaw(t, path, "\"partial")
	follower := newTestFollower(1024)
	_, err := follower.Refresh(path)
	require.NoError(t, err)
	cursor, ok := follower.Checkpoint()
	require.True(t, ok)
	require.Greater(t, cursor.FileSize, cursor.Offset)

	require.NoError(t, os.Truncate(path, cursor.FileSize-1))
	result, err := follower.Refresh(path)
	require.NoError(t, err)
	assert.True(t, result.Rebuilt, "T+1 size smaller than T invalidates the generation")
	assert.Equal(t, []string{"one"}, result.State.Values)
}

func TestFollowerRebuildsOnShrinkAndReplacement(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	writeRecords(t, path, "one", "two")
	follower := newTestFollower(1024)
	_, err := follower.Refresh(path)
	require.NoError(t, err)

	writeRecords(t, path, "short")
	shrunk, err := follower.Refresh(path)
	require.NoError(t, err)
	assert.True(t, shrunk.Rebuilt)
	assert.Equal(t, []string{"short"}, shrunk.State.Values)

	replacement := filepath.Join(dir, "replacement")
	writeRecords(t, replacement, "replacement", "larger")
	require.NoError(t, os.Rename(replacement, path))
	replaced, err := follower.Refresh(path)
	require.NoError(t, err)
	assert.True(t, replaced.Rebuilt)
	assert.Equal(t, []string{"replacement", "larger"}, replaced.State.Values)
}

func TestFollowerRebuildsOnSameSizeRewriteAndAnchorMismatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	writeRecords(t, path, "aaaa", "bbbb")
	follower := newTestFollower(1024)
	_, err := follower.Refresh(path)
	require.NoError(t, err)

	writeRecords(t, path, "cccc", "dddd")
	future := time.Now().Add(2 * time.Second)
	require.NoError(t, os.Chtimes(path, future, future))
	rewritten, err := follower.Refresh(path)
	require.NoError(t, err)
	assert.True(t, rewritten.Rebuilt)
	assert.Equal(t, []string{"cccc", "dddd"}, rewritten.State.Values)

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	data[len(data)-3] = 'x'
	require.NoError(t, os.WriteFile(path, data, 0o600))
	appendRaw(t, path, "\"tail\"\n")
	anchored, err := follower.Refresh(path)
	require.NoError(t, err)
	assert.True(t, anchored.Rebuilt)
	assert.Equal(t, []string{"cccc", "dddx", "tail"}, anchored.State.Values)
}

func TestFollowerCheckpointRestoreAndMalformedAppend(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	writeRecords(t, path, "one")
	firstFollower := newTestFollower(1024)
	first, err := firstFollower.Refresh(path)
	require.NoError(t, err)
	cursor, ok := firstFollower.Checkpoint()
	require.True(t, ok)

	restored := newTestFollower(1024)
	require.NoError(t, restored.Restore(cursor, first.State))
	unchanged, err := restored.Refresh(path)
	require.NoError(t, err)
	assert.True(t, unchanged.Unchanged)
	assert.Zero(t, unchanged.ReadBytes)

	appendRaw(t, path, "not-json\n\"two\"\n")
	result, err := restored.Refresh(path)
	require.NoError(t, err)
	assert.True(t, result.Rebuilt, "decode doubt must discard the append fold and rebuild")
	assert.Equal(t, []string{"one", "two"}, result.State.Values)
}

func TestScanLinesBoundsOversizedRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	require.NoError(t, os.WriteFile(path, []byte("\"this record is oversized\"\n\"ok\"\n"), 0o600))
	follower := newTestFollower(8)

	result, err := follower.Refresh(path)
	require.NoError(t, err)
	assert.Equal(t, 1, result.State.Oversized)
	assert.Equal(t, []string{"ok"}, result.State.Values)
}

func TestFollowerRetriesWhenPathIsReplacedDuringScan(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	replacement := filepath.Join(dir, "replacement.jsonl")
	writeRecords(t, path, "old")
	writeRecords(t, replacement, "new", "generation")

	var replaceOnce sync.Once
	follower := New(Config[testFold]{
		NewState:   func(string, int64) testFold { return testFold{} },
		CloneState: func(state testFold) testFold { return state },
		Scan: func(r io.Reader, _ string, state *testFold, strict bool) (int64, bool, error) {
			consumed, doubt, err := ScanLines(r, LineConfig{MaxRecordBytes: 1024}, func(line Line) bool {
				var value string
				if json.Unmarshal(line.Data, &value) != nil {
					return false
				}
				state.Values = append(state.Values, value)
				return true
			}, strict)
			replaceOnce.Do(func() { require.NoError(t, os.Rename(replacement, path)) })
			return consumed, doubt, err
		},
	})

	result, err := follower.Refresh(path)
	require.NoError(t, err)
	assert.True(t, result.Rebuilt)
	assert.Equal(t, []string{"new", "generation"}, result.State.Values)
}

func TestFollowerConsumesGrowthThatRacesEOF(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	writeRecords(t, path, "one")
	appendBytes := "\"two\"\n"

	var appendOnce sync.Once
	follower := New(Config[testFold]{
		NewState: func(string, int64) testFold { return testFold{} },
		CloneState: func(state testFold) testFold {
			state.Values = slices.Clone(state.Values)
			return state
		},
		Scan: func(r io.Reader, scanPath string, state *testFold, strict bool) (int64, bool, error) {
			consumed, doubt, err := ScanLines(r, LineConfig{MaxRecordBytes: 1024}, func(line Line) bool {
				var value string
				if json.Unmarshal(line.Data, &value) != nil {
					return false
				}
				state.Values = append(state.Values, value)
				return true
			}, strict)
			appendOnce.Do(func() { appendRaw(t, scanPath, appendBytes) })
			return consumed, doubt, err
		},
	})

	result, err := follower.Refresh(path)
	require.NoError(t, err)
	assert.Equal(t, []string{"one", "two"}, result.State.Values)
	assert.Equal(t, int64(len("\"one\"\n")+len(appendBytes)), result.ReadBytes)
	cursor, ok := follower.Checkpoint()
	require.True(t, ok)
	assert.Equal(t, cursor.FileSize, cursor.Offset)
}

func TestFollowerRetriesWhenFileShrinksDuringScan(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	writeRecords(t, path, "old", "records")

	var shrinkOnce sync.Once
	follower := New(Config[testFold]{
		NewState:   func(string, int64) testFold { return testFold{} },
		CloneState: func(state testFold) testFold { return state },
		Scan: func(r io.Reader, scanPath string, state *testFold, strict bool) (int64, bool, error) {
			consumed, doubt, err := ScanLines(r, LineConfig{MaxRecordBytes: 1024}, func(line Line) bool {
				var value string
				if json.Unmarshal(line.Data, &value) != nil {
					return false
				}
				state.Values = append(state.Values, value)
				return true
			}, strict)
			shrinkOnce.Do(func() { writeRecords(t, scanPath, "new") })
			return consumed, doubt, err
		},
	})

	result, err := follower.Refresh(path)
	require.NoError(t, err)
	assert.True(t, result.Rebuilt)
	assert.Equal(t, []string{"new"}, result.State.Values)
}
