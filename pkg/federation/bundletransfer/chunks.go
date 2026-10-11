package bundletransfer

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
)

const ChunkBytes int64 = 16 << 20

// PartialBytes reports only complete, verified chunks. An interrupted append
// is truncated to the previous chunk boundary before another fetch starts.
func (s Spool) PartialBytes(peer string, d Descriptor) (int64, error) {
	f, err := s.partial(peer, d)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return 0, err
	}
	n := st.Size()
	if n > d.Bytes {
		return 0, errors.New("partial bundle exceeds its descriptor")
	}
	if n != d.Bytes {
		n -= n % ChunkBytes
	}
	return n, f.Truncate(n)
}

func (s Spool) partial(peer string, d Descriptor) (*os.File, error) {
	path, err := s.path("in", peer, d.ID)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(s.Root, 0700); err != nil {
		return nil, err
	}
	// Include the authenticated digest so a different descriptor cannot reuse
	// bytes from an earlier transfer with the same offer ID.
	hash, err := hex.DecodeString(d.SHA256)
	if err != nil || len(hash) != sha256.Size {
		return nil, errors.New("invalid bundle digest")
	}
	f, err := openPartialFile(path + "." + d.SHA256 + ".partial")
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		f.Close()
		return nil, errors.New("invalid partial spool file")
	}
	return f, nil
}

// ReceiveChunk verifies length, digest and authenticated EOF before appending.
// A failed chunk never changes the previously verified prefix.
func (s Spool) ReceiveChunk(peer string, d Descriptor, offset, length int64, digest string, r io.Reader) error {
	if offset < 0 || offset%ChunkBytes != 0 || length <= 0 || length > ChunkBytes || offset > d.Bytes-length || length != min(ChunkBytes, d.Bytes-offset) {
		return errors.New("invalid bundle chunk range")
	}
	chunk, err := os.CreateTemp(s.Root, ".receiving-chunk-")
	if err != nil {
		return err
	}
	defer func() { _ = chunk.Close(); _ = os.Remove(chunk.Name()) }()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(chunk, h), io.LimitReader(r, length+1))
	if err != nil {
		return err
	}
	if n != length || hex.EncodeToString(h.Sum(nil)) != digest {
		return errors.New("chunk length or SHA-256 mismatch")
	}
	f, err := s.partial(peer, d)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if st.Size() != offset {
		return errors.New("partial bundle offset changed")
	}
	if _, err = f.Seek(offset, io.SeekStart); err != nil {
		return err
	}
	if _, err = chunk.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if _, err = io.CopyN(f, chunk, length); err != nil {
		_ = f.Truncate(offset)
		return err
	}
	if err = f.Sync(); err != nil {
		_ = f.Truncate(offset)
		return err
	}
	return nil
}

func (s Spool) FinishChunks(peer string, d Descriptor) error {
	f, err := s.partial(peer, d)
	if err != nil {
		return err
	}
	name := f.Name()
	err = d.VerifyReader(f)
	closeErr := f.Close()
	if err != nil {
		_ = os.Remove(name)
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	path, err := s.path("in", peer, d.ID)
	if err != nil {
		return err
	}
	if err = os.Rename(name, path); err != nil {
		return fmt.Errorf("publish bundle: %w", err)
	}
	return nil
}
