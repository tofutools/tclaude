//go:build unix

package agentd

// Static live-page source: a directory or one file inside the agent's
// worktree, served read-only. Every request walks from the worktree root one
// component at a time with O_NOFOLLOW, so no symlink is ever followed (not
// even one that points back inside), and serves only regular files with an
// allowlisted extension. Dot-named components (.git, .env) are refused. The
// daemon watches the source and asks open views to reload on a save.

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
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
	stageStaticMaxWatch  = 2000
	stageStaticDebounce  = 150 * time.Millisecond
	stageStaticIndexName = "index.html"
)

// stageStatic serves a worktree directory or file.
type stageStatic struct {
	root   string   // canonical worktree root
	target []string // path components from root to the source
	isFile bool

	watcher *fsnotify.Watcher
	stop    chan struct{}
	once    sync.Once
}

// newStageStatic opens a static source. worktree is the agent's worktree and
// target a path inside it (absolute or relative to the worktree). onChange is
// called (debounced) when a watched file changes.
func newStageStatic(worktree, target string, onChange func()) (*stageStatic, error) {
	root, err := filepath.EvalSymlinks(worktree)
	if err != nil || !filepath.IsAbs(root) {
		return nil, stageSourceErrorf("the worktree %s cannot be resolved", worktree)
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(worktree, target)
	}
	rel, err := filepath.Rel(filepath.Clean(worktree), filepath.Clean(target))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		// Allow a target spelled through the canonical root too.
		rel, err = filepath.Rel(root, filepath.Clean(target))
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, stageSourceErrorf("%s is outside the worktree", target)
		}
	}
	var parts []string
	if rel != "." {
		parts = strings.Split(rel, string(filepath.Separator))
	}
	s := &stageStatic{root: root, target: parts, stop: make(chan struct{})}
	f, err := s.open(parts)
	if err != nil {
		return nil, stageSourceErrorf("%s cannot be served: %v", target, err)
	}
	var st unix.Stat_t
	err = unix.Fstat(int(f.Fd()), &st)
	_ = f.Close()
	if err != nil {
		return nil, stageSourceErrorf("%s cannot be served: %v", target, err)
	}
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFDIR:
	case unix.S_IFREG:
		s.isFile = true
		if _, ok := stageStaticTypes[strings.ToLower(path.Ext(parts[len(parts)-1]))]; !ok {
			return nil, stageSourceErrorf("%s is not a servable file type", target)
		}
	default:
		return nil, stageSourceErrorf("%s is not a file or directory", target)
	}
	if onChange != nil {
		s.watch(onChange)
	}
	return s, nil
}

// open walks parts from the root without following any symlink and returns
// the final component opened read-only.
func (s *stageStatic) open(parts []string) (*os.File, error) {
	fd, err := unix.Open(s.root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	for i, part := range parts {
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, "/\x00") {
			_ = unix.Close(fd)
			return nil, errors.New("invalid path")
		}
		flags := unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_CLOEXEC | unix.O_NONBLOCK
		if i < len(parts)-1 {
			flags |= unix.O_DIRECTORY
		}
		next, err := unix.Openat(fd, part, flags, 0)
		_ = unix.Close(fd)
		if err != nil {
			return nil, err
		}
		fd = next
	}
	return os.NewFile(uintptr(fd), path.Join(parts...)), nil
}

// resolve maps a request path to components under the root, or nil.
func (s *stageStatic) resolve(urlPath string) []string {
	clean := path.Clean("/" + urlPath)
	var req []string
	if clean != "/" {
		req = strings.Split(strings.TrimPrefix(clean, "/"), "/")
	}
	for _, part := range req {
		if part == "" || strings.HasPrefix(part, ".") || strings.ContainsAny(part, "\\\x00") {
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
		return stageStaticResponse(req, http.StatusNotFound, "404 page not found\n"), nil
	}
	f, err := s.open(parts)
	if err != nil {
		return stageStaticResponse(req, http.StatusNotFound, "404 page not found\n"), nil
	}
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		_ = f.Close()
		return stageStaticResponse(req, http.StatusNotFound, "404 page not found\n"), nil
	}
	if st.Mode&unix.S_IFMT == unix.S_IFDIR {
		_ = f.Close()
		if !strings.HasSuffix(req.URL.Path, "/") {
			res := stageStaticResponse(req, http.StatusMovedPermanently, "")
			res.Header.Set("Location", path.Base(req.URL.Path)+"/")
			return res, nil
		}
		parts = append(parts, stageStaticIndexName)
		if f, err = s.open(parts); err != nil {
			return stageStaticResponse(req, http.StatusNotFound, "404 page not found\n"), nil
		}
		if err := unix.Fstat(int(f.Fd()), &st); err != nil {
			_ = f.Close()
			return stageStaticResponse(req, http.StatusNotFound, "404 page not found\n"), nil
		}
	}
	ctype, ok := stageStaticTypes[strings.ToLower(path.Ext(parts[len(parts)-1]))]
	if st.Mode&unix.S_IFMT != unix.S_IFREG || !ok || st.Size > stageStaticMaxFile {
		_ = f.Close()
		return stageStaticResponse(req, http.StatusNotFound, "404 page not found\n"), nil
	}
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

// watch watches the source (recursively for a directory, skipping dot and
// node_modules directories and never following a symlink) and calls
// onChange, debounced, on any change.
func (s *stageStatic) watch(onChange func()) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		slog.Warn("stage: static watch unavailable", "err", err)
		return
	}
	s.watcher = w
	dir := filepath.Join(append([]string{s.root}, s.target...)...)
	var fileName string
	if s.isFile {
		fileName = filepath.Base(dir)
		dir = filepath.Dir(dir)
		if err := w.Add(dir); err != nil {
			slog.Warn("stage: static watch", "dir", dir, "err", err)
		}
	} else {
		s.addTree(dir)
	}
	go func() {
		var timer *time.Timer
		fire := make(chan struct{}, 1)
		for {
			select {
			case <-s.stop:
				if timer != nil {
					timer.Stop()
				}
				return
			case ev, ok := <-w.Events:
				if !ok {
					return
				}
				if fileName != "" && filepath.Base(ev.Name) != fileName {
					continue
				}
				if !s.isFile && ev.Has(fsnotify.Create) {
					if fi, err := os.Lstat(ev.Name); err == nil && fi.IsDir() {
						s.addTree(ev.Name)
					}
				}
				if timer == nil {
					timer = time.AfterFunc(stageStaticDebounce, func() {
						select {
						case fire <- struct{}{}:
						default:
						}
					})
				} else {
					timer.Reset(stageStaticDebounce)
				}
			case <-fire:
				onChange()
			case _, ok := <-w.Errors:
				if !ok {
					return
				}
			}
		}
	}()
}

func (s *stageStatic) addTree(dir string) {
	count := len(s.watcher.WatchList())
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			return nil // WalkDir reports symlinks as non-dirs: never followed
		}
		if p != dir && (strings.HasPrefix(d.Name(), ".") || d.Name() == "node_modules") {
			return filepath.SkipDir
		}
		if count >= stageStaticMaxWatch {
			return filepath.SkipAll
		}
		if err := s.watcher.Add(p); err == nil {
			count++
		}
		return nil
	})
}

func (s *stageStatic) Close() error {
	s.once.Do(func() {
		close(s.stop)
		if s.watcher != nil {
			_ = s.watcher.Close()
		}
	})
	return nil
}

// Describe is for errors and logs.
func (s *stageStatic) String() string {
	return fmt.Sprintf("static %s", filepath.Join(append([]string{s.root}, s.target...)...))
}
