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
)

// stageStatic serves a worktree directory or file.
type stageStatic struct {
	rootFD int      // the worktree root, held open
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
func newStageStatic(worktree, target string, onChange func()) (*stageStatic, error) {
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
	s := &stageStatic{rootFD: rootFD, target: parts, stop: make(chan struct{})}
	fd, err := s.openAt(parts)
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

// openAt walks parts from the root fd without following any symlink and
// returns the final component's fd, opened read-only.
func (s *stageStatic) openAt(parts []string) (int, error) {
	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return -1, errors.New("closed")
	}
	fd, err := unix.Openat(s.rootFD, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	s.mu.RUnlock()
	if err != nil {
		return -1, err
	}
	for i, part := range parts {
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, "/\x00") {
			_ = unix.Close(fd)
			return -1, errors.New("invalid path")
		}
		flags := unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_CLOEXEC | unix.O_NONBLOCK
		if i < len(parts)-1 {
			flags |= unix.O_DIRECTORY
		}
		next, err := unix.Openat(fd, part, flags, 0)
		_ = unix.Close(fd)
		if err != nil {
			return -1, err
		}
		fd = next
	}
	return fd, nil
}

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
	fd, err := s.openAt(parts)
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
		if fd, err = s.openAt(parts); err != nil {
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
// under the source, walked with the same no-follow rules as serving.
func (s *stageStatic) signature() uint64 {
	h := fnv.New64a()
	if s.isFile {
		dir, name := s.target[:len(s.target)-1], s.target[len(s.target)-1]
		fd, err := s.openAt(dir)
		if err != nil {
			return 0
		}
		stageStaticHashEntry(h, fd, name)
		_ = unix.Close(fd)
		return h.Sum64()
	}
	fd, err := s.openAt(s.target)
	if err != nil {
		return 0
	}
	budget := stageStaticPollEntries
	stageStaticHashDir(h, fd, "", &budget)
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

// stageStaticHashDir hashes dirFD's tree and closes dirFD.
func stageStaticHashDir(h io.Writer, dirFD int, prefix string, budget *int) {
	dir := os.NewFile(uintptr(dirFD), prefix)
	defer func() { _ = dir.Close() }()
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return
	}
	for _, e := range entries {
		if *budget <= 0 {
			return
		}
		*budget--
		name := e.Name()
		if stageStaticBadComponent(name) || name == "node_modules" {
			continue
		}
		switch {
		case e.Type().IsDir():
			sub, err := unix.Openat(dirFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			if err != nil {
				continue
			}
			_, _ = io.WriteString(h, prefix+name+"/\x00")
			stageStaticHashDir(h, sub, prefix+name+"/", budget)
		case e.Type().IsRegular():
			if _, ok := stageStaticTypes[strings.ToLower(path.Ext(name))]; ok {
				stageStaticHashEntry(h, dirFD, name)
			}
		}
	}
}

func (s *stageStatic) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		close(s.stop)
		_ = unix.Close(s.rootFD)
	}
	return nil
}
