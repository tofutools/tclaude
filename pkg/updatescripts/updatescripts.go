// Package updatescripts runs the operator's own update scripts: a status
// command that reports what is pending and an update command that applies
// it, per named target. The commands come only from this node's local config;
// callers (the dashboard, trusted peers) select a target by name and never
// supply a command.
package updatescripts

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/tofutools/tclaude/pkg/common/executil"
)

const (
	DefaultStatusTimeout = time.Minute
	DefaultUpdateTimeout = 30 * time.Minute
	// ExitUpdatesPending is the plain-text status script's "updates pending"
	// exit code, matching `dnf check-update`.
	ExitUpdatesPending = 100

	outputLimit = 64 << 10
	logTail     = 16 << 10
	maxItems    = 200
	fieldLimit  = 200
	detailLimit = 4 << 10
	keepJobs    = 20
)

var ErrBusy = errors.New("an update script is already running")

// Script is one configured target.
type Script struct {
	Name          string
	Label         string
	Status        string
	Update        string
	StatusTimeout time.Duration
	UpdateTimeout time.Duration
}

// Item is one row a status script reports, such as a package or a tool.
type Item struct {
	Name            string `json:"name"`
	Current         string `json:"current,omitempty"`
	Latest          string `json:"latest,omitempty"`
	UpdateAvailable *bool  `json:"update_available,omitempty"`
	Detail          string `json:"detail,omitempty"`
}

// Status is a target's last known state.
type Status struct {
	Name            string     `json:"name"`
	Label           string     `json:"label"`
	HasStatus       bool       `json:"has_status"`
	HasUpdate       bool       `json:"has_update"`
	StatusCommand   string     `json:"status_command,omitempty"`
	UpdateCommand   string     `json:"update_command,omitempty"`
	State           string     `json:"state"`
	Summary         string     `json:"summary,omitempty"`
	Details         string     `json:"details,omitempty"`
	UpdateAvailable *bool      `json:"update_available"`
	Items           []Item     `json:"items"`
	CheckedAt       *time.Time `json:"checked_at,omitempty"`
	ExitCode        int        `json:"exit_code"`
	Error           string     `json:"error,omitempty"`
}

// Job is one update run.
type Job struct {
	ID         string     `json:"id"`
	Actor      string     `json:"actor"`
	Script     string     `json:"script"`
	State      string     `json:"state"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	ExitCode   int        `json:"exit_code"`
	Output     string     `json:"output,omitempty"`
	Error      string     `json:"error,omitempty"`
}

// Request is a POST body: check every status script (or one by name), or
// run one target's update.
type Request struct {
	Action string `json:"action"`
	Name   string `json:"name,omitempty"`
}

func (r Request) Validate() error {
	if r.Action != "check" && r.Action != "update" {
		return fmt.Errorf("action must be check or update")
	}
	if r.Action == "update" && r.Name == "" {
		return fmt.Errorf("update names one script")
	}
	if len(r.Name) > 40 {
		return fmt.Errorf("unknown update script")
	}
	return nil
}

// Overview is what GET returns.
type Overview struct {
	Scripts []Status `json:"scripts"`
	Jobs    []Job    `json:"jobs"`
	Error   string   `json:"error,omitempty"`
}

// Runner runs one shell command and returns its stdout, stderr and exit code.
type Runner func(ctx context.Context, command string, env []string) (stdout, stderr []byte, exitCode int, err error)

type Hooks struct {
	// Scripts reads the current configuration; it is re-read on every call so
	// config edits apply without a daemon restart.
	Scripts  func() ([]Script, error)
	Finished func(Job)
}

type Service struct {
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	mu       sync.Mutex
	closed   bool
	hooks    Hooks
	run      Runner
	status   map[string]Status
	checking map[string]bool
	jobs     []Job
	active   bool
}

func New(h Hooks, run Runner) *Service {
	if run == nil {
		run = ShellRunner("")
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Service{ctx: ctx, cancel: cancel, hooks: h, run: run, status: map[string]Status{}, checking: map[string]bool{}}
}

func (s *Service) scripts() ([]Script, error) {
	if s.hooks.Scripts == nil {
		return nil, nil
	}
	return s.hooks.Scripts()
}

// Overview reports every configured target with its last status, and starts
// a check for a status script that has never run.
func (s *Service) Overview() Overview {
	scripts, err := s.scripts()
	out := Overview{Scripts: []Status{}, Jobs: []Job{}}
	if err != nil {
		out.Error = "update script settings unavailable: " + err.Error()
		return out
	}
	s.mu.Lock()
	var unchecked []Script
	for _, sc := range scripts {
		st, ok := s.status[sc.Name]
		if !ok {
			st = Status{State: "unchecked", Items: []Item{}}
			if sc.Status != "" && !s.checking[sc.Name] {
				unchecked = append(unchecked, sc)
			}
		}
		if s.checking[sc.Name] {
			st.State = "checking"
		}
		st.Name, st.Label, st.HasStatus, st.HasUpdate = sc.Name, sc.Label, sc.Status != "", sc.Update != ""
		st.StatusCommand, st.UpdateCommand = sc.Status, sc.Update
		if st.Label == "" {
			st.Label = sc.Name
		}
		out.Scripts = append(out.Scripts, st)
	}
	for i := len(s.jobs) - 1; i >= 0; i-- {
		out.Jobs = append(out.Jobs, s.jobs[i])
	}
	s.mu.Unlock()
	for _, sc := range unchecked {
		s.check(sc)
	}
	for i := range out.Scripts {
		if slices.ContainsFunc(unchecked, func(sc Script) bool { return sc.Name == out.Scripts[i].Name }) {
			out.Scripts[i].State = "checking"
		}
	}
	return out
}

// Job returns one update job by id.
func (s *Service) Job(id string) (Job, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, j := range s.jobs {
		if j.ID == id {
			return j, true
		}
	}
	return Job{}, false
}

// Check starts status runs for every status script, or the one named.
func (s *Service) Check(name string) error {
	scripts, err := s.scripts()
	if err != nil {
		return err
	}
	found := false
	for _, sc := range scripts {
		if sc.Status == "" || name != "" && sc.Name != name {
			continue
		}
		found = true
		s.check(sc)
	}
	if name != "" && !found {
		return fmt.Errorf("no status script named %q", name)
	}
	return nil
}

// check runs one status script in the background unless it already runs.
func (s *Service) check(sc Script) {
	s.mu.Lock()
	if s.closed || s.checking[sc.Name] {
		s.mu.Unlock()
		return
	}
	s.checking[sc.Name] = true
	s.wg.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.wg.Done()
		st := s.runStatus(sc)
		s.mu.Lock()
		delete(s.checking, sc.Name)
		s.status[sc.Name] = st
		s.mu.Unlock()
	}()
}

func (s *Service) runStatus(sc Script) Status {
	timeout := sc.StatusTimeout
	if timeout <= 0 {
		timeout = DefaultStatusTimeout
	}
	ctx, cancel := context.WithTimeout(s.ctx, timeout)
	defer cancel()
	stdout, stderr, code, err := s.run(ctx, sc.Status, scriptEnv(sc.Name, "status"))
	now := time.Now().UTC()
	st := Status{CheckedAt: &now, ExitCode: code, Items: []Item{}}
	if err != nil {
		st.State, st.Error = "failed", err.Error()
		if ctx.Err() == context.DeadlineExceeded {
			st.Error = fmt.Sprintf("status script timed out after %s", timeout)
		}
		return st
	}
	if parsed, perr := ParseStatus(stdout, code); perr != nil {
		st.State, st.Error = "failed", perr.Error()
		if tail := clip(strings.TrimSpace(string(stderr)), detailLimit); tail != "" {
			st.Details = tail
		}
	} else {
		parsed.CheckedAt, parsed.ExitCode = st.CheckedAt, code
		st = parsed
	}
	return st
}

// ParseStatus reads a status script's stdout. JSON output is
// {"summary","update_available","details","items":[{"name","current",
// "latest","update_available","detail"}]}; any other output is plain text
// whose first line is the summary and whose rest is the details, with exit
// 0 meaning up to date and ExitUpdatesPending meaning updates are pending.
// Any other exit code is a failed check.
func ParseStatus(stdout []byte, code int) (Status, error) {
	st := Status{State: "ok", Items: []Item{}}
	if code != 0 && code != ExitUpdatesPending {
		return st, fmt.Errorf("status script exited %d", code)
	}
	text := strings.TrimSpace(string(stdout))
	if strings.HasPrefix(text, "{") {
		var raw struct {
			Summary         string `json:"summary"`
			Details         string `json:"details"`
			UpdateAvailable *bool  `json:"update_available"`
			Items           []Item `json:"items"`
		}
		if err := json.Unmarshal([]byte(text), &raw); err != nil {
			return st, fmt.Errorf("status script printed invalid JSON: %v", err)
		}
		st.Summary, st.Details, st.UpdateAvailable = clip(raw.Summary, fieldLimit), clip(raw.Details, detailLimit), raw.UpdateAvailable
		for i, it := range raw.Items {
			if i == maxItems {
				break
			}
			if it.Name = clip(it.Name, fieldLimit); it.Name == "" {
				continue
			}
			it.Current, it.Latest, it.Detail = clip(it.Current, fieldLimit), clip(it.Latest, fieldLimit), clip(it.Detail, fieldLimit)
			st.Items = append(st.Items, it)
		}
		if st.UpdateAvailable == nil {
			st.UpdateAvailable = derivePending(st.Items, code)
		}
		return st, nil
	}
	first, rest, _ := strings.Cut(text, "\n")
	st.Summary, st.Details = clip(strings.TrimSpace(first), fieldLimit), clip(strings.TrimSpace(rest), detailLimit)
	pending := code == ExitUpdatesPending
	st.UpdateAvailable = &pending
	return st, nil
}

// derivePending answers update_available for JSON output that left it out:
// exit ExitUpdatesPending means pending; otherwise the items' own flags
// decide, and with none of those it is unknown.
func derivePending(items []Item, code int) *bool {
	pending := code == ExitUpdatesPending
	known := pending
	for _, it := range items {
		if it.UpdateAvailable != nil {
			known = true
			pending = pending || *it.UpdateAvailable
		}
	}
	if !known {
		return nil
	}
	return &pending
}

// Start runs one target's update command in the background. One update runs
// at a time; when it finishes, that target's status script runs again.
func (s *Service) Start(name, actor string) (Job, error) {
	scripts, err := s.scripts()
	if err != nil {
		return Job{}, err
	}
	i := slices.IndexFunc(scripts, func(sc Script) bool { return sc.Name == name })
	if i < 0 || scripts[i].Update == "" {
		return Job{}, fmt.Errorf("no update script named %q", name)
	}
	sc := scripts[i]
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return Job{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return Job{}, fmt.Errorf("daemon is shutting down")
	}
	if s.active {
		return Job{}, ErrBusy
	}
	j := Job{ID: hex.EncodeToString(nonce[:]), Actor: actor, Script: name, State: "running", StartedAt: time.Now().UTC()}
	s.jobs = append(s.jobs, j)
	if len(s.jobs) > keepJobs {
		s.jobs = s.jobs[len(s.jobs)-keepJobs:]
	}
	s.active = true
	s.wg.Add(1)
	go s.runUpdate(sc, j)
	return j, nil
}

func (s *Service) runUpdate(sc Script, j Job) {
	defer s.wg.Done()
	timeout := sc.UpdateTimeout
	if timeout <= 0 {
		timeout = DefaultUpdateTimeout
	}
	ctx, cancel := context.WithTimeout(s.ctx, timeout)
	defer cancel()
	stdout, stderr, code, err := s.run(ctx, sc.Update, scriptEnv(sc.Name, "update"))
	now := time.Now().UTC()
	j.FinishedAt, j.ExitCode = &now, code
	j.Output = tail(strings.TrimSpace(string(stdout)+"\n"+string(stderr)), logTail)
	j.State = "succeeded"
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		j.State, j.Error = "failed", fmt.Sprintf("update script timed out after %s", timeout)
	case err != nil:
		j.State, j.Error = "failed", err.Error()
	case code != 0:
		j.State, j.Error = "failed", fmt.Sprintf("update script exited %d", code)
	}
	s.mu.Lock()
	for i := range s.jobs {
		if s.jobs[i].ID == j.ID {
			s.jobs[i] = j
		}
	}
	s.active = false
	s.mu.Unlock()
	if s.hooks.Finished != nil {
		s.hooks.Finished(j)
	}
	if sc.Status != "" {
		s.check(sc)
	}
}

// Close cancels running scripts and waits for them.
func (s *Service) Close() {
	s.mu.Lock()
	s.closed = true
	s.cancel()
	s.mu.Unlock()
	s.wg.Wait()
}

func scriptEnv(name, action string) []string {
	return append(os.Environ(), "TCLAUDE_UPDATE_SCRIPT="+name, "TCLAUDE_UPDATE_ACTION="+action)
}

// ShellRunner runs a command with /bin/sh -c from dir, keeping at most
// outputLimit bytes of each stream. A non-zero exit is reported as the exit
// code, not an error; err means the script could not run or was killed.
func ShellRunner(dir string) Runner {
	return func(ctx context.Context, command string, env []string) ([]byte, []byte, int, error) {
		cmd := executil.CommandContextWithGrace(ctx, 5*time.Second, "/bin/sh", "-c", command)
		cmd.Dir, cmd.Env, cmd.WaitDelay = dir, env, 5*time.Second
		stdout, stderr := &capped{limit: outputLimit}, &capped{limit: outputLimit}
		cmd.Stdout, cmd.Stderr = stdout, stderr
		err := cmd.Run()
		var exit *exec.ExitError
		if errors.As(err, &exit) && ctx.Err() == nil && exit.ExitCode() >= 0 {
			return stdout.Bytes(), stderr.Bytes(), exit.ExitCode(), nil
		}
		if err != nil {
			return stdout.Bytes(), stderr.Bytes(), -1, fmt.Errorf("script did not complete: %w", err)
		}
		return stdout.Bytes(), stderr.Bytes(), 0, nil
	}
}

type capped struct {
	bytes.Buffer
	limit int
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.limit - c.Len(); room > 0 {
		c.Buffer.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func clip(s string, n int) string {
	s = strings.ToValidUTF8(s, "")
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

func tail(s string, n int) string {
	s = strings.ToValidUTF8(s, "")
	if len(s) <= n {
		return s
	}
	cut := len(s) - n
	for cut < len(s) && !utf8.RuneStart(s[cut]) {
		cut++
	}
	return "…" + s[cut:]
}
