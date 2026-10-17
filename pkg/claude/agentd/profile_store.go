package agentd

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/pprof"
	"runtime/trace"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/pprof/profile"
	"github.com/tofutools/tclaude/pkg/claude/common/perfstats"
	"github.com/tofutools/tclaude/pkg/common"
)

// Profile captures live under ~/.tclaude/data/profiles/<id>/, one directory
// per capture with a meta.json describing it. Both the dashboard (in-process
// capture) and `tclaude agentd profile` (over the pprof socket) write here, so
// the Activity › Performance list shows every capture regardless of origin.

const (
	profileMetaFile        = "meta.json"
	profileSummaryFile     = "summary.txt"
	profileMaxSeconds      = 600
	profileDefaultSeconds  = 30
	profileDefaultTraceSec = 5
	profileMaxLabelLen     = 80
)

const (
	profileStatusRunning     = "running"
	profileStatusDone        = "done"
	profileStatusFailed      = "failed"
	profileStatusInterrupted = "interrupted"
)

// profileFiles is the closed set of artifacts a capture may contain. The
// download and viewer routes only ever serve names from this table, so a
// request can never address an arbitrary file under the store.
var profileFiles = []struct {
	Name string
	// Kind is the pprof viewer key ("" for files the viewer does not open).
	Kind string
	// Lookup is the runtime/pprof profile written after the CPU window.
	Lookup string
	Debug  int
	// Delta marks cumulative-since-start profiles (block, mutex) that are
	// stored as the difference over the capture window.
	Delta bool
}{
	{Name: "cpu.pprof", Kind: "cpu"},
	{Name: "trace.out"},
	{Name: "heap.pprof", Kind: "heap", Lookup: "heap"},
	{Name: "allocs.pprof", Kind: "allocs", Lookup: "allocs"},
	{Name: "goroutine.pprof", Kind: "goroutine", Lookup: "goroutine"},
	{Name: "goroutines.txt", Lookup: "goroutine", Debug: 2},
	{Name: "block.pprof", Kind: "block", Lookup: "block", Delta: true},
	{Name: "mutex.pprof", Kind: "mutex", Lookup: "mutex", Delta: true},
	{Name: "stats-before.json"},
	{Name: "stats-after.json"},
	{Name: profileSummaryFile},
}

func profileFileByName(name string) (string, bool) {
	for _, f := range profileFiles {
		if f.Name == name {
			return f.Name, true
		}
	}
	return "", false
}

func profileFileByKind(kind string) (string, bool) {
	for _, f := range profileFiles {
		if f.Kind != "" && f.Kind == kind {
			return f.Name, true
		}
	}
	return "", false
}

type profileFileInfo struct {
	Name  string `json:"name"`
	Kind  string `json:"kind,omitempty"`
	Bytes int64  `json:"bytes"`
}

// profileHeadline is the handful of numbers the capture list shows without
// opening anything.
type profileHeadline struct {
	WindowSeconds      float64 `json:"window_seconds"`
	SelfCPUPercent     float64 `json:"self_cpu_percent"`
	ChildCPUPercent    float64 `json:"child_cpu_percent"`
	CPUAvailable       bool    `json:"cpu_available"`
	SubprocessesPerSec float64 `json:"subprocesses_per_sec"`
	Goroutines         int     `json:"goroutines"`
	GCPerSec           float64 `json:"gc_per_sec"`
}

type profileCapture struct {
	ID           string            `json:"id"`
	Label        string            `json:"label,omitempty"`
	Source       string            `json:"source"`
	Status       string            `json:"status"`
	Error        string            `json:"error,omitempty"`
	Seconds      int               `json:"seconds"`
	TraceSeconds int               `json:"trace_seconds"`
	StartedAt    time.Time         `json:"started_at"`
	FinishedAt   *time.Time        `json:"finished_at,omitempty"`
	Headline     *profileHeadline  `json:"headline,omitempty"`
	Files        []profileFileInfo `json:"files,omitempty"`
}

// profileStoreDir is a var so tests can point it at a temp directory.
var profileStoreDir = func() string {
	dir := common.TclaudeDataDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "profiles")
}

var profileIDPattern = regexp.MustCompile(`^[0-9]{8}-[0-9]{6}-[0-9a-f]{6}$`)

func newProfileID(now time.Time) string {
	var b [3]byte
	_, _ = rand.Read(b[:])
	return now.Format("20060102-150405") + "-" + hex.EncodeToString(b[:])
}

// profileDir resolves a capture directory, rejecting anything that is not a
// well-formed capture id.
func profileDir(id string) (string, error) {
	if !profileIDPattern.MatchString(id) {
		return "", errors.New("invalid profile id")
	}
	root := profileStoreDir()
	if root == "" {
		return "", errors.New("could not determine profile store directory")
	}
	return filepath.Join(root, id), nil
}

// createProfileDir allocates a fresh capture directory in the store.
func createProfileDir(now time.Time) (string, string, error) {
	root := profileStoreDir()
	if root == "" {
		return "", "", errors.New("could not determine profile store directory")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", "", fmt.Errorf("create profile store: %w", err)
	}
	id := newProfileID(now)
	dir := filepath.Join(root, id)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return "", "", fmt.Errorf("create profile dir: %w", err)
	}
	return id, dir, nil
}

func writeProfileMeta(dir string, c *profileCapture) error {
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, profileMetaFile+".tmp")
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, profileMetaFile))
}

func readProfileMeta(dir string) (*profileCapture, error) {
	raw, err := os.ReadFile(filepath.Join(dir, profileMetaFile))
	if err != nil {
		return nil, err
	}
	var c profileCapture
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// profileFileInventory lists which known artifacts exist in dir.
func profileFileInventory(dir string) []profileFileInfo {
	var out []profileFileInfo
	for _, f := range profileFiles {
		fi, err := os.Stat(filepath.Join(dir, f.Name))
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		out = append(out, profileFileInfo{Name: f.Name, Kind: f.Kind, Bytes: fi.Size()})
	}
	return out
}

func profileHeadlineFrom(before, after perfstats.Snapshot) *profileHeadline {
	window := after.TakenAt.Sub(before.TakenAt).Seconds()
	if window <= 0 {
		window = 1
	}
	h := &profileHeadline{
		WindowSeconds:      window,
		CPUAvailable:       after.CPU.Available,
		SubprocessesPerSec: float64(after.SubprocessTotal-before.SubprocessTotal) / window,
		Goroutines:         after.Goroutines,
		GCPerSec:           float64(after.NumGC-before.NumGC) / window,
	}
	if after.CPU.Available {
		self := after.CPU.SelfUserSeconds + after.CPU.SelfSystemSeconds -
			before.CPU.SelfUserSeconds - before.CPU.SelfSystemSeconds
		child := after.CPU.ChildrenUserSeconds + after.CPU.ChildrenSysSeconds -
			before.CPU.ChildrenUserSeconds - before.CPU.ChildrenSysSeconds
		h.SelfCPUPercent = 100 * self / window
		h.ChildCPUPercent = 100 * child / window
	}
	return h
}

// finishProfileCapture writes the summary and the final meta.json for a
// capture whose artifacts are already on disk. extraSummary is appended to
// summary.txt (the CLI adds `go tool pprof -top` output there).
func finishProfileCapture(dir string, c *profileCapture, before, after perfstats.Snapshot, extraSummary string) error {
	summary := formatProfileSummary(before, after)
	if extraSummary != "" {
		summary += "\n" + extraSummary
	}
	if err := os.WriteFile(filepath.Join(dir, profileSummaryFile), []byte(summary), 0o600); err != nil {
		return err
	}
	now := time.Now()
	c.Status = profileStatusDone
	c.FinishedAt = &now
	c.Headline = profileHeadlineFrom(before, after)
	c.Files = profileFileInventory(dir)
	return writeProfileMeta(dir, c)
}

func failProfileCapture(dir string, c *profileCapture, err error) {
	now := time.Now()
	c.Status = profileStatusFailed
	c.Error = err.Error()
	c.FinishedAt = &now
	c.Files = profileFileInventory(dir)
	if werr := writeProfileMeta(dir, c); werr != nil {
		slog.Warn("profile capture: could not record failure", "id", c.ID, "error", werr)
	}
}

// profileRunner serialises in-process captures: the Go runtime allows only
// one CPU profile and one execution trace at a time per process.
var profileRunner struct {
	mu      sync.Mutex
	running string
}

func runningProfileID() string {
	profileRunner.mu.Lock()
	defer profileRunner.mu.Unlock()
	return profileRunner.running
}

var errProfileBusy = errors.New("a profile capture is already running")

type profileRequest struct {
	Seconds      int    `json:"seconds"`
	TraceSeconds *int   `json:"trace_seconds"`
	Label        string `json:"label"`
}

// normalize applies defaults and bounds. A nil TraceSeconds means "default";
// an explicit 0 disables the trace.
func (r profileRequest) normalize() (secs, traceSecs int, label string, err error) {
	secs = r.Seconds
	if secs == 0 {
		secs = profileDefaultSeconds
	}
	if secs < 1 || secs > profileMaxSeconds {
		return 0, 0, "", fmt.Errorf("seconds must be between 1 and %d", profileMaxSeconds)
	}
	traceSecs = profileDefaultTraceSec
	if r.TraceSeconds != nil {
		traceSecs = *r.TraceSeconds
	}
	if traceSecs < 0 {
		return 0, 0, "", errors.New("trace_seconds must not be negative")
	}
	traceSecs = min(traceSecs, secs)
	label = strings.Join(strings.Fields(r.Label), " ")
	if len([]rune(label)) > profileMaxLabelLen {
		label = string([]rune(label)[:profileMaxLabelLen])
	}
	return secs, traceSecs, label, nil
}

// startInProcessProfile begins a capture of this daemon and returns its
// initial record immediately; the capture finishes in the background.
func startInProcessProfile(req profileRequest) (*profileCapture, error) {
	secs, traceSecs, label, err := req.normalize()
	if err != nil {
		return nil, err
	}
	profileRunner.mu.Lock()
	defer profileRunner.mu.Unlock()
	if profileRunner.running != "" {
		return nil, errProfileBusy
	}
	now := time.Now()
	id, dir, err := createProfileDir(now)
	if err != nil {
		return nil, err
	}
	c := &profileCapture{
		ID: id, Label: label, Source: "dashboard", Status: profileStatusRunning,
		Seconds: secs, TraceSeconds: traceSecs, StartedAt: now,
	}
	if err := writeProfileMeta(dir, c); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	// Start the CPU profile synchronously so "another profile is already
	// running" (e.g. a concurrent /debug/pprof/profile) is reported to the
	// caller instead of surfacing later as a failed capture.
	cpuFile, err := os.OpenFile(filepath.Join(dir, "cpu.pprof"), os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	before := perfstats.Take()
	restoreRates := enableContentionProfiling()
	baselines := snapshotDeltaBaselines()
	if err := pprof.StartCPUProfile(cpuFile); err != nil {
		restoreRates()
		_ = cpuFile.Close()
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("start CPU profile: %w", err)
	}
	profileRunner.running = id
	snapshot := *c
	go func() {
		defer func() {
			profileRunner.mu.Lock()
			profileRunner.running = ""
			profileRunner.mu.Unlock()
		}()
		defer restoreRates()
		runInProcessProfile(dir, c, cpuFile, before, baselines)
	}()
	return &snapshot, nil
}

func runInProcessProfile(dir string, c *profileCapture, cpuFile *os.File, before perfstats.Snapshot, baselines map[string]*profile.Profile) {
	var traceErr error
	if c.TraceSeconds > 0 {
		traceErr = captureTrace(filepath.Join(dir, "trace.out"), time.Duration(c.TraceSeconds)*time.Second)
		time.Sleep(time.Duration(c.Seconds-c.TraceSeconds) * time.Second)
	} else {
		time.Sleep(time.Duration(c.Seconds) * time.Second)
	}
	pprof.StopCPUProfile()
	if err := cpuFile.Close(); err != nil {
		failProfileCapture(dir, c, fmt.Errorf("write cpu profile: %w", err))
		return
	}
	after := perfstats.Take()
	var notes []string
	if traceErr != nil {
		notes = append(notes, "trace capture failed: "+traceErr.Error())
		_ = os.Remove(filepath.Join(dir, "trace.out"))
	}
	for _, f := range profileFiles {
		if f.Lookup == "" {
			continue
		}
		var err error
		if f.Delta {
			err = writeDeltaProfile(filepath.Join(dir, f.Name), f.Lookup, baselines[f.Lookup])
		} else {
			err = writeLookupProfile(filepath.Join(dir, f.Name), f.Lookup, f.Debug)
		}
		if err != nil {
			notes = append(notes, fmt.Sprintf("%s: %v", f.Name, err))
		}
	}
	for _, s := range []struct {
		name string
		snap perfstats.Snapshot
	}{{"stats-before.json", before}, {"stats-after.json", after}} {
		raw, _ := json.MarshalIndent(s.snap, "", "  ")
		if err := os.WriteFile(filepath.Join(dir, s.name), raw, 0o600); err != nil {
			notes = append(notes, fmt.Sprintf("%s: %v", s.name, err))
		}
	}
	extra := ""
	if len(notes) > 0 {
		extra = "warnings:\n  " + strings.Join(notes, "\n  ") + "\n"
	}
	if err := finishProfileCapture(dir, c, before, after, extra); err != nil {
		failProfileCapture(dir, c, err)
		return
	}
	slog.Info("profile capture finished", "id", c.ID, "seconds", c.Seconds)
}

func captureTrace(path string, d time.Duration) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if err := trace.Start(f); err != nil {
		_ = f.Close()
		return err
	}
	time.Sleep(d)
	trace.Stop()
	return f.Close()
}

func writeLookupProfile(path, name string, debug int) error {
	p := pprof.Lookup(name)
	if p == nil {
		return fmt.Errorf("no %s profile", name)
	}
	if name == "heap" {
		// Match /debug/pprof/heap?gc=1 semantics: report live objects as of now.
		runtime.GC()
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if err := p.WriteTo(f, debug); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// snapshotDeltaBaselines records the cumulative block/mutex profiles at the
// start of a capture. The runtime never resets them, so without a baseline
// every capture would include contention since daemon start.
func snapshotDeltaBaselines() map[string]*profile.Profile {
	out := map[string]*profile.Profile{}
	for _, f := range profileFiles {
		if !f.Delta {
			continue
		}
		var buf bytes.Buffer
		if p := pprof.Lookup(f.Lookup); p != nil && p.WriteTo(&buf, 0) == nil {
			if parsed, err := profile.Parse(&buf); err == nil {
				out[f.Lookup] = parsed
			}
		}
	}
	return out
}

// writeDeltaProfile writes name's profile minus base, the same delta
// net/http/pprof serves for ?seconds=N.
func writeDeltaProfile(path, name string, base *profile.Profile) error {
	if base == nil {
		return fmt.Errorf("no baseline for %s profile", name)
	}
	p := pprof.Lookup(name)
	if p == nil {
		return fmt.Errorf("no %s profile", name)
	}
	var buf bytes.Buffer
	if err := p.WriteTo(&buf, 0); err != nil {
		return err
	}
	now, err := profile.Parse(&buf)
	if err != nil {
		return err
	}
	base = base.Copy()
	base.Scale(-1)
	delta, err := profile.Merge([]*profile.Profile{base, now})
	if err != nil {
		return err
	}
	delta.TimeNanos = now.TimeNanos
	delta.DurationNanos = now.TimeNanos - base.TimeNanos
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if err := delta.Write(f); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// contentionProfiling tracks whether block/mutex sampling is on for the
// daemon's lifetime (serve --pprof) so a dashboard capture does not switch it
// off underneath the profiling endpoint when it finishes.
var contentionProfiling struct {
	mu      sync.Mutex
	pinned  bool
	holders int
}

// Block events are sampled about once per 10µs of blocked time and mutex
// contention 1 in 100: cheap enough while someone is measuring, and only on
// while someone is.
const (
	blockProfileRate     = 10_000
	mutexProfileFraction = 100
)

func pinContentionProfiling() {
	contentionProfiling.mu.Lock()
	defer contentionProfiling.mu.Unlock()
	contentionProfiling.pinned = true
	runtime.SetBlockProfileRate(blockProfileRate)
	runtime.SetMutexProfileFraction(mutexProfileFraction)
}

// enableContentionProfiling turns block/mutex sampling on for one capture and
// returns the func that releases it.
func enableContentionProfiling() func() {
	contentionProfiling.mu.Lock()
	contentionProfiling.holders++
	runtime.SetBlockProfileRate(blockProfileRate)
	runtime.SetMutexProfileFraction(mutexProfileFraction)
	contentionProfiling.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			contentionProfiling.mu.Lock()
			defer contentionProfiling.mu.Unlock()
			contentionProfiling.holders--
			if contentionProfiling.holders == 0 && !contentionProfiling.pinned {
				runtime.SetBlockProfileRate(0)
				runtime.SetMutexProfileFraction(0)
			}
		})
	}
}

// listProfileCaptures returns every capture in the store, newest first. A
// "running" record that this process is not running belongs to a daemon or
// CLI run that ended mid-capture and is reported as interrupted — unless it
// is a CLI capture that is still recent enough to be in flight.
func listProfileCaptures() ([]profileCapture, error) {
	root := profileStoreDir()
	if root == "" {
		return nil, errors.New("could not determine profile store directory")
	}
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return []profileCapture{}, nil
	}
	if err != nil {
		return nil, err
	}
	running := runningProfileID()
	out := []profileCapture{}
	for _, e := range entries {
		if !e.IsDir() || !profileIDPattern.MatchString(e.Name()) {
			continue
		}
		c, err := readProfileMeta(filepath.Join(root, e.Name()))
		if err != nil {
			continue
		}
		c.ID = e.Name()
		// Re-read the running id: a capture that started after the first read
		// already has its "running" meta.json on disk.
		if c.Status == profileStatusRunning && c.ID != running && c.ID != runningProfileID() && profileCaptureStale(c) {
			c.Status = profileStatusInterrupted
		}
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.After(out[j].StartedAt) })
	return out, nil
}

// profileCaptureStale reports whether a running record has outlived any
// capture that could still be writing it.
func profileCaptureStale(c *profileCapture) bool {
	if c.Source == "dashboard" {
		return true
	}
	return time.Since(c.StartedAt) > time.Duration(c.Seconds)*time.Second+2*time.Minute
}

func deleteProfileCapture(id string) error {
	dir, err := profileDir(id)
	if err != nil {
		return err
	}
	if id == runningProfileID() {
		return errProfileBusy
	}
	c, err := readProfileMeta(dir)
	if err != nil {
		return err
	}
	// A CLI capture is written by another process; refuse while it can still
	// be fetching artifacts into this directory.
	if c.Status == profileStatusRunning && c.Source != "dashboard" && !profileCaptureStale(c) {
		return errProfileBusy
	}
	dropProfileViewers(id)
	return os.RemoveAll(dir)
}
