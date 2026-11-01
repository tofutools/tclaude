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
	"strings"
	"time"
)

const (
	// sourceURL serves GitHub's archive of a commit or tag.
	sourceURL = "https://codeload.github.com/tofutools/tclaude/tar.gz/"
	// fetchTimeout covers the whole download on a slow link.
	fetchTimeout = 10 * time.Minute
	// fetchRetryAfter stops a reader on a dead link from retrying on every call.
	fetchRetryAfter = time.Minute
	// A tree is 65 MiB today. The caps are far above it and only bound a bad
	// response.
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
const SourceNotDownloaded = "not downloaded; docs are readable, and only the operator can choose to download the source"

// SourceStatus is the source half of a status report.
type SourceStatus struct {
	State  SourceState `json:"state"`
	Commit string      `json:"commit,omitempty"`
	Files  int         `json:"files,omitempty"`
	Detail string      `json:"detail,omitempty"`
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
		return SourceStatus{State: SourceReady, Commit: meta.Commit, Files: meta.Files}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.fetching {
		return SourceStatus{State: SourceFetching, Detail: "fetching " + shortRef(b.build.Ref) + "; docs are readable meanwhile"}
	}
	if b.fetchErr != nil {
		return SourceStatus{State: SourceFailed, Detail: b.fetchErr.Error() + "; docs are readable"}
	}
	return SourceStatus{State: SourceUnavailable, Detail: SourceNotDownloaded}
}

// Prefetch starts fetching the source if this build has one and it is not
// stored. It returns at once. A recent failure is not retried until
// fetchRetryAfter has passed. A download takes the operator's consent, so no read
// path, summon or daemon start may call this.
func (b *Base) Prefetch() {
	if !b.canFetch() {
		return
	}
	if _, ok := b.readSourceMeta(); ok {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.fetching || (b.fetchErr != nil && time.Since(b.fetchedAt) < fetchRetryAfter) {
		return
	}
	b.fetching = true
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
	b.fetching, b.fetchErr, b.fetchedAt = false, err, time.Now()
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
	meta, err := extractSource(io.LimitReader(resp.Body, maxArchiveBytes), filepath.Join(staging, sourceTreeDir))
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

// keepSourcePath says whether an archive path belongs in the tree. docs/ comes
// from the binary, which is what the build shipped with. .claude/ is this
// repository's own agent setup, not how tclaude works.
func keepSourcePath(name string) bool {
	return !strings.HasPrefix(name, "docs/") && !strings.HasPrefix(name, ".claude/")
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
	}
	if meta.Files == 0 {
		return nil, errors.New("source archive holds no files")
	}
	return meta, nil
}
