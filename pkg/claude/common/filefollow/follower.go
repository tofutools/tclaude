// Package filefollow provides the shared cursor and record-reading machinery
// for append-only files such as harness transcripts, rollouts, and daemon logs.
package filefollow

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
)

var errFileChanged = errors.New("file changed while scanning")

const (
	DefaultAnchorBytes        = 64
	defaultStabilityAttempts  = 8
	defaultReplacementRetries = 3
)

// Cursor is the durable, format-neutral part of a follower checkpoint. Parser
// fold state must be checkpointed alongside it by the format-specific caller.
type Cursor struct {
	Path            string `json:"path"`
	Offset          int64  `json:"offset"`
	FileSize        int64  `json:"file_size"`
	ModTimeUnixNano int64  `json:"mod_time_unix_nano"`
	Device          uint64 `json:"device,omitempty"`
	Inode           uint64 `json:"inode,omitempty"`
	Anchor          []byte `json:"anchor"`
}

// Update describes the work performed by Refresh. ReadBytes counts payload
// bytes presented to the format scanner; unchanged refreshes report zero.
type Update[S any] struct {
	State     S
	Info      os.FileInfo
	Offset    int64
	ReadBytes int64
	Rebuilt   bool
	Unchanged bool
}

// Stats is cumulative follower instrumentation. PayloadBytes counts bytes
// presented to the scanner by successful refreshes, including retried tails.
type Stats struct {
	Refreshes    uint64
	Unchanged    uint64
	Rebuilds     uint64
	Appends      uint64
	PayloadBytes int64
}

// Scanner folds records from r into state. It returns the number of complete
// bytes committed, whether appended input was doubtful, and any hard error.
// A doubtful append is discarded and rebuilt from the configured initial
// offset. Full scans receive strict=false; append scans receive strict=true.
type Scanner[S any] func(r io.Reader, path string, state *S, strict bool) (consumed int64, doubt bool, err error)

// Config supplies only format-specific behavior. Cursor validation, stable
// EOF capture, replacement handling, and checkpoint anchors remain shared.
type Config[S any] struct {
	NewState   func(path string, initialOffset int64) S
	CloneState func(S) S
	Scan       Scanner[S]

	// InitialOffset optionally chooses a bounded hydration start. JSONL
	// transcripts and rollouts leave it nil (byte zero); tail views such as
	// logs can seek near EOF and align to their first complete record.
	InitialOffset func(file *os.File, info os.FileInfo) (int64, error)
	AnchorBytes   int
}

// Follower owns one path's validated cursor and accumulated parser state. It
// is intentionally not internally synchronized; callers choose the lock scope
// that also protects their derived snapshots and checkpoint persistence.
type Follower[S any] struct {
	config Config[S]

	path     string
	info     os.FileInfo
	cursor   Cursor
	state    S
	primed   bool
	restored bool
	stats    Stats
}

func New[S any](config Config[S]) *Follower[S] {
	if config.NewState == nil || config.CloneState == nil || config.Scan == nil {
		panic("filefollow: NewState, CloneState, and Scan are required")
	}
	if config.AnchorBytes <= 0 {
		config.AnchorBytes = DefaultAnchorBytes
	}
	return &Follower[S]{config: config}
}

// Restore primes a follower with a cursor and its matching parser fold state.
// Refresh validates file identity, size, mtime, and anchor before trusting it.
func (f *Follower[S]) Restore(cursor Cursor, state S) error {
	if cursor.Path == "" || cursor.Offset < 0 || cursor.FileSize < cursor.Offset ||
		cursor.Offset > 0 && (len(cursor.Anchor) == 0 || len(cursor.Anchor) > f.config.AnchorBytes) {
		return fmt.Errorf("filefollow: invalid checkpoint cursor")
	}
	f.path = cursor.Path
	f.info = nil
	f.cursor = cloneCursor(cursor)
	f.state = state
	f.primed = true
	f.restored = true
	return nil
}

// Checkpoint returns the current validated cursor. Parser state is deliberately
// separate so each format can encode it in its own versioned checkpoint.
func (f *Follower[S]) Checkpoint() (Cursor, bool) {
	if !f.primed || f.cursor.Path == "" || f.cursor.FileSize < f.cursor.Offset ||
		f.cursor.Offset > 0 && len(f.cursor.Anchor) == 0 {
		return Cursor{}, false
	}
	return cloneCursor(f.cursor), true
}

// FileInfo returns the identity/metadata captured from the descriptor used by
// the most recent successful scan. It is nil before hydration or after Reset.
func (f *Follower[S]) FileInfo() os.FileInfo { return f.info }

// Stats returns cumulative work counters for tests and diagnostics.
func (f *Follower[S]) Stats() Stats { return f.stats }

func (f *Follower[S]) Reset() {
	var zero S
	f.path = ""
	f.info = nil
	f.cursor = Cursor{}
	f.state = zero
	f.primed = false
	f.restored = false
}

// Refresh returns cached state without opening an unchanged file, scans only
// appended bytes when the cursor validates, and performs one authoritative
// rebuild on rotation, truncation, rewrite, or decode doubt.
func (f *Follower[S]) Refresh(path string) (Update[S], error) {
	f.stats.Refreshes++
	info, err := os.Stat(path)
	if err != nil {
		return Update[S]{}, err
	}
	if f.path != "" && path != f.path {
		f.Reset()
	}
	if f.restored {
		if !f.restoreMatches(path, info) {
			f.Reset()
		} else {
			f.path = path
			f.info = info
			f.restored = false
			if info.Size() == f.cursor.FileSize && info.ModTime().UnixNano() == f.cursor.ModTimeUnixNano {
				f.stats.Unchanged++
				return Update[S]{State: f.state, Info: info, Offset: f.cursor.Offset, Unchanged: true}, nil
			}
		}
	}
	if f.primed && f.info != nil && os.SameFile(f.info, info) &&
		f.cursor.FileSize == info.Size() && f.cursor.ModTimeUnixNano == info.ModTime().UnixNano() {
		f.stats.Unchanged++
		return Update[S]{State: f.state, Info: info, Offset: f.cursor.Offset, Unchanged: true}, nil
	}

	if f.primed && f.info != nil && os.SameFile(f.info, info) &&
		info.Size() >= f.cursor.FileSize && info.Size() > f.cursor.Offset {
		update, appendErr := f.scanAppend(path)
		if appendErr == nil {
			f.stats.Appends++
			f.stats.PayloadBytes += update.ReadBytes
			return update, nil
		}
	}
	update, err := f.fullScan(path)
	if err == nil {
		f.stats.Rebuilds++
		f.stats.PayloadBytes += update.ReadBytes
	}
	return update, err
}

func (f *Follower[S]) fullScan(path string) (Update[S], error) {
	for range defaultReplacementRetries {
		file, err := os.Open(path) //nolint:gosec // callers provide their own transcript/log paths
		if err != nil {
			return Update[S]{}, err
		}
		openedInfo, err := file.Stat()
		if err != nil {
			_ = file.Close()
			return Update[S]{}, err
		}
		start := int64(0)
		if f.config.InitialOffset != nil {
			start, err = f.config.InitialOffset(file, openedInfo)
			if err != nil {
				_ = file.Close()
				return Update[S]{}, err
			}
		}
		state := f.config.NewState(path, start)
		offset, cursor, scannedInfo, readBytes, doubt, scanErr := f.scanStable(file, path, start, &state, false)
		_ = file.Close()
		if scanErr != nil {
			if errors.Is(scanErr, errFileChanged) {
				continue
			}
			return Update[S]{}, scanErr
		}
		if doubt {
			return Update[S]{}, fmt.Errorf("filefollow: doubtful record during full scan of %s", path)
		}
		pathInfo, statErr := os.Stat(path)
		if statErr != nil {
			return Update[S]{}, statErr
		}
		if !os.SameFile(openedInfo, pathInfo) {
			continue
		}
		f.commit(path, scannedInfo, cursor, state)
		return Update[S]{State: state, Info: f.info, Offset: offset, ReadBytes: readBytes, Rebuilt: true}, nil
	}
	return Update[S]{}, fmt.Errorf("filefollow: %s changed repeatedly while scanning", path)
}

func (f *Follower[S]) scanAppend(path string) (Update[S], error) {
	file, err := os.Open(path) //nolint:gosec // callers provide their own transcript/log paths
	if err != nil {
		return Update[S]{}, err
	}
	defer func() { _ = file.Close() }()
	openedInfo, err := file.Stat()
	if err != nil {
		return Update[S]{}, err
	}
	if f.info == nil || !os.SameFile(f.info, openedInfo) {
		return Update[S]{}, fmt.Errorf("filefollow: file replaced before append scan")
	}
	if !f.anchorMatches(file, f.cursor) {
		return Update[S]{}, fmt.Errorf("filefollow: cursor anchor mismatch")
	}
	state := f.config.CloneState(f.state)
	offset, cursor, scannedInfo, readBytes, doubt, err := f.scanStable(file, path, f.cursor.Offset, &state, true)
	if err != nil {
		return Update[S]{}, err
	}
	if doubt {
		return Update[S]{}, fmt.Errorf("filefollow: doubtful appended record")
	}
	pathInfo, err := os.Stat(path)
	if err != nil {
		return Update[S]{}, err
	}
	if !os.SameFile(openedInfo, pathInfo) {
		return Update[S]{}, fmt.Errorf("filefollow: file replaced during append scan")
	}
	f.commit(path, scannedInfo, cursor, state)
	return Update[S]{State: state, Info: f.info, Offset: offset, ReadBytes: readBytes}, nil
}

func (f *Follower[S]) scanStable(file *os.File, path string, offset int64, state *S, strict bool) (int64, Cursor, os.FileInfo, int64, bool, error) {
	var totalRead int64
	var doubt bool
	for range defaultStabilityAttempts {
		before, err := file.Stat()
		if err != nil {
			return offset, Cursor{}, nil, totalRead, doubt, err
		}
		if _, err := file.Seek(offset, io.SeekStart); err != nil {
			return offset, Cursor{}, nil, totalRead, doubt, err
		}
		counter := &countingReader{r: file}
		consumed, scanDoubt, scanErr := f.config.Scan(counter, path, state, strict)
		totalRead += counter.n
		if scanErr != nil {
			return offset, Cursor{}, nil, totalRead, doubt, scanErr
		}
		if consumed < 0 || consumed > counter.n {
			return offset, Cursor{}, nil, totalRead, doubt, fmt.Errorf("filefollow: scanner committed invalid byte count %d of %d", consumed, counter.n)
		}
		doubt = doubt || scanDoubt
		offset += consumed
		after, err := file.Stat()
		if err != nil {
			return offset, Cursor{}, nil, totalRead, doubt, err
		}
		if before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
			continue
		}
		cursor, capturedInfo, err := f.captureCursor(file, path, offset)
		if err != nil {
			return offset, Cursor{}, nil, totalRead, doubt, err
		}
		if cursor.FileSize != after.Size() || cursor.ModTimeUnixNano != after.ModTime().UnixNano() {
			continue
		}
		return offset, cursor, capturedInfo, totalRead, doubt, nil
	}
	return offset, Cursor{}, nil, totalRead, doubt, fmt.Errorf("%w: %s did not stabilize", errFileChanged, path)
}

func (f *Follower[S]) captureCursor(file *os.File, path string, offset int64) (Cursor, os.FileInfo, error) {
	if offset < 0 {
		return Cursor{}, nil, fmt.Errorf("filefollow: invalid offset %d", offset)
	}
	info, err := file.Stat()
	if err != nil {
		return Cursor{}, nil, err
	}
	if info.Size() < offset {
		return Cursor{}, nil, fmt.Errorf("%w: file shrank below offset", errFileChanged)
	}
	start := max(offset-int64(f.config.AnchorBytes), 0)
	anchor := make([]byte, offset-start)
	if len(anchor) > 0 {
		if _, err := file.ReadAt(anchor, start); err != nil {
			return Cursor{}, nil, err
		}
	}
	device, inode, _ := fileIdentity(info)
	return Cursor{Path: path, Offset: offset, FileSize: info.Size(), ModTimeUnixNano: info.ModTime().UnixNano(), Device: device, Inode: inode, Anchor: anchor}, info, nil
}

func (f *Follower[S]) restoreMatches(path string, info os.FileInfo) bool {
	if path != f.cursor.Path || info.Size() < f.cursor.Offset || info.Size() < f.cursor.FileSize {
		return false
	}
	file, err := os.Open(path) //nolint:gosec // caller-provided transcript/log path
	if err != nil {
		return false
	}
	defer func() { _ = file.Close() }()
	openedInfo, err := file.Stat()
	if err != nil || !os.SameFile(info, openedInfo) || !f.anchorMatches(file, f.cursor) {
		return false
	}
	if info.Size() == f.cursor.FileSize && info.ModTime().UnixNano() != f.cursor.ModTimeUnixNano {
		return false
	}
	device, inode, ok := fileIdentity(openedInfo)
	if ok && (f.cursor.Device == 0 || f.cursor.Inode == 0 || device != f.cursor.Device || inode != f.cursor.Inode) {
		return false
	}
	pathInfo, err := os.Stat(path)
	return err == nil && os.SameFile(openedInfo, pathInfo)
}

func (f *Follower[S]) anchorMatches(file *os.File, cursor Cursor) bool {
	if cursor.Offset == 0 {
		return true
	}
	if len(cursor.Anchor) == 0 || int64(len(cursor.Anchor)) > cursor.Offset {
		return false
	}
	buf := make([]byte, len(cursor.Anchor))
	if _, err := file.ReadAt(buf, cursor.Offset-int64(len(buf))); err != nil {
		return false
	}
	return bytes.Equal(buf, cursor.Anchor)
}

func (f *Follower[S]) commit(path string, info os.FileInfo, cursor Cursor, state S) {
	f.path = path
	f.info = info
	f.cursor = cloneCursor(cursor)
	f.state = state
	f.primed = true
	f.restored = false
}

type countingReader struct {
	r io.Reader
	n int64
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	r.n += int64(n)
	return n, err
}

func cloneCursor(cursor Cursor) Cursor {
	cursor.Anchor = append([]byte(nil), cursor.Anchor...)
	return cursor
}
