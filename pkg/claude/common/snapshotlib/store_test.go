package snapshotlib

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/testutil"
)

func content(s string) func(io.Writer) error {
	return func(w io.Writer) error { _, err := io.WriteString(w, s); return err }
}

func TestNamesAreStrict(t *testing.T) {
	for _, ok := range []string{"a", "reviewer-1", "v1.2_final", "0abc"} {
		assert.True(t, ValidName(ok), ok)
	}
	for _, bad := range []string{"", ".", "..", "../x", "a/b", `a\b`, ".hidden", "-x", "Upper", "a b", "x\x00", string(make([]byte, 65)), "a" + string(rune(0x2215)) + "b", tmpPrefix + "x"} {
		assert.False(t, ValidName(bad), "%q", bad)
	}
	s := New(testutil.CanonicalTempDir(t))
	_, err := s.Create(Meta{Name: "../escape"}, content("x"))
	require.Error(t, err)
	_, _, err = s.Get("../escape")
	require.Error(t, err)
	require.Error(t, s.Remove(".."))
}

func TestCreateListGetRemove(t *testing.T) {
	dir := filepath.Join(testutil.CanonicalTempDir(t), "snapshots")
	s := New(dir)
	m, err := s.Create(Meta{Name: "one", AgentName: "reviewer", Harness: "claude", CreatedAt: time.Now()}, content("paf bytes"))
	require.NoError(t, err)
	assert.Equal(t, int64(9), m.Bytes)
	_, err = s.Create(Meta{Name: "one"}, content("again"))
	require.ErrorIs(t, err, ErrExists)

	st, err := os.Stat(dir)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), st.Mode().Perm())
	got, path, err := s.Get("one")
	require.NoError(t, err)
	assert.Equal(t, "reviewer", got.AgentName)
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "paf bytes", string(raw))
	fi, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm())

	list, err := s.List()
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.NoError(t, s.Remove("one"))
	require.ErrorIs(t, s.Remove("one"), ErrNotFound)
	_, _, err = s.Get("one")
	require.ErrorIs(t, err, ErrNotFound)
}

// A failed or in-progress write never shows up.
func TestHalfWrittenSnapshotIsInvisible(t *testing.T) {
	s := New(testutil.CanonicalTempDir(t))
	started, release := make(chan struct{}), make(chan struct{})
	done := make(chan error)
	go func() {
		_, err := s.Create(Meta{Name: "slow", CreatedAt: time.Now()}, func(w io.Writer) error {
			_, _ = io.WriteString(w, "partial")
			close(started)
			<-release
			return errors.New("capture failed")
		})
		done <- err
	}()
	<-started
	list, err := s.List()
	require.NoError(t, err)
	assert.Empty(t, list, "a snapshot being written is not listed")
	_, _, err = s.Get("slow")
	assert.ErrorIs(t, err, ErrNotFound)
	close(release)
	require.Error(t, <-done)
	list, err = s.List()
	require.NoError(t, err)
	assert.Empty(t, list)
	entries, err := os.ReadDir(s.Dir())
	require.NoError(t, err)
	assert.Empty(t, entries, "the temporary directory is cleaned up")
}

func TestPruneRemovesExpiredOnly(t *testing.T) {
	s := New(testutil.CanonicalTempDir(t))
	now := time.Now()
	past, future := now.Add(-time.Minute), now.Add(time.Hour)
	_, err := s.Create(Meta{Name: "old", CreatedAt: now, ExpiresAt: &past}, content("x"))
	require.NoError(t, err)
	_, err = s.Create(Meta{Name: "new", CreatedAt: now, ExpiresAt: &future}, content("x"))
	require.NoError(t, err)
	_, err = s.Create(Meta{Name: "keep", CreatedAt: now}, content("x"))
	require.NoError(t, err)
	// A stale leftover from an interrupted create is swept.
	stale := filepath.Join(s.Dir(), tmpPrefix+"dead")
	require.NoError(t, os.Mkdir(stale, 0o700))
	require.NoError(t, os.Chtimes(stale, now.Add(-2*time.Hour), now.Add(-2*time.Hour)))

	removed, err := s.Prune(now)
	require.NoError(t, err)
	assert.Equal(t, []string{"old"}, removed)
	list, err := s.List()
	require.NoError(t, err)
	assert.Len(t, list, 2)
	assert.NoDirExists(t, stale)
}

// Prune running alongside creates never removes or exposes a snapshot
// that is still being written.
func TestPruneDoesNotRaceCreate(t *testing.T) {
	s := New(testutil.CanonicalTempDir(t))
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, err := s.Create(Meta{Name: fmt.Sprintf("s%d", i), CreatedAt: time.Now()}, content("bytes"))
			assert.NoError(t, err)
		}()
		go func() {
			defer wg.Done()
			_, err := s.Prune(time.Now())
			assert.NoError(t, err)
			list, err := s.List()
			assert.NoError(t, err)
			for _, m := range list {
				assert.Equal(t, int64(5), m.Bytes, "only complete snapshots are listed")
			}
		}()
	}
	wg.Wait()
	list, err := s.List()
	require.NoError(t, err)
	assert.Len(t, list, 20)
}
