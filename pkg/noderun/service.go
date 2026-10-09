// Package noderun retains operator script jobs and bounded output. The runner
// owns subprocess isolation; this service owns durable identity and authority.
package noderun

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const MaxScriptBytes = 16 << 10
const MaxOutputBytes = 4 << 20
const TailBytes = 8 << 10

var ErrBusy = errors.New("node script workers busy")

type Request struct {
	Script         string `json:"script"`
	TimeoutSeconds int64  `json:"timeout_seconds,omitempty"`
}

func (r *Request) Validate() error {
	if strings.TrimSpace(r.Script) == "" || strings.ContainsRune(r.Script, 0) || len(r.Script) > MaxScriptBytes {
		return fmt.Errorf("script must be nonempty, contain no NUL, and fit 16 KiB")
	}
	if r.TimeoutSeconds == 0 {
		r.TimeoutSeconds = 3600
	}
	if r.TimeoutSeconds < 1 || r.TimeoutSeconds > 86400 {
		return fmt.Errorf("timeout_seconds must be 1..86400")
	}
	raw, err := json.Marshal(r)
	if err != nil || len(raw) > 32<<10 {
		return fmt.Errorf("encoded script request exceeds 32 KiB transport limit")
	}
	return nil
}

type Job struct {
	ID             string    `json:"id"`
	Actor          string    `json:"actor"`
	Peer           string    `json:"peer,omitempty"`
	Node           string    `json:"node"`
	State          string    `json:"state"`
	ScriptSHA256   string    `json:"script_sha256"`
	ScriptBytes    int       `json:"script_bytes"`
	TimeoutSeconds int64     `json:"timeout_seconds"`
	CreatedAt      time.Time `json:"created_at"`
	FinishedAt     time.Time `json:"finished_at,omitempty"`
	DurationMS     int64     `json:"duration_ms"`
	ExitCode       int       `json:"exit_code"`
	Error          string    `json:"error,omitempty"`
	StdoutTail     string    `json:"stdout_tail"`
	StderrTail     string    `json:"stderr_tail"`
}
type Result struct {
	Stdout, Stderr string
	ExitCode       int
	Error          string
}
type Execute func(context.Context, string, string, int64) Result
type Service struct {
	mu       sync.Mutex
	dir      string
	jobs     map[string]Job
	active   map[string]context.CancelFunc
	closed   bool
	wg       sync.WaitGroup
	execute  Execute
	finished func(Job)
}

func ValidID(id string) bool { return len(id) == 32 && strings.Trim(id, "0123456789abcdef") == "" }
func New(dir string, execute Execute, finished func(Job)) (*Service, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	s := &Service{dir: dir, jobs: map[string]Job{}, active: map[string]context.CancelFunc{}, execute: execute, finished: finished}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if !entry.IsDir() || !ValidID(entry.Name()) {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, entry.Name(), "job.json"))
		if err != nil || len(raw) > 64<<10 {
			continue
		}
		var j Job
		if json.Unmarshal(raw, &j) != nil || j.ID != entry.Name() {
			continue
		}
		if j.State == "running" {
			j.State = "interrupted"
			j.Error = "daemon restarted; run was not replayed"
			j.ExitCode = 125
			j.FinishedAt = time.Now().UTC()
			j.DurationMS = j.FinishedAt.Sub(j.CreatedAt).Milliseconds()
			if err := s.save(j); err != nil {
				return nil, err
			}
		}
		s.jobs[j.ID] = j
	}
	return s, nil
}
func (s *Service) save(j Job) error {
	raw, err := json.Marshal(j)
	if err != nil {
		return err
	}
	dir := filepath.Join(s.dir, j.ID)
	f, err := os.CreateTemp(dir, ".job-")
	if err != nil {
		return err
	}
	name := f.Name()
	defer func() { _ = os.Remove(name) }()
	if err := f.Chmod(0600); err != nil {
		_ = f.Close()
		return err
	}
	_, e1 := f.Write(raw)
	e2 := f.Sync()
	e3 := f.Close()
	if err := errors.Join(e1, e2, e3); err != nil {
		return err
	}
	if err := os.Rename(name, filepath.Join(dir, "job.json")); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}
func (s *Service) Start(req Request, actor, peer, node string, authorize func() bool) (Job, error) {
	if err := req.Validate(); err != nil {
		return Job{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || len(s.active) >= 4 {
		return Job{}, ErrBusy
	}
	if !authorize() {
		return Job{}, fmt.Errorf("script authority unavailable")
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return Job{}, err
	}
	id := hex.EncodeToString(nonce[:])
	hash := sha256.Sum256([]byte(req.Script))
	j := Job{ID: id, Actor: actor, Peer: peer, Node: node, State: "running", ScriptSHA256: hex.EncodeToString(hash[:]), ScriptBytes: len(req.Script), TimeoutSeconds: req.TimeoutSeconds, CreatedAt: time.Now().UTC(), ExitCode: -1}
	dir := filepath.Join(s.dir, id)
	if err := os.Mkdir(dir, 0700); err != nil {
		return Job{}, err
	}
	if err := os.WriteFile(filepath.Join(dir, "script.sh"), []byte(req.Script), 0600); err != nil {
		return Job{}, err
	}
	if err := s.save(j); err != nil {
		return Job{}, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.active[id] = cancel
	s.jobs[id] = j
	s.wg.Add(1)
	go s.run(ctx, j, authorize)
	return j, nil
}
func (s *Service) run(ctx context.Context, j Job, authorize func() bool) {
	defer s.wg.Done()
	watchCtx, stop := context.WithCancel(ctx)
	defer stop()
	revoked := make(chan struct{})
	go func() {
		timer := time.NewTicker(500 * time.Millisecond)
		defer timer.Stop()
		for {
			select {
			case <-watchCtx.Done():
				return
			case <-timer.C:
				if !authorize() {
					close(revoked)
					stop()
					return
				}
			}
		}
	}()
	result := Result{ExitCode: 125, Error: "script authority unavailable"}
	if authorize() {
		result = s.execute(watchCtx, filepath.Join(s.dir, j.ID, "script.sh"), j.ID, j.TimeoutSeconds)
	}
	j.ExitCode = result.ExitCode
	j.Error = result.Error
	j.State = "completed"
	if result.ExitCode != 0 || result.Error != "" {
		j.State = "failed"
	}
	select {
	case <-revoked:
		j.State = "canceled"
		j.Error = "script authority revoked"
		j.ExitCode = 130
	default:
	}
	if ctx.Err() != nil {
		j.State = "canceled"
		j.Error = "daemon stopped"
		j.ExitCode = 130
	}
	if len(result.Stdout) > MaxOutputBytes {
		result.Stdout = result.Stdout[:MaxOutputBytes]
	}
	if len(result.Stderr) > MaxOutputBytes+4096 {
		result.Stderr = result.Stderr[:MaxOutputBytes+4096]
	}
	for name, data := range map[string]string{"stdout": result.Stdout, "stderr": result.Stderr} {
		if err := os.WriteFile(filepath.Join(s.dir, j.ID, name+".log"), []byte(data), 0600); err != nil {
			j.State = "failed"
			j.Error = "could not persist script output"
			j.ExitCode = 125
		}
	}
	tail := func(text string) string {
		if len(text) > TailBytes {
			return text[len(text)-TailBytes:]
		}
		return text
	}
	j.StdoutTail = tail(result.Stdout)
	j.StderrTail = tail(result.Stderr)
	j.FinishedAt = time.Now().UTC()
	j.DurationMS = j.FinishedAt.Sub(j.CreatedAt).Milliseconds()
	s.mu.Lock()
	if err := s.save(j); err != nil {
		j.State = "failed"
		j.Error = "could not persist script result"
		j.ExitCode = 125
	}
	s.jobs[j.ID] = j
	cancel := s.active[j.ID]
	delete(s.active, j.ID)
	s.mu.Unlock()
	cancel()
	if s.finished != nil {
		s.finished(j)
	}
}
func (s *Service) Job(id string) (Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	if !ValidID(id) || !ok {
		return Job{}, os.ErrNotExist
	}
	return j, nil
}

type LogChunk struct {
	Data       []byte `json:"data"`
	NextOffset int64  `json:"next_offset"`
	EOF        bool   `json:"eof"`
}

func (s *Service) Log(id, channel string, offset int64) (LogChunk, error) {
	if !ValidID(id) || (channel != "stdout" && channel != "stderr") || offset < 0 || offset > MaxOutputBytes+4096 {
		return LogChunk{}, fmt.Errorf("invalid log request")
	}
	f, err := os.Open(filepath.Join(s.dir, id, channel+".log"))
	if err != nil {
		return LogChunk{}, err
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return LogChunk{}, err
	}
	raw, err := io.ReadAll(io.LimitReader(f, 64<<10))
	if err != nil {
		return LogChunk{}, err
	}
	info, err := f.Stat()
	if err != nil {
		return LogChunk{}, err
	}
	return LogChunk{Data: raw, NextOffset: offset + int64(len(raw)), EOF: offset+int64(len(raw)) >= info.Size()}, nil
}
func (s *Service) Close() {
	s.mu.Lock()
	s.closed = true
	for _, cancel := range s.active {
		cancel()
	}
	s.mu.Unlock()
	s.wg.Wait()
}
