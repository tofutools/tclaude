//go:build linux || darwin

package agentd

// Static live-page source: a directory or one file inside the agent's
// worktree, served read-only.
//
//   - The worktree root is opened once and held as a directory fd; every
//     request walks from it one component at a time with O_NOFOLLOW, so no
//     symlink is ever followed (not even one that points back inside) and a
//     swapped parent directory cannot redirect it.
//   - Only regular files with an allowlisted extension and a single link are
//     served: a hardlink to a file outside the worktree is refused.
//   - Dot-named components (.git, .env) are refused, in the declared target as
//     well as in request paths.
//   - The guard (pages_fsview.go) says where a walk starts: for a Linux
//     tclaude-layer agent, the worktree as the agent's own mount namespace
//     shows it, so the page serves exactly what the agent can read; when
//     that view cannot be opened nothing is served. Otherwise the walk starts
//     from the daemon's view and the guard leaves out by path what the
//     agent's recorded profile hides (the last mount entry covering the path
//     a walk ends at decides).
//   - Changes are found by polling the same no-follow walk, not by inotify: an
//     inotify watch follows a symlink swapped in between check and add (so it
//     could be steered at directories outside the worktree), and every watched
//     page would spend one of the user's few inotify instances.

import (
	"errors"
	"hash/fnv"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// stageStaticTypes is the extension allowlist and the Content-Type each is
// served with. Anything else is a 404.
var stageStaticTypes = map[string]string{
	".html": "text/html; charset=utf-8", ".htm": "text/html; charset=utf-8",
	".css": "text/css; charset=utf-8", ".js": "text/javascript; charset=utf-8",
	".mjs": "text/javascript; charset=utf-8", ".map": "application/json",
	".json": "application/json", ".txt": "text/plain; charset=utf-8",
	".svg": "image/svg+xml", ".png": "image/png", ".jpg": "image/jpeg",
	".jpeg": "image/jpeg", ".gif": "image/gif", ".webp": "image/webp",
	".avif": "image/avif", ".ico": "image/x-icon", ".woff": "font/woff",
	".woff2": "font/woff2", ".ttf": "font/ttf", ".otf": "font/otf",
	".wasm": "application/wasm", ".mp4": "video/mp4", ".webm": "video/webm",
	".mp3": "audio/mpeg", ".wav": "audio/wav", ".ogg": "audio/ogg",
	".xml": "application/xml", ".csv": "text/csv; charset=utf-8",
}

const (
	stageStaticMaxFile   = 64 << 20
	stageStaticIndexName = "index.html"
	// The change poll walks at most this many directory entries per pass; a
	// larger tree is watched only up to the limit.
	stageStaticPollEntries = 20000
	stageStaticPollEvery   = 400 * time.Millisecond
	stageStaticPollDepth   = 32
)

// stageStaticGuard is the page agent's view of its files (pageFSViewer).
type stageStaticGuard interface {
	// hidden says the stage leaves a canonical host path out.
	hidden(abs string) bool
	// base is a directory fd for the worktree as the agent sees it (the
	// caller closes it), or -1 to walk the daemon's view; gen is the view's
	// generation. An error means the agent's view is unreachable.
	base() (fd int, gen uint64, err error)
	// served records a page document served under gen.
	served(gen uint64)
	close()
}

// stageStatic serves a worktree directory or file.
type stageStatic struct {
	rootFD   int    // the worktree root, held open
	rootPath string // its canonical path, for the guard
	guard    stageStaticGuard
	target []string // path components from root to the source
	isFile bool

	// mu guards rootFD against Close: a closed fd number can be reused by an
	// unrelated file, and an openat against it would resolve somewhere else.
	mu     sync.RWMutex
	closed bool
	stop   chan struct{}
}

// stageStaticBadComponent reports a path component the static source never
// opens.
func stageStaticBadComponent(part string) bool {
	return part == "" || strings.HasPrefix(part, ".") || strings.ContainsAny(part, "/\\\x00")
}

// newStageStatic opens a static source. worktree is the agent's worktree and
// target a path inside it (absolute or relative to the worktree). onChange is
// called when a served file changes.
func newStageStatic(worktree, target string, onChange func(), guard stageStaticGuard) (*stageStatic, error) {
	root, err := filepath.EvalSymlinks(worktree)
	if err != nil || !filepath.IsAbs(root) {
		return nil, stageSourceErrorf("the worktree cannot be resolved")
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(worktree, target)
	}
	rel, err := filepath.Rel(filepath.Clean(worktree), filepath.Clean(target))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		// Allow a target spelled through the canonical root too.
		rel, err = filepath.Rel(root, filepath.Clean(target))
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, stageSourceErrorf("the target is outside the worktree")
		}
	}
	var parts []string
	if rel != "." {
		parts = strings.Split(rel, string(filepath.Separator))
	}
	for _, part := range parts {
		if stageStaticBadComponent(part) {
			return nil, stageSourceErrorf("the target names a hidden or invalid path")
		}
	}
	rootFD, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, stageSourceErrorf("the worktree cannot be opened")
	}
	s := &stageStatic{rootFD: rootFD, rootPath: root, guard: guard, target: parts, stop: make(chan struct{})}
	fd, _, err := s.openAt(parts)
	var unreachable *stageStaticUnreachable
	if errors.Is(err, errStageStaticHidden) {
		_ = s.Close()
		return nil, stageSourceErrorf("your sandbox hides the target, so the page cannot serve it")
	}
	if errors.As(err, &unreachable) {
		_ = s.Close()
		return nil, stageSourceErrorf("the page is served from your sandbox's own view of its files, which cannot be opened: %v", unreachable.err)
	}
	if errors.Is(err, unix.ENOENT) && s.guard != nil {
		_ = s.Close()
		return nil, stageSourceErrorf("your sandbox does not show the target")
	}
	if err != nil {
		_ = s.Close()
		return nil, stageSourceErrorf("the target cannot be served")
	}
	var st unix.Stat_t
	err = unix.Fstat(fd, &st)
	_ = unix.Close(fd)
	if err != nil {
		_ = s.Close()
		return nil, stageSourceErrorf("the target cannot be served")
	}
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFDIR:
	case unix.S_IFREG:
		s.isFile = true
		if _, ok := stageStaticTypes[strings.ToLower(path.Ext(parts[len(parts)-1]))]; !ok || st.Nlink != 1 {
			_ = s.Close()
			return nil, stageSourceErrorf("the target is not a servable file")
		}
	default:
		_ = s.Close()
		return nil, stageSourceErrorf("the target is not a file or directory")
	}
	if onChange != nil {
		go s.poll(onChange)
	}
	return s, nil
}

var errStageStaticHidden = errors.New("hidden from the page's agent")

// stageStaticReadBack says whether a static source's content may go back to
// its agent: it is served exactly as the agent sees its files. live asks
// about what the operator's browser shows now (see pageFSViewer.readBack).
func stageStaticReadBack(up http.RoundTripper, live bool) bool {
	s, ok := up.(*stageStatic)
	if !ok {
		return false
	}
	v, ok := s.guard.(*pageFSViewer)
	return ok && v.readBack(live)
}

// hiddenAt reports whether the guard hides root/parts.
func (s *stageStatic) hiddenAt(parts []string) bool {
	if s.guard == nil {
		return false
	}
	return s.guard.hidden(filepath.Join(append([]string{s.rootPath}, parts...)...))
}

// openAt walks parts from the root fd without following any symlink and
// returns the final component's fd, opened read-only. A path the guard hides
// is refused (its fd closed unread).
func (s *stageStatic) openAt(parts []string) (int, uint64, error) {
	// The walk refreshes the guard's view, so the check after it uses the
	// view the file was reached through.
	fd, gen, err := s.walkAt(parts)
	if s.hiddenAt(parts) {
		if err == nil {
			_ = unix.Close(fd)
		}
		return -1, gen, errStageStaticHidden
	}
	return fd, gen, err
}

// walkAt is openAt without the guard. Only the change poll uses it, to walk
// directories (never to read a file), so a path bound below a hidden one is
// still watched.
func (s *stageStatic) walkAt(parts []string) (int, uint64, error) {
	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return -1, 0, errors.New("closed")
	}
	fd, gen := -1, uint64(0)
	var err error
	if s.guard != nil {
		if fd, gen, err = s.guard.base(); err != nil {
			s.mu.RUnlock()
			return -1, gen, &stageStaticUnreachable{err}
		}
	}
	if fd < 0 {
		fd, err = unix.Openat(s.rootFD, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	}
	s.mu.RUnlock()
	if err != nil {
		return -1, gen, err
	}
	for i, part := range parts {
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, "/\x00") {
			_ = unix.Close(fd)
			return -1, gen, errors.New("invalid path")
		}
		flags := unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_CLOEXEC | unix.O_NONBLOCK
		if i < len(parts)-1 {
			flags |= unix.O_DIRECTORY
		}
		next, err := unix.Openat(fd, part, flags, 0)
		_ = unix.Close(fd)
		if err != nil {
			return -1, gen, err
		}
		fd = next
	}
	return fd, gen, nil
}

// stageStaticUnreachable is the agent's own view failing to open: the page
// serves nothing rather than fall back to the daemon's.
type stageStaticUnreachable struct{ err error }

func (e *stageStaticUnreachable) Error() string { return e.err.Error() }

// resolve maps a request path to components under the root, or nil.
func (s *stageStatic) resolve(urlPath string) []string {
	clean := path.Clean("/" + urlPath)
	var req []string
	if clean != "/" {
		req = strings.Split(strings.TrimPrefix(clean, "/"), "/")
	}
	for _, part := range req {
		if stageStaticBadComponent(part) {
			return nil
		}
	}
	if s.isFile {
		name := s.target[len(s.target)-1]
		if len(req) == 0 || (len(req) == 1 && req[0] == name) {
			return s.target
		}
		return nil
	}
	return append(append([]string{}, s.target...), req...)
}

func stageStaticResponse(req *http.Request, code int, msg string) *http.Response {
	return &http.Response{
		StatusCode: code, Status: strconv.Itoa(code) + " " + http.StatusText(code),
		Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header:        http.Header{"Content-Type": {"text/plain; charset=utf-8"}},
		Body:          io.NopCloser(strings.NewReader(msg)),
		ContentLength: int64(len(msg)),
		Request:       req,
	}
}

func stageStaticNotFound(req *http.Request) *http.Response {
	return stageStaticResponse(req, http.StatusNotFound, "404 page not found\n")
}

func (s *stageStatic) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		_ = req.Body.Close()
	}
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		res := stageStaticResponse(req, http.StatusMethodNotAllowed, "static pages are read-only\n")
		res.Header.Set("Allow", "GET, HEAD")
		return res, nil
	}
	parts := s.resolve(req.URL.Path)
	if parts == nil {
		return stageStaticNotFound(req), nil
	}
	fd, gen, err := s.openAt(parts)
	var unreachable *stageStaticUnreachable
	if errors.As(err, &unreachable) {
		return stageStaticResponse(req, http.StatusServiceUnavailable,
			"this page is served from its agent's sandbox, which cannot be reached: "+unreachable.err.Error()+"\n"), nil
	}
	if err != nil {
		return stageStaticNotFound(req), nil
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		_ = unix.Close(fd)
		return stageStaticNotFound(req), nil
	}
	if st.Mode&unix.S_IFMT == unix.S_IFDIR {
		_ = unix.Close(fd)
		if !strings.HasSuffix(req.URL.Path, "/") {
			res := stageStaticResponse(req, http.StatusMovedPermanently, "")
			res.Header.Set("Location", path.Base(req.URL.Path)+"/")
			return res, nil
		}
		parts = append(parts, stageStaticIndexName)
		if fd, gen, err = s.openAt(parts); err != nil {
			return stageStaticNotFound(req), nil
		}
		if err := unix.Fstat(fd, &st); err != nil {
			_ = unix.Close(fd)
			return stageStaticNotFound(req), nil
		}
	}
	ctype, ok := stageStaticTypes[strings.ToLower(path.Ext(parts[len(parts)-1]))]
	if st.Mode&unix.S_IFMT != unix.S_IFREG || !ok || st.Nlink != 1 || st.Size > stageStaticMaxFile {
		_ = unix.Close(fd)
		return stageStaticNotFound(req), nil
	}
	if s.guard != nil && strings.HasPrefix(ctype, "text/html") && stageFrameLoad(req) {
		// The operator's frame loaded a document: what it shows now came
		// from this view.
		s.guard.served(gen)
	}
	f := os.NewFile(uintptr(fd), path.Join(parts...))
	res := &http.Response{
		StatusCode: http.StatusOK, Status: "200 OK",
		Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header: http.Header{
			"Content-Type":  {ctype},
			"Cache-Control": {"no-cache"},
		},
		ContentLength: st.Size,
		Request:       req,
	}
	if req.Method == http.MethodHead {
		_ = f.Close()
		res.Body = http.NoBody
		return res, nil
	}
	// Exactly the size fstat saw, even if the file grows while being read.
	res.Body = struct {
		io.Reader
		io.Closer
	}{io.LimitReader(f, st.Size), f}
	return res, nil
}

// poll calls onChange whenever the signature of the served files changes.
func (s *stageStatic) poll(onChange func()) {
	last := s.signature()
	tick := time.NewTicker(stageStaticPollEvery)
	defer tick.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-tick.C:
			if sig := s.signature(); sig != last {
				last = sig
				onChange()
			}
		}
	}
}

// signature hashes the name, size, mtime and inode of every servable file
// under the source, walked with the same no-follow rules as serving. The walk
// is breadth-first and holds at most one directory open at a time; it stops
// at stageStaticPollDepth levels and stageStaticPollEntries entries, so a
// hostile tree (very deep, or one huge directory) costs bounded fds and CPU.
func (s *stageStatic) signature() uint64 {
	h := fnv.New64a()
	if s.isFile {
		dir, name := s.target[:len(s.target)-1], s.target[len(s.target)-1]
		if s.hiddenAt(s.target) {
			return 0
		}
		fd, _, err := s.walkAt(dir)
		if err != nil {
			return 0
		}
		stageStaticHashEntry(h, fd, name)
		_ = unix.Close(fd)
		return h.Sum64()
	}
	budget := stageStaticPollEntries
	queue := [][]string{nil}
	for len(queue) > 0 && budget > 0 {
		rel := queue[0]
		queue = queue[1:]
		fd, _, err := s.walkAt(append(append([]string{}, s.target...), rel...))
		if err != nil {
			continue
		}
		prefix := strings.Join(rel, "/")
		dir := os.NewFile(uintptr(fd), prefix)
		for budget > 0 {
			entries, err := dir.ReadDir(256)
			for _, e := range entries {
				if budget <= 0 {
					break
				}
				budget--
				name := e.Name()
				if stageStaticBadComponent(name) || name == "node_modules" {
					continue
				}
				// A hidden entry's changes would reload the page: a one-bit
				// signal about a file the agent cannot read. A hidden
				// directory is still walked, for any path bound below it.
				hidden := s.hiddenAt(append(append(append([]string{}, s.target...), rel...), name))
				switch {
				case e.Type().IsDir():
					if !hidden {
						_, _ = io.WriteString(h, prefix+"/"+name+"/\x00")
					}
					if len(rel) < stageStaticPollDepth {
						queue = append(queue, append(append([]string{}, rel...), name))
					}
				case e.Type().IsRegular() && !hidden:
					if _, ok := stageStaticTypes[strings.ToLower(path.Ext(name))]; ok {
						_, _ = io.WriteString(h, prefix+"/")
						stageStaticHashEntry(h, fd, name)
					}
				}
			}
			if err != nil {
				break
			}
		}
		_ = dir.Close()
	}
	return h.Sum64()
}

func stageStaticHashEntry(h io.Writer, dirFD int, name string) {
	var st unix.Stat_t
	if unix.Fstatat(dirFD, name, &st, unix.AT_SYMLINK_NOFOLLOW) != nil {
		_, _ = io.WriteString(h, name+"\x00missing\x00")
		return
	}
	_, _ = io.WriteString(h, name+"\x00"+strconv.FormatInt(st.Size, 10)+"\x00"+
		strconv.FormatInt(st.Mtim.Nano(), 10)+"\x00"+strconv.FormatUint(st.Ino, 10)+"\x00")
}

func (s *stageStatic) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		close(s.stop)
		_ = unix.Close(s.rootFD)
		if s.guard != nil {
			s.guard.close()
		}
	}
	return nil
}
