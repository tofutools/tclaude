package common

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestWriter opens a RotatingWriter on a fresh temp-dir log file and
// registers its Close so tests do not leak fds.
func newTestWriter(t *testing.T) (*RotatingWriter, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "output.log")
	rw, err := OpenRotatingWriter(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = rw.Close() })
	return rw, path
}

func writeString(t *testing.T, rw *RotatingWriter, s string) {
	t.Helper()
	n, err := rw.Write([]byte(s))
	require.NoError(t, err)
	require.Equal(t, len(s), n)
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(b)
}

func assertAbsent(t *testing.T, path string) {
	t.Helper()
	_, err := os.Stat(path)
	assert.Truef(t, os.IsNotExist(err), "expected %s to be absent, stat err: %v", path, err)
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	require.NoError(t, err)
	return info.Size()
}

// Past the configured max size, MaybeRotate moves the active log into
// slot .1 and leaves a fresh, empty active file behind.
func TestRotatingWriter_RotatesPastMaxSize(t *testing.T) {
	rw, path := newTestWriter(t)
	rw.Configure(100, 3)

	writeString(t, rw, string(bytes.Repeat([]byte("x"), 150)))
	require.NoError(t, rw.MaybeRotate())

	assert.EqualValues(t, 150, fileSize(t, rotatedPath(path, 1)),
		"the oversized log should have become output.log.1")
	assert.EqualValues(t, 0, fileSize(t, path),
		"a fresh, empty active log should exist at the original path")
}

// Below the max size, MaybeRotate is a no-op.
func TestRotatingWriter_NoRotateUnderMaxSize(t *testing.T) {
	rw, path := newTestWriter(t)
	rw.Configure(1000, 3)

	writeString(t, rw, "small")
	require.NoError(t, rw.MaybeRotate())

	assertAbsent(t, rotatedPath(path, 1))
	assert.EqualValues(t, len("small"), fileSize(t, path))
}

// Repeated rotations cascade oldest-to-newest: the most recent rotation
// is always .1, older ones shift up, and content is never clobbered.
func TestRotatingWriter_CascadeOrder(t *testing.T) {
	rw, path := newTestWriter(t)
	rw.Configure(1, 5) // maxSize irrelevant — rotate() is called directly

	writeString(t, rw, "first")
	require.NoError(t, rw.rotate())
	writeString(t, rw, "second")
	require.NoError(t, rw.rotate())
	writeString(t, rw, "third")
	require.NoError(t, rw.rotate())

	assert.Equal(t, "third", readFile(t, rotatedPath(path, 1)), ".1 holds the newest rotation")
	assert.Equal(t, "second", readFile(t, rotatedPath(path, 2)), ".2 holds the previous one")
	assert.Equal(t, "first", readFile(t, rotatedPath(path, 3)), ".3 holds the oldest")
	assert.EqualValues(t, 0, fileSize(t, path), "the active log is fresh after rotation")
}

// Once keep rotations have accumulated, the next rotation drops the
// oldest file rather than growing an unbounded tail.
func TestRotatingWriter_DropsOldestPastKeep(t *testing.T) {
	rw, path := newTestWriter(t)
	const keep = 2
	rw.Configure(1, keep)

	for _, marker := range []string{"A", "B", "C", "D"} {
		writeString(t, rw, marker)
		require.NoError(t, rw.rotate())
	}

	assert.Equal(t, "D", readFile(t, rotatedPath(path, 1)))
	assert.Equal(t, "C", readFile(t, rotatedPath(path, 2)))
	assertAbsent(t, rotatedPath(path, keep+1))
	assert.Equal(t, keep, countRotatedFiles(t, path),
		"no more than keep rotated files survive")
}

// countRotatedFiles counts output.log.N siblings of the active log.
func countRotatedFiles(t *testing.T, active string) int {
	t.Helper()
	n := 0
	for i := 1; ; i++ {
		if _, err := os.Stat(rotatedPath(active, i)); err != nil {
			break
		}
		n++
	}
	return n
}

// Every rotated file is a sibling of the active log — same directory,
// hence same filesystem, which is what makes each os.Rename atomic.
func TestRotatingWriter_RotatedFilesAreSiblings(t *testing.T) {
	rw, path := newTestWriter(t)
	rw.Configure(1, 3)

	writeString(t, rw, "data")
	require.NoError(t, rw.rotate())

	rotated := rotatedPath(path, 1)
	assert.Equal(t, filepath.Dir(path), filepath.Dir(rotated),
		"rotated file must live in the active log's directory")
	info, err := os.Stat(rotated)
	require.NoError(t, err)
	assert.True(t, info.Mode().IsRegular(), "rotated file must be a regular file")
}

// A log file that is already oversized when the writer opens it — the
// real-world first run of this feature on a long-lived install — is
// rotated cleanly on the first size check.
func TestRotatingWriter_ExistingOversizedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "output.log")
	require.NoError(t, os.WriteFile(path, bytes.Repeat([]byte("old"), 300), 0644)) // 900 bytes

	rw, err := OpenRotatingWriter(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = rw.Close() })
	rw.Configure(500, 3)

	require.NoError(t, rw.MaybeRotate())

	assert.EqualValues(t, 900, fileSize(t, rotatedPath(path, 1)),
		"the pre-existing oversized log is rotated whole into .1")
	assert.EqualValues(t, 0, fileSize(t, path))
}

// After a rotation the writer's fd is swapped to the fresh file, so
// subsequent writes land in the new active log — never in the
// rotated-away one. This is the crux of the in-process fd swap.
func TestRotatingWriter_WritesGoToNewFileAfterRotation(t *testing.T) {
	rw, path := newTestWriter(t)
	rw.Configure(1, 3)

	writeString(t, rw, "before-rotation\n")
	require.NoError(t, rw.rotate())
	writeString(t, rw, "after-rotation\n")

	assert.Equal(t, "before-rotation\n", readFile(t, rotatedPath(path, 1)))
	assert.Equal(t, "after-rotation\n", readFile(t, path),
		"post-rotation writes must reach the fresh active log via the swapped fd")
}

// maxSize 0 disables rotation entirely — MaybeRotate never rotates no
// matter how large the log grows. This is the state of every transient
// CLI process (it never calls Configure with a positive size).
func TestRotatingWriter_MaxSizeZeroDisablesRotation(t *testing.T) {
	rw, path := newTestWriter(t)
	rw.Configure(0, 3)

	writeString(t, rw, string(bytes.Repeat([]byte("x"), 5000)))
	require.NoError(t, rw.MaybeRotate())

	assertAbsent(t, rotatedPath(path, 1))
	assert.EqualValues(t, 5000, fileSize(t, path), "the log keeps growing when rotation is off")
}

// If the active log is deleted out from under the writer, MaybeRotate
// reopens it by path so logging keeps reaching a visible file.
func TestRotatingWriter_ReopensVanishedFile(t *testing.T) {
	rw, path := newTestWriter(t)
	rw.Configure(100, 3)

	require.NoError(t, os.Remove(path))
	require.NoError(t, rw.MaybeRotate(), "a vanished log must be reopened, not error out")

	writeString(t, rw, "post-reopen\n")
	assert.Equal(t, "post-reopen\n", readFile(t, path),
		"writes after a reopen reach the recreated active log")
}

// keep 0 retains no rotated files — rotation just discards the active
// log and starts fresh. (ResolvedLogRotation never yields 0, but a
// direct Configure(_, 0) must still behave sanely.)
func TestRotatingWriter_KeepZeroDiscardsActiveLog(t *testing.T) {
	rw, path := newTestWriter(t)
	rw.Configure(1, 0)

	writeString(t, rw, "discard-me")
	require.NoError(t, rw.rotate())

	assertAbsent(t, rotatedPath(path, 1))
	assert.EqualValues(t, 0, fileSize(t, path), "a fresh active log remains after a keep=0 rotation")
}

// Concurrent writes and rotations must not race — Write and rotate are
// mutually exclusive under the writer's mutex. Run with -race to catch
// an unguarded fd swap.
func TestRotatingWriter_ConcurrentWritesAndRotate(t *testing.T) {
	rw, _ := newTestWriter(t)
	rw.Configure(2*1024, 4) // small cap so rotation fires repeatedly

	line := []byte("a concurrent log line, long enough to grow the file fast\n")
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				_, _ = rw.Write(line)
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			_ = rw.MaybeRotate()
		}
	}()
	wg.Wait()

	_, err := rw.Write([]byte("still-usable\n"))
	assert.NoError(t, err, "the writer must remain usable after concurrent writes + rotations")
}

// Path returns the active log path unchanged across rotations.
func TestRotatingWriter_PathStableAcrossRotation(t *testing.T) {
	rw, path := newTestWriter(t)
	rw.Configure(1, 3)

	assert.Equal(t, path, rw.Path())
	writeString(t, rw, "x")
	require.NoError(t, rw.rotate())
	assert.Equal(t, path, rw.Path(), "the active log path is fixed; only fds swap")
}

// When the reopen of a fresh active log fails, rotate must roll the
// active file back to its path and keep the old fd usable — the daemon
// must not lose its log over a failed rotation. This pins the
// highest-risk branch in the writer.
func TestRotatingWriter_ReopenFailureRollsBack(t *testing.T) {
	rw, path := newTestWriter(t)
	rw.Configure(1, 3)

	// First rotation succeeds normally — .1 holds "first\n".
	writeString(t, rw, "first\n")
	require.NoError(t, rw.rotate())
	writeString(t, rw, "active-before-fail\n")

	// Inject a reopen failure for the next rotation.
	prev := openLogFile
	t.Cleanup(func() { openLogFile = prev })
	openLogFile = func(string) (*os.File, error) {
		return nil, errors.New("injected reopen failure")
	}
	err := rw.rotate()
	openLogFile = prev // restore real opening before the filesystem assertions
	require.Error(t, err, "rotate must surface the reopen failure")

	// The active log is rolled back to its canonical path, and slot .1
	// (renamed away during the failed rotate) is left empty.
	require.FileExists(t, path, "active log must be rolled back to its path")
	assertAbsent(t, rotatedPath(path, 1))
	// The cascade ran before the failed reopen, so "first\n" shifted up
	// to .2 — the documented slow-history-loss edge.
	assert.Equal(t, "first\n", readFile(t, rotatedPath(path, 2)))

	// The old fd is still valid: a Write succeeds and lands in the
	// rolled-back active file — logging is not lost over a failed
	// rotation. Reaching here without a panic also covers requirement
	// (c) of the rollback contract.
	writeString(t, rw, "after-fail\n")
	assert.Equal(t, "active-before-fail\nafter-fail\n", readFile(t, path),
		"the daemon keeps logging through the old fd after a failed rotation")
}

// Lowering keep between runs must not leak the now-excess rotated
// files: rotate sweeps every slot beyond keep so history stays bounded.
func TestRotatingWriter_SweepsOrphansWhenKeepLowered(t *testing.T) {
	rw, path := newTestWriter(t)

	// Accumulate 5 rotated files under keep=5.
	rw.Configure(1, 5)
	for _, m := range []string{"r1", "r2", "r3", "r4", "r5"} {
		writeString(t, rw, m)
		require.NoError(t, rw.rotate())
	}
	require.Equal(t, 5, countRotatedFiles(t, path))

	// Lower keep to 2 and rotate once more.
	rw.Configure(1, 2)
	writeString(t, rw, "r6")
	require.NoError(t, rw.rotate())

	assert.Equal(t, 2, countRotatedFiles(t, path),
		"files beyond a lowered keep must be swept, not leaked")
	assertAbsent(t, rotatedPath(path, 3))
}

// SetupLogging runs more than once per process (main.go, then cobra's
// PersistentPreRun). The second call must reuse the one RotatingWriter
// rather than opening a fresh fd — so ActiveLogRotator never hands out
// a stale or duplicated writer, and no log fd leaks per call.
func TestSetupLogging_ReusesOneRotator(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)        // os.UserHomeDir on Linux/macOS
	t.Setenv("USERPROFILE", home) // os.UserHomeDir on Windows

	prevSlog := slog.Default()
	prevRotator := activeLogRotator
	activeLogRotator = nil
	t.Cleanup(func() {
		slog.SetDefault(prevSlog)
		activeLogRotator = prevRotator
	})

	SetupLogging(slog.LevelInfo)
	first := ActiveLogRotator()
	require.NotNil(t, first, "file logging should be set up under a valid home dir")

	SetupLogging(slog.LevelDebug) // the second startup call, with the configured level
	second := ActiveLogRotator()
	assert.Same(t, first, second,
		"a second SetupLogging must reuse the one writer, not open a fresh fd")
}
