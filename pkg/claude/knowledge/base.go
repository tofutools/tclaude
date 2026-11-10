package knowledge

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

const (
	docsPrefix = "docs/"
	// maxReadLines and maxReadBytes keep one read from filling a context; the
	// reader asks for the next range.
	maxReadLines = 2000
	maxReadBytes = 256 << 10
	// DefaultSearchLimit and MaxSearchLimit bound matches, and maxMatchRunes one
	// line of a match: minified assets are a single enormous line.
	DefaultSearchLimit = 100
	MaxSearchLimit     = 500
	maxMatchRunes      = 240
)

// ErrNotFound is a path the knowledge base does not hold.
var ErrNotFound = errors.New("not in the knowledge base")

// ErrFetching refuses to remove the source during a download.
var ErrFetching = errors.New("the source is downloading")

// ErrBadRequest wraps an argument a reader can correct.
var ErrBadRequest = errors.New("bad request")

// Base reads the docs and source of one build. It has no method that writes
// what it serves.
type Base struct {
	build      Build
	docs       fs.FS
	root       string
	sourceBase string
	client     *http.Client

	graphOnce sync.Once
	pages     map[string]bool
	backlinks map[string][]Link

	mu       sync.Mutex
	fetching bool
	fetchErr error
	done     chan struct{}
	received atomic.Int64
}

// New returns a base for build, reading docs and keeping source under root.
func New(build Build, docs fs.FS, root string) *Base {
	return &Base{build: build, docs: docs, root: root, sourceBase: sourceURL, client: &http.Client{Timeout: fetchTimeout}}
}

// WithSourceURL points the source fetch elsewhere, for tests.
func (b *Base) WithSourceURL(base string) *Base {
	b.sourceBase = base
	return b
}

// Status is what a reader needs to know before trusting an answer.
type Status struct {
	Build  Build        `json:"build"`
	Label  string       `json:"label"`
	Pages  int          `json:"pages"`
	Source SourceStatus `json:"source"`
}

// Status reports the pin and how the source stands. A build tagged as a release
// learns its commit from the stored archive.
func (b *Base) Status() Status {
	b.loadGraph()
	source := b.sourceStatus()
	build := b.build
	if build.Commit == "" {
		build.Commit = source.Commit
	}
	return Status{Build: build, Label: build.Label(), Pages: len(b.pages), Source: source}
}

func (b *Base) loadGraph() {
	b.graphOnce.Do(func() {
		b.pages = map[string]bool{}
		b.backlinks = map[string][]Link{}
		var names []string
		_ = fs.WalkDir(b.docs, ".", func(name string, entry fs.DirEntry, err error) error {
			if err == nil && !entry.IsDir() && strings.HasSuffix(name, ".md") {
				names = append(names, name)
				b.pages[docsPrefix+name] = true
			}
			return nil
		})
		for _, name := range names {
			raw, err := fs.ReadFile(b.docs, name)
			if err != nil {
				continue
			}
			from := docsPrefix + name
			for _, link := range pageLinks(from, string(raw), b.pages) {
				if linkers := b.backlinks[link.Path]; !slices.ContainsFunc(linkers, func(l Link) bool { return l.Path == from }) {
					b.backlinks[link.Path] = append(linkers, Link{Path: from})
				}
			}
		}
	})
}

// sourceFS opens the stored tree. os.Root keeps every read inside it.
func (b *Base) sourceFS() (fs.FS, func(), bool) {
	if !b.canFetch() {
		return nil, nil, false
	}
	if _, ok := b.readSourceMeta(); !ok {
		return nil, nil, false
	}
	root, err := os.OpenRoot(filepath.Join(b.sourceDir(), sourceTreeDir))
	if err != nil {
		return nil, nil, false
	}
	return root.FS(), func() { _ = root.Close() }, true
}

// cleanPath turns what a reader typed into a tree path. Empty means the top.
func cleanPath(name string) (string, error) {
	name = strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(name), "./"), "/")
	if name == "" || name == "." {
		return "", nil
	}
	if !fs.ValidPath(name) {
		return "", fmt.Errorf("%w: %q is not a path inside the knowledge base", ErrBadRequest, name)
	}
	return name, nil
}

// locate picks the filesystem a path lives in and its name there.
func (b *Base) locate(name string, source fs.FS) (fs.FS, string, bool) {
	if inDocs, ok := strings.CutPrefix(name, docsPrefix); ok {
		return b.docs, inDocs, true
	}
	if name == "docs" {
		return b.docs, ".", true
	}
	if source != nil {
		return source, name, true
	}
	return nil, "", false
}

// readFile finds a file. A bare page name such as companion.md means the docs.
func (b *Base) readFile(name string, source fs.FS) (string, []byte, error) {
	for _, candidate := range []string{name, docsPrefix + name} {
		from, inner, ok := b.locate(candidate, source)
		if !ok {
			continue
		}
		raw, err := fs.ReadFile(from, inner)
		if err == nil {
			return candidate, raw, nil
		}
	}
	if source == nil && !strings.HasPrefix(name, docsPrefix) {
		return "", nil, fmt.Errorf("%w: %s (source is not available, see `tclaude agent knowledge status`)", ErrNotFound, name)
	}
	return "", nil, fmt.Errorf("%w: %s", ErrNotFound, name)
}

// Entry is one row of a listing.
type Entry struct {
	Path string `json:"path"`
	Dir  bool   `json:"dir,omitempty"`
	Size int64  `json:"size,omitempty"`
}

// List shows what is directly under a directory, like ls. The top joins docs
// with the source tree.
func (b *Base) List(dir string) ([]Entry, error) {
	dir, err := cleanPath(dir)
	if err != nil {
		return nil, err
	}
	source, closeSource, ok := b.sourceFS()
	if ok {
		defer closeSource()
	}
	if dir == "" {
		out := []Entry{{Path: "docs", Dir: true}}
		if source != nil {
			rows, err := listDir(source, ".", "")
			if err != nil {
				return nil, err
			}
			out = append(out, rows...)
		}
		return out, nil
	}
	from, inner, ok := b.locate(dir, source)
	if !ok {
		return nil, fmt.Errorf("%w: %s (source is not available, see `tclaude agent knowledge status`)", ErrNotFound, dir)
	}
	return listDir(from, inner, dir)
}

func listDir(from fs.FS, inner, prefix string) ([]Entry, error) {
	if info, err := fs.Stat(from, inner); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("%w: no directory %s", ErrNotFound, prefix)
	}
	rows, err := fs.ReadDir(from, inner)
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(rows))
	for _, row := range rows {
		entry := Entry{Path: path.Join(prefix, row.Name()), Dir: row.IsDir()}
		if info, err := row.Info(); err == nil && !row.IsDir() {
			entry.Size = info.Size()
		}
		out = append(out, entry)
	}
	return out, nil
}

// ReadRequest names what to read: a path, optionally with #section in it, and
// optionally a range of lines counted from 1. Section and lines exclude each
// other.
type ReadRequest struct {
	Path string
	From int
	To   int
}

// Page is one read. The links are what make the docs a graph: Links are the
// pages this text points to, LinkedFrom the pages that point here.
type Page struct {
	Path       string `json:"path"`
	Section    string `json:"section,omitempty"`
	Content    string `json:"content"`
	FromLine   int    `json:"from_line"`
	ToLine     int    `json:"to_line"`
	TotalLines int    `json:"total_lines"`
	Truncated  bool   `json:"truncated,omitempty"`
	LineCut    bool   `json:"line_cut,omitempty"` // ToLine itself is shown only in part
	Links      []Link `json:"links,omitempty"`
	LinkedFrom []Link `json:"linked_from,omitempty"`
}

// Read returns a file, a section of a page or a range of lines.
func (b *Base) Read(req ReadRequest) (*Page, error) {
	b.loadGraph()
	name, section, _ := strings.Cut(strings.TrimSpace(req.Path), "#")
	name, err := cleanPath(name)
	if err != nil {
		return nil, err
	}
	if name == "" {
		return nil, fmt.Errorf("%w: a path is required", ErrBadRequest)
	}
	if section != "" && (req.From != 0 || req.To != 0) {
		return nil, fmt.Errorf("%w: use a #section or --lines, not both", ErrBadRequest)
	}
	source, closeSource, ok := b.sourceFS()
	if ok {
		defer closeSource()
	}
	name, raw, err := b.readFile(name, source)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(string(raw), "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	start, end := 0, len(lines)
	if section != "" {
		var found bool
		var anchors []string
		start, end, found, anchors = sectionLines(lines, section)
		if !found {
			return nil, fmt.Errorf("%w: %s has no section %q; it has: %s", ErrNotFound, name, section, strings.Join(anchors, ", "))
		}
	}
	if req.From > 0 {
		start = req.From - 1
	}
	if req.To > 0 && req.To < end {
		end = req.To
	}
	if start > len(lines) || (req.To > 0 && req.To < req.From) {
		return nil, fmt.Errorf("%w: lines %d-%d are outside %s, which has %d", ErrBadRequest, req.From, req.To, name, len(lines))
	}
	page := &Page{Path: name, Section: section, FromLine: start + 1, TotalLines: len(lines)}
	var content strings.Builder
	for i := start; i < end; i++ {
		if i-start >= maxReadLines || content.Len()+len(lines[i])+1 > maxReadBytes {
			page.Truncated = true
			if i == start {
				// One line over the cap, as in a minified asset: show its start,
				// or no range of lines could ever reach it.
				content.WriteString(strings.ToValidUTF8(lines[i][:maxReadBytes], ""))
				content.WriteByte('\n')
				page.LineCut = true
				i++
			}
			end = i
			break
		}
		content.WriteString(lines[i])
		content.WriteByte('\n')
	}
	page.Content = content.String()
	page.ToLine = end
	if b.pages[name] {
		page.Links = pageLinks(name, page.Content, b.pages)
		page.LinkedFrom = b.backlinks[name]
	}
	return page, nil
}

// Match is one matching line.
type Match struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

// SearchRequest is a Go regular expression, which cannot backtrack badly,
// optionally limited to a path prefix.
type SearchRequest struct {
	Pattern    string
	Path       string
	IgnoreCase bool
	Limit      int
}

// SearchResult holds the matches, docs before source.
type SearchResult struct {
	Matches   []Match `json:"matches"`
	Truncated bool    `json:"truncated,omitempty"`
}

// Search greps the docs, then the source if it is there.
func (b *Base) Search(ctx context.Context, req SearchRequest) (*SearchResult, error) {
	pattern := req.Pattern
	if pattern == "" {
		return nil, fmt.Errorf("%w: a pattern is required", ErrBadRequest)
	}
	if req.IgnoreCase {
		pattern = "(?i)" + pattern
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrBadRequest, err.Error())
	}
	prefix, err := cleanPath(req.Path)
	if err != nil {
		return nil, err
	}
	limit := req.Limit
	if limit <= 0 {
		limit = DefaultSearchLimit
	}
	limit = min(limit, MaxSearchLimit)
	result := &SearchResult{Matches: []Match{}}
	if err := searchFS(ctx, b.docs, docsPrefix, prefix, re, limit, result); err != nil {
		return nil, err
	}
	if result.Truncated {
		return result, nil
	}
	if source, closeSource, ok := b.sourceFS(); ok {
		defer closeSource()
		if err := searchFS(ctx, source, "", prefix, re, limit, result); err != nil {
			return nil, err
		}
	}
	return result, nil
}

var errSearchFull = errors.New("search limit reached")

func searchFS(ctx context.Context, from fs.FS, mount, prefix string, re *regexp.Regexp, limit int, result *SearchResult) error {
	var names []string
	err := fs.WalkDir(from, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && withinPrefix(mount+name, prefix) {
			names = append(names, name)
		}
		return nil
	})
	if err != nil {
		return err
	}
	sort.Strings(names)
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return err
		}
		raw, err := fs.ReadFile(from, name)
		if err != nil {
			continue
		}
		if err := searchFile(mount+name, raw, re, limit, result); errors.Is(err, errSearchFull) {
			return nil
		}
	}
	return nil
}

func withinPrefix(name, prefix string) bool {
	return prefix == "" || name == prefix || strings.HasPrefix(name, prefix+"/")
}

func searchFile(name string, raw []byte, re *regexp.Regexp, limit int, result *SearchResult) error {
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(nil, maxFileBytes+1)
	for line := 1; scanner.Scan(); line++ {
		if !re.Match(scanner.Bytes()) {
			continue
		}
		if len(result.Matches) == limit {
			result.Truncated = true
			return errSearchFull
		}
		text := []rune(strings.TrimSpace(scanner.Text()))
		if len(text) > maxMatchRunes {
			text = append(text[:maxMatchRunes], '…')
		}
		result.Matches = append(result.Matches, Match{Path: name, Line: line, Text: string(text)})
	}
	return nil
}
