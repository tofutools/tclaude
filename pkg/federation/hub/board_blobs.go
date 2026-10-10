package hub

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/tofutools/tclaude/pkg/federation/proto"
	"golang.org/x/sys/unix"
)

const boardBlobSchema = `CREATE TABLE IF NOT EXISTS board_blobs(board TEXT NOT NULL REFERENCES boards(id) ON DELETE CASCADE,id TEXT NOT NULL,bytes INTEGER NOT NULL CHECK(bytes>0),digest TEXT NOT NULL,state TEXT NOT NULL CHECK(state IN ('pending','ready')),expires_at TEXT NOT NULL,pinned INTEGER NOT NULL DEFAULT 0,PRIMARY KEY(board,id)) STRICT;`

func (s *Store) boardBlobRoot(board string) (*os.Root, error) {
	if !proto.ValidStreamID(board) {
		return nil, adminErr(400, "board", "invalid board ID")
	}
	base := filepath.Join(filepath.Dir(s.path), "board-blobs")
	if err := os.MkdirAll(base, 0700); err != nil {
		return nil, err
	}
	// Canonicalize the operator-owned hub state root, then pin directories. Blob
	// filenames and board IDs are generated IDs, never arbitrary user paths.
	canonical, err := filepath.EvalSymlinks(base)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(canonical)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	if err = root.Mkdir(board, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	info, err := root.Lstat(board)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("refusing aliased board storage directory")
	}
	return root.OpenRoot(board)
}

// StoreBoardBlob reserves quota before reading a bounded ciphertext stream.
// Pending uploads consume the same quota as ready blobs. The final authority
// check ensures removal/freeze during the upload refuses publication.
func (s *Store) StoreBoardBlob(instance, board, id, digest string, size int64, source io.Reader) error {
	if !proto.ValidStreamID(board) || !proto.ValidStreamID(id) || size <= 0 || size > proto.MaxBoardItemBytes+64 {
		return adminErr(400, "blob", "invalid board ciphertext descriptor")
	}
	expected, err := hex.DecodeString(digest)
	if err != nil || len(expected) != sha256.Size {
		return adminErr(400, "blob", "invalid ciphertext digest")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var quota, versions, used, count int64
	if err = tx.QueryRow(`SELECT b.quota_bytes,b.max_versions FROM boards b JOIN board_members m ON m.board=b.id WHERE b.id=? AND m.instance=? AND m.role IN ('owner','publisher') AND b.frozen=0`, board, instance).Scan(&quota, &versions); err != nil {
		return adminErr(403, "board_publish", "active publisher required")
	}
	if err = tx.QueryRow(`SELECT coalesce(sum(bytes),0),count(*) FROM board_blobs WHERE board=?`, board).Scan(&used, &count); err != nil {
		return err
	}
	used, err = boardStorageUsed(tx, board)
	if err != nil {
		return err
	}
	if size > quota-used || count >= versions {
		return adminErr(409, "board_quota", "board ciphertext quota reached")
	}
	if _, err = tx.Exec(`INSERT INTO board_blobs VALUES(?,?,?,?,'pending',?,0)`, board, id, size, digest, ts(time.Now().Add(time.Hour))); err != nil {
		return adminErr(409, "blob_exists", "blob already exists or upload in progress")
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	published := false
	defer func() {
		if !published {
			_, _ = s.db.Exec(`DELETE FROM board_blobs WHERE board=? AND id=? AND state='pending'`, board, id)
		}
	}()
	root, err := s.boardBlobRoot(board)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	temp := "pending-" + proto.NewEnvelopeID()
	f, err := root.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	defer func() { _ = root.Remove(temp) }()
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, hash), io.LimitReader(source, size+1))
	if err == nil && n != size {
		err = fmt.Errorf("ciphertext stream length mismatch")
	}
	if err == nil && hex.EncodeToString(hash.Sum(nil)) != digest {
		err = fmt.Errorf("ciphertext digest mismatch")
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	tx, err = s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var authority int
	if err = tx.QueryRow(`SELECT count(*) FROM boards b JOIN board_members m ON m.board=b.id WHERE b.id=? AND m.instance=? AND m.role IN ('owner','publisher') AND b.frozen=0`, board, instance).Scan(&authority); err != nil {
		return err
	}
	if authority != 1 {
		return adminErr(403, "board_publish", "publisher removed or board frozen during upload")
	}
	// Both source and destination are within the pinned board directory. Existing
	// IDs have a quota reservation, so a second publisher cannot replace them.
	if err = root.Rename(temp, id); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE board_blobs SET state='ready',expires_at=? WHERE board=? AND id=? AND state='pending'`, ts(time.Now().Add(30*24*time.Hour)), board, id); err != nil {
		_ = root.Remove(id)
		return err
	}
	if err = tx.Commit(); err != nil {
		_ = root.Remove(id)
		return err
	}
	published = true
	return nil
}
func (s *Store) ReadBoardBlob(instance, board, id string, dst io.Writer) error {
	if !proto.ValidStreamID(board) || !proto.ValidStreamID(id) {
		return adminErr(400, "blob", "invalid blob ID")
	}
	var size int64
	var digest string
	if err := s.db.QueryRow(`SELECT x.bytes,x.digest FROM board_blobs x JOIN board_members m ON m.board=x.board WHERE x.board=? AND x.id=? AND x.state='ready' AND m.instance=? AND (x.pinned=1 OR x.expires_at>?)`, board, id, instance, ts(time.Now())).Scan(&size, &digest); err != nil {
		return adminErr(404, "blob_unknown", "blob not available")
	}
	root, err := s.boardBlobRoot(board)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	f, err := root.OpenFile(id, os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() || st.Size() != size {
		return errors.New("invalid board ciphertext file")
	}
	// Verify ciphertext on disk before releasing it, including after a restart.
	hash := sha256.New()
	if _, err = io.Copy(hash, io.LimitReader(f, size+1)); err != nil {
		return err
	}
	if hex.EncodeToString(hash.Sum(nil)) != digest {
		return errors.New("board ciphertext digest mismatch")
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	var authorized int
	if e := s.db.QueryRow(`SELECT count(*) FROM board_members WHERE board=? AND instance=?`, board, instance).Scan(&authorized); e != nil {
		return e
	}
	if authorized != 1 {
		return adminErr(403, "board_member", "board membership removed")
	}
	_, err = io.CopyN(dst, f, size)
	return err
}

// SweepBoardBlobs removes expired unpinned blobs, abandoned reservations and
// unreferenced files from prior crashes. Call at startup, before accepting any
// uploads. Only expired reservations and hour-old orphans are removed; another
// live hub process may still be using fresh files in the same state directory.
func (s *Store) SweepBoardBlobs() error {
	rows, err := s.db.Query(`SELECT board,id FROM board_blobs WHERE pinned=0 AND expires_at<=?`, ts(time.Now()))
	if err != nil {
		return err
	}
	type blob struct{ board, id string }
	expired := []blob{}
	for rows.Next() {
		var b blob
		if err = rows.Scan(&b.board, &b.id); err != nil {
			rows.Close()
			return err
		}
		expired = append(expired, b)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, b := range expired {
		root, err := s.boardBlobRoot(b.board)
		if err != nil {
			return err
		}
		err = root.Remove(b.id)
		_ = root.Close()
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if _, err = s.db.Exec(`DELETE FROM board_blobs WHERE board=? AND id=?`, b.board, b.id); err != nil {
			return err
		}
	}
	base := filepath.Join(filepath.Dir(s.path), "board-blobs")
	if err = os.MkdirAll(base, 0700); err != nil {
		return err
	}
	root, err := os.OpenRoot(base)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	dirs, err := os.ReadDir(base)
	if err != nil {
		return err
	}
	for _, dir := range dirs {
		if !proto.ValidStreamID(dir.Name()) || !dir.IsDir() {
			continue
		}
		child, err := root.OpenRoot(dir.Name())
		if err != nil {
			return err
		}
		f, err := child.Open(".")
		if err != nil {
			_ = child.Close()
			return err
		}
		entries, err := f.ReadDir(-1)
		f.Close()
		if err != nil {
			_ = child.Close()
			return err
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			var n int
			if err = s.db.QueryRow(`SELECT count(*) FROM board_blobs WHERE board=? AND id=?`, dir.Name(), entry.Name()).Scan(&n); err != nil {
				_ = child.Close()
				return err
			}
			if n == 0 {
				info, e := entry.Info()
				if e != nil {
					_ = child.Close()
					return e
				}
				if time.Since(info.ModTime()) < time.Hour {
					continue
				}
				if err = child.Remove(entry.Name()); err != nil {
					_ = child.Close()
					return err
				}
			}
		}
		_ = child.Close()
	}
	return nil
}
