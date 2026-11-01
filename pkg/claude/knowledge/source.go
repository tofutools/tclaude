package knowledge

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"time"
)

const (
	// sourceURL serves GitHub's archive of a commit or tag.
	sourceURL = "https://codeload.github.com/tofutools/tclaude/tar.gz/"
	// SourceOrigin is where the source comes from, to tell the operator.
	SourceOrigin = "github.com/tofutools/tclaude"
	// fetchTimeout covers the whole download on a slow link.
	fetchTimeout = 10 * time.Minute
	// ApproxDownloadBytes and ApproxStoredBytes are what to expect, measured on
	// a real archive. GitHub sends no length for an archive, so the operator's
	// question and the progress bar rest on these, and both say "about".
	ApproxDownloadBytes = 18 << 20
	ApproxStoredBytes   = 28 << 20
	// The caps are far above a real tree and only bound a bad response.
	maxArchiveBytes = 512 << 20
	maxTreeBytes    = 2 << 30
	maxFileBytes    = 2 << 20
	// sourceTreeDir holds the files, beside sourceMetaFile.
	sourceTreeDir  = "tree"
	sourceMetaFile = "source.json"
)

// safeRef is what may name a directory in the store.
var safeRef = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z._+-]*$`)

// sourceMeta records what a stored tree is.
type sourceMeta struct {
	Commit string `json:"commit"`
	Files  int    `json:"files"`
	Bytes  int64  `json:"bytes"`
}

// SourceState says where the source stands.
type SourceState string

const (
	SourceReady       SourceState = "ready"
	SourceFetching    SourceState = "fetching"
	SourceFailed      SourceState = "failed"
	SourceUnavailable SourceState = "unavailable"
)

// SourceNotDownloaded explains a missing source to a reader. Only the operator
// starts a download, so an agent is told to say so rather than to wait or retry.
const SourceNotDownloaded = "not downloaded; docs are readable. Only the operator can download the source, in the dashboard under Config > Steward; an agent can't start it"

// SourceStatus is the source half of a status report.
type SourceStatus struct {
	State  SourceState `json:"state"`
	Commit string      `json:"commit,omitempty"`
	Files  int         `json:"files,omitempty"`
	// Bytes is what the stored tree takes.
	Bytes int64 `json:"bytes,omitempty"`
	// Received is how much of a running download has arrived.
	Received int64  `json:"received,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

func (b *Base) sourceDir() string {
	return filepath.Join(b.root, b.build.Ref)
}

// readSourceMeta reports the stored tree for this build, if there is one. The
// directory is renamed into place whole, so its presence means it is complete.
func (b *Base) readSourceMeta() (*sourceMeta, bool) {
	raw, err := os.ReadFile(filepath.Join(b.sourceDir(), sourceMetaFile))
	if err != nil {
		return nil, false
	}
	var meta sourceMeta
	if json.Unmarshal(raw, &meta) != nil {
		return nil, false
	}
	return &meta, true
}

// canFetch reports whether this build names a source at all.
func (b *Base) canFetch() bool {
	return b.build.State != StateUnknown && safeRef.MatchString(b.build.Ref)
}

func (b *Base) sourceStatus() SourceStatus {
	if !b.canFetch() {
		return SourceStatus{State: SourceUnavailable, Detail: "this build names no commit to fetch"}
	}
	if meta, ok := b.readSourceMeta(); ok {
		return SourceStatus{State: SourceReady, Commit: meta.Commit, Files: meta.Files, Bytes: meta.Bytes}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.fetching {
		return SourceStatus{State: SourceFetching, Received: b.received.Load(), Detail: "downloading " + shortRef(b.build.Ref) + "; docs are readable meanwhile"}
	}
	if b.fetchErr != nil {
		return SourceStatus{State: SourceFailed, Detail: b.fetchErr.Error() + "; docs are readable"}
	}
	return SourceStatus{State: SourceUnavailable, Detail: SourceNotDownloaded}
}

// StartFetch starts downloading the source if this build has one and it is not
// stored, so a version already held is never fetched again. It returns at once.
// A download takes the operator's consent, so only their answer or their
// Config choice may lead here, never a read.
func (b *Base) StartFetch() {
	if !b.canFetch() {
		return
	}
	if _, ok := b.readSourceMeta(); ok {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.fetching {
		return
	}
	b.fetching, b.fetchErr = true, nil
	b.received.Store(0)
	b.done = make(chan struct{})
	go b.runFetch(b.done)
}

func (b *Base) runFetch(done chan struct{}) {
	ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
	defer cancel()
	err := b.fetch(ctx)
	if err != nil {
		slog.Warn("knowledge: fetch source failed", "ref", b.build.Ref, "error", err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.fetching, b.fetchErr = false, err
	close(done)
}

// WaitForFetch waits for a running fetch to end.
func (b *Base) WaitForFetch(ctx context.Context) {
	b.mu.Lock()
	done := b.done
	b.mu.Unlock()
	if done == nil {
		return
	}
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// fetch downloads the tree, moves it into place whole and drops any other
// build's tree: only the running build is worth the disk.
func (b *Base) fetch(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.sourceBase+b.build.Ref, nil)
	if err != nil {
		return err
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return fmt.Errorf("fetch source: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%s is not on GitHub; a local commit has no source here", shortRef(b.build.Ref))
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch source: %s", resp.Status)
	}
	if err := os.MkdirAll(b.root, 0o700); err != nil {
		return err
	}
	staging, err := os.MkdirTemp(b.root, ".fetch-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(staging) }()
	counted := &countingReader{from: resp.Body, count: &b.received}
	meta, err := extractSource(io.LimitReader(counted, maxArchiveBytes), filepath.Join(staging, sourceTreeDir))
	if err != nil {
		return err
	}
	if err := b.checkCommit(meta.Commit); err != nil {
		return err
	}
	raw, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(staging, sourceMetaFile), raw, 0o600); err != nil {
		return err
	}
	// Only a tree with no readable record can be in the way here.
	if err := os.RemoveAll(b.sourceDir()); err != nil {
		return err
	}
	if err := os.Rename(staging, b.sourceDir()); err != nil {
		return err
	}
	b.pruneOthers()
	return nil
}

// countingReader tells a status read how far a download has come.
type countingReader struct {
	from  io.Reader
	count *atomic.Int64
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.from.Read(p)
	r.count.Add(int64(n))
	return n, err
}

// checkCommit refuses an archive of anything but the commit this build names.
func (b *Base) checkCommit(got string) error {
	if got == "" {
		return errors.New("source archive names no commit")
	}
	if want := b.build.Commit; want != "" && !strings.HasPrefix(got, want) {
		return fmt.Errorf("source archive is commit %s, not %s", shortRef(got), shortRef(want))
	}
	return nil
}

// PruneOthers drops every other build's tree and anything a stopped download
// left, so the store holds one tree at most. A running download does this itself
// when it lands, and its staging directory must stay.
func (b *Base) PruneOthers() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.fetching {
		b.pruneOthers()
	}
}

func (b *Base) pruneOthers() {
	entries, err := os.ReadDir(b.root)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.Name() != b.build.Ref {
			_ = os.RemoveAll(filepath.Join(b.root, entry.Name()))
		}
	}
}

// Remove drops the stored source. A running download finishes first.
func (b *Base) Remove() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.fetching {
		return ErrFetching
	}
	b.fetchErr = nil
	if !safeRef.MatchString(b.build.Ref) {
		return nil
	}
	return os.RemoveAll(b.sourceDir())
}

// droppedSourceDirs and droppedSourceSuffixes are what a Steward doesn't need to
// answer how tclaude works, about half of the tree: tests, their helpers and
// vendored or generated files. docs/ comes from the binary, which is what the
// build shipped with. .claude/ is this repository's own agent setup.
var (
	droppedSourceDirs     = []string{"docs", ".claude", ".github", "jstest", "testdata", "testharness", "third_party"}
	droppedSourceSuffixes = []string{"_test.go", ".test.mjs", ".min.js", ".min.mjs", ".map", ".lock", "go.sum", "package-lock.json"}
)

// keepSourcePath says whether an archive path belongs in the tree.
func keepSourcePath(name string) bool {
	dirs := strings.Split(path.Dir(name), "/")
	for _, dropped := range droppedSourceDirs {
		if slices.Contains(dirs, dropped) {
			return false
		}
	}
	return !slices.ContainsFunc(droppedSourceSuffixes, func(suffix string) bool { return strings.HasSuffix(name, suffix) })
}

// extractSource writes the readable files of a GitHub archive to dir and reads
// the commit from its header. Only regular text files within the size cap are
// kept, so a link cannot point a later read outside the tree.
func extractSource(archive io.Reader, dir string) (*sourceMeta, error) {
	gz, err := gzip.NewReader(archive)
	if err != nil {
		return nil, fmt.Errorf("read source archive: %w", err)
	}
	defer func() { _ = gz.Close() }()
	meta := &sourceMeta{}
	var total int64
	tr := tar.NewReader(gz)
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read source archive: %w", err)
		}
		if header.Typeflag == tar.TypeXGlobalHeader {
			meta.Commit = header.PAXRecords["comment"]
			continue
		}
		if header.Typeflag != tar.TypeReg || header.Size > maxFileBytes {
			continue
		}
		// The archive's one top-level directory is tclaude-<ref>.
		_, name, found := strings.Cut(header.Name, "/")
		if !found || !filepath.IsLocal(name) || path.Clean(name) != name || !keepSourcePath(name) {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return nil, fmt.Errorf("read source archive: %w", err)
		}
		if bytes.IndexByte(data, 0) >= 0 {
			continue
		}
		if total += int64(len(data)); total > maxTreeBytes {
			return nil, errors.New("source archive is larger than expected")
		}
		target := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(target, data, 0o400); err != nil {
			return nil, err
		}
		meta.Files++
		meta.Bytes = total
	}
	if meta.Files == 0 {
		return nil, errors.New("source archive holds no files")
	}
	return meta, nil
}
