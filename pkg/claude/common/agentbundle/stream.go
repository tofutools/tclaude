package agentbundle

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

func (b *Bundle) Limit() int64 {
	if b.MaxBytes > 0 {
		return b.MaxBytes
	}
	return MaxBytes
}
func (b *Bundle) LimitError(what string) error {
	return fmt.Errorf("%s exceeds federation.agent_transfer_max_bytes=%d; raise this node setting for a larger agent", what, b.Limit())
}
func (b *Bundle) OpenHistory() (io.ReadCloser, error) {
	if b.TranscriptPath != "" {
		return os.Open(b.TranscriptPath)
	}
	return io.NopCloser(bytes.NewReader(b.Transcript)), nil
}
func (b *Bundle) Close() error {
	if b.ownedTranscript {
		b.ownedTranscript = false
		return os.Remove(b.TranscriptPath)
	}
	return nil
}
func (b *Bundle) SetHistoryFile(format, source, path string, owned bool) error {
	r, err := os.Open(path)
	if err != nil {
		return err
	}
	defer r.Close()
	sum := sha256.New()
	n, err := io.Copy(sum, io.LimitReader(r, b.Limit()+1))
	if err != nil {
		return err
	}
	if n > b.Limit() {
		return b.LimitError("history")
	}
	b.Transcript = nil
	b.TranscriptPath = path
	b.ownedTranscript = owned
	b.Manifest.History = &History{format, source, n, hex.EncodeToString(sum.Sum(nil))}
	return nil
}

type limitWriter struct {
	io.Writer
	left int64
}

func (w *limitWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.left {
		return 0, errors.New("archive exceeds configured transfer cap")
	}
	n, e := w.Writer.Write(p)
	w.left -= int64(n)
	return n, e
}

// DecodeFile validates the ZIP directory before streaming the transcript to a
// private temporary file. Close removes only that freshly extracted transcript.
func DecodeFile(f *os.File, limit int64, dir string) (bundle *Bundle, err error) {
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	b := &Bundle{MaxBytes: limit}
	if st.Size() > b.Limit() {
		return nil, b.LimitError("archive")
	}
	zr, err := zip.NewReader(f, st.Size())
	if err != nil {
		return nil, fmt.Errorf("invalid agent bundle ZIP: %w", err)
	}
	if len(zr.File) > 2 {
		return nil, errors.New("unexpected archive entries")
	}
	defer func() {
		if err != nil {
			_ = b.Close()
		}
	}()
	seen := map[string]bool{}
	for _, entry := range zr.File {
		cap := b.Limit()
		switch entry.Name {
		case ManifestFile:
			cap = MaxManifestBytes
		case HistoryFile:
		default:
			return nil, fmt.Errorf("unexpected archive entry %q", entry.Name)
		}
		if seen[entry.Name] || !entry.Mode().IsRegular() {
			return nil, errors.New("duplicate or non-regular archive entry")
		}
		seen[entry.Name] = true
		if entry.UncompressedSize64 > uint64(cap) {
			return nil, b.LimitError(entry.Name)
		}
		r, e := entry.Open()
		if e != nil {
			return nil, e
		}
		if entry.Name == ManifestFile {
			raw, e := io.ReadAll(io.LimitReader(r, cap+1))
			ce := r.Close()
			if e != nil {
				return nil, e
			}
			if ce != nil {
				return nil, ce
			}
			if int64(len(raw)) > cap {
				return nil, errors.New("manifest exceeds 1 MiB")
			}
			if e = json.Unmarshal(raw, &b.Manifest); e != nil {
				return nil, e
			}
		} else {
			tmp, e := os.CreateTemp(dir, ".agent-history-")
			if e != nil {
				_ = r.Close()
				return nil, e
			}
			b.TranscriptPath = tmp.Name()
			b.ownedTranscript = true
			n, e := io.Copy(tmp, io.LimitReader(r, cap+1))
			ce := r.Close()
			fe := tmp.Close()
			if e != nil {
				return nil, e
			}
			if ce != nil {
				return nil, ce
			}
			if fe != nil {
				return nil, fe
			}
			if n > cap {
				return nil, b.LimitError("history")
			}
		}
	}
	if !seen[ManifestFile] {
		return nil, errors.New("archive has no manifest.json")
	}
	if b.Manifest.History != nil && !seen[HistoryFile] {
		return nil, errors.New("declared history entry is missing")
	}
	if err = b.Validate(); err != nil {
		return nil, err
	}
	return b, nil
}
