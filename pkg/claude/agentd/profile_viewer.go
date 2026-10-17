package agentd

import (
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/google/pprof/driver"
)

// The dashboard serves pprof's own web UI (the one `go tool pprof -http`
// opens) for a stored capture, mounted under
// /api/profiles/{id}/ui/{kind}/ behind dashboard auth. That gives remote
// operators flame graphs, top tables and source views in a browser tab
// without a Go toolchain, a second port, or an SSH tunnel. pprof's pages use
// relative links, so they work under the prefix unchanged.

// profileViewerCacheSize bounds how many parsed profiles stay in memory. Each
// holds the decoded profile; a few is plenty for one operator comparing runs.
const profileViewerCacheSize = 4

type profileViewer struct {
	handlers map[string]http.Handler
}

var profileViewers struct {
	mu    sync.Mutex
	order []string // least recently used first
	byKey map[string]*profileViewer
}

func profileViewerKey(id, kind string) string { return id + "/" + kind }

// profileViewerFor returns (building on first use) the pprof web handlers for
// one profile file of one capture.
func profileViewerFor(id, kind string) (*profileViewer, error) {
	name, ok := profileFileByKind(kind)
	if !ok {
		return nil, errProfileNotFound
	}
	dir, err := profileDir(id)
	if err != nil {
		return nil, errProfileNotFound
	}
	path := filepath.Join(dir, name)
	if fi, err := os.Stat(path); err != nil || !fi.Mode().IsRegular() || fi.Size() == 0 {
		return nil, errProfileNotFound
	}

	key := profileViewerKey(id, kind)
	profileViewers.mu.Lock()
	if v := profileViewers.byKey[key]; v != nil {
		touchProfileViewer(key)
		profileViewers.mu.Unlock()
		return v, nil
	}
	profileViewers.mu.Unlock()
	// Parse outside the lock: a large heap/allocs profile must not stall
	// requests for viewers already cached. A concurrent duplicate build is
	// harmless; the first one stored wins.
	v, err := buildProfileViewer(path)
	if err != nil {
		return nil, err
	}
	profileViewers.mu.Lock()
	defer profileViewers.mu.Unlock()
	if existing := profileViewers.byKey[key]; existing != nil {
		touchProfileViewer(key)
		return existing, nil
	}
	if profileViewers.byKey == nil {
		profileViewers.byKey = map[string]*profileViewer{}
	}
	profileViewers.byKey[key] = v
	profileViewers.order = append(profileViewers.order, key)
	for len(profileViewers.order) > profileViewerCacheSize {
		delete(profileViewers.byKey, profileViewers.order[0])
		profileViewers.order = profileViewers.order[1:]
	}
	return v, nil
}

// touchProfileViewer moves key to the most-recently-used end. Caller holds mu.
func touchProfileViewer(key string) {
	for i, k := range profileViewers.order {
		if k == key {
			profileViewers.order = append(append(profileViewers.order[:i:i], profileViewers.order[i+1:]...), key)
			return
		}
	}
}

// dropProfileViewers forgets every cached viewer for a deleted capture.
func dropProfileViewers(id string) {
	profileViewers.mu.Lock()
	defer profileViewers.mu.Unlock()
	kept := profileViewers.order[:0]
	for _, k := range profileViewers.order {
		if strings.HasPrefix(k, id+"/") {
			delete(profileViewers.byKey, k)
			continue
		}
		kept = append(kept, k)
	}
	profileViewers.order = kept
}

// buildProfileViewer runs pprof's driver in web mode with an HTTPServer hook
// that captures the handler table instead of listening. The -http address is
// never bound: it only has to parse (a fixed port skips pprof's free-port
// probe). Symbolization is off because Go profiles carry their own symbols.
func buildProfileViewer(path string) (*profileViewer, error) {
	var handlers map[string]http.Handler
	err := driver.PProf(&driver.Options{
		Flagset: newPprofFlags([]string{"-http=127.0.0.1:1", "-no_browser", "-symbolize=none", path}),
		UI:      pprofQuietUI{},
		HTTPServer: func(args *driver.HTTPServerArgs) error {
			handlers = args.Handlers
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("load profile: %w", err)
	}
	if handlers == nil {
		return nil, errors.New("load profile: pprof did not start its web UI")
	}
	return &profileViewer{handlers: handlers}, nil
}

// pprofFlags adapts the standard flag package to pprof's FlagSet, parsing a
// fixed argument list instead of os.Args.
type pprofFlags struct {
	*flag.FlagSet
	args       []string
	extraUsage string
}

func newPprofFlags(args []string) *pprofFlags {
	fs := flag.NewFlagSet("pprof", flag.ContinueOnError)
	fs.SetOutput(discardWriter{})
	return &pprofFlags{FlagSet: fs, args: args}
}

func (f *pprofFlags) StringList(name, def, usage string) *[]*string {
	list := []*string{&def}
	if def == "" {
		list = nil
	}
	val := &list
	f.Func(name, usage, func(s string) error {
		*val = append(*val, &s)
		return nil
	})
	return val
}

func (f *pprofFlags) ExtraUsage() string      { return f.extraUsage }
func (f *pprofFlags) AddExtraUsage(eu string) { f.extraUsage += eu }
func (f *pprofFlags) Parse(usage func()) []string {
	if err := f.FlagSet.Parse(f.args); err != nil {
		usage()
		return nil
	}
	return f.Args()
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// pprofQuietUI routes pprof's console chatter to slog at debug level and
// never asks for input.
type pprofQuietUI struct{}

func (pprofQuietUI) ReadLine(string) (string, error)     { return "", errors.New("non-interactive") }
func (pprofQuietUI) Print(args ...any)                   { slog.Debug("pprof", "msg", fmt.Sprint(args...)) }
func (pprofQuietUI) PrintErr(args ...any)                { slog.Debug("pprof", "err", fmt.Sprint(args...)) }
func (pprofQuietUI) IsTerminal() bool                    { return false }
func (pprofQuietUI) WantBrowser() bool                   { return false }
func (pprofQuietUI) SetAutoComplete(func(string) string) {}

// serveProfileViewer dispatches a request under
// /api/profiles/{id}/ui/{kind}/ to the matching pprof handler.
func serveProfileViewer(w http.ResponseWriter, r *http.Request, id, kind string) {
	v, err := profileViewerFor(id, kind)
	if errors.Is(err, errProfileNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	prefix := "/api/profiles/" + id + "/ui/" + kind
	sub := strings.TrimPrefix(r.URL.Path, prefix)
	if sub == "" {
		sub = "/"
	}
	h, ok := v.handlers[sub]
	if !ok {
		http.NotFound(w, r)
		return
	}
	// pprof renders our own binary's symbols, but keep its pages out of
	// frames and stop them from sniffing types.
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	h.ServeHTTP(w, r)
}
