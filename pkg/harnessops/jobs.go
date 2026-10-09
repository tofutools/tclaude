package harnessops

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/harnesscredentials"
	"github.com/tofutools/tclaude/pkg/nodeinfo"
)

var ErrBusy = errors.New("a harness operation is already running")
var ErrWorkersBusy = errors.New("harness has active sessions; explicitly choose now or when_idle")
var jobID = regexp.MustCompile(`^[a-f0-9]{32}$`)

type Request struct {
	Action               string                     `json:"action"`
	Harness              string                     `json:"harness,omitempty"`
	All                  bool                       `json:"all,omitempty"`
	Mode                 string                     `json:"mode,omitempty"`
	CopyCredentials      bool                       `json:"copy_credentials,omitempty"`
	OverwriteCredentials bool                       `json:"overwrite_credentials,omitempty"`
	Credentials          *harnesscredentials.Bundle `json:"credentials,omitempty"`
}

func (r Request) Validate(peer bool) error {
	if r.Action != "install" && r.Action != "update" {
		return fmt.Errorf("action must be install or update")
	}
	if r.All && r.Harness != "" || !r.All && r.Harness == "" || r.All && r.Action != "update" {
		return fmt.Errorf("select one harness or update all")
	}
	if !r.All {
		if _, err := recipe(r.Harness); err != nil {
			return err
		}
	}
	if r.Mode != "" && r.Mode != "now" && r.Mode != "when_idle" {
		return fmt.Errorf("mode must be now or when_idle")
	}
	if r.CopyCredentials && (r.Action != "install" || r.All) {
		return fmt.Errorf("credential copy is an explicit single-harness install option")
	}
	if r.OverwriteCredentials && !r.CopyCredentials {
		return fmt.Errorf("credential overwrite requires copy_credentials")
	}
	if r.Credentials != nil {
		if !peer || !r.CopyCredentials || r.Credentials.Harness != r.Harness {
			return fmt.Errorf("credential contents accepted only from the encrypted install transport")
		}
		if err := r.Credentials.Validate(); err != nil {
			return err
		}
	}
	if peer && r.CopyCredentials && r.Credentials == nil {
		return fmt.Errorf("sender credential bundle required")
	}
	return nil
}

type Log struct {
	At      time.Time `json:"at"`
	Message string    `json:"message"`
}
type Result struct {
	Recipe        string                      `json:"recipe,omitempty"`
	Harness       string                      `json:"harness"`
	State         string                      `json:"state"`
	Error         string                      `json:"error,omitempty"`
	ManualCommand string                      `json:"manual_command,omitempty"`
	Credentials   *harnesscredentials.Receipt `json:"credentials,omitempty"`
}
type Job struct {
	ID           string                 `json:"id"`
	Actor        string                 `json:"actor"`
	Action       string                 `json:"action"`
	Harnesses    []string               `json:"harnesses"`
	Mode         string                 `json:"mode,omitempty"`
	State        string                 `json:"state"`
	Phase        string                 `json:"phase"`
	StartedAt    time.Time              `json:"started_at"`
	FinishedAt   *time.Time             `json:"finished_at,omitempty"`
	Warnings     []string               `json:"warnings"`
	Log          []Log                  `json:"log"`
	Results      []Result               `json:"results"`
	Error        string                 `json:"error,omitempty"`
	Availability *nodeinfo.Availability `json:"availability,omitempty"`
}
type Hooks struct {
	Busy     func(string) (bool, error)
	Probe    func() nodeinfo.Availability
	Finished func(Job)
}
type Service struct {
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	closed    bool
	mu        sync.Mutex
	dir, home string
	active    bool
	hooks     Hooks
	plan      func(context.Context, string, string, string) (Command, error)
	execute   func(context.Context, Command) error
	receive   func(string, string, harnesscredentials.Bundle, bool, func() bool) (harnesscredentials.Receipt, error)
}

func New(dir, home string, h Hooks) (*Service, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Service{ctx: ctx, cancel: cancel, dir: dir, home: home, hooks: h, plan: Plan, execute: Execute, receive: harnesscredentials.Receive}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".json" && jobID.MatchString(e.Name()[:len(e.Name())-5]) {
			j, err := s.Job(e.Name()[:len(e.Name())-5])
			if err != nil {
				return nil, err
			}
			if j.State == "running" || j.State == "waiting_idle" {
				j.State = "failed"
				j.Error = "daemon restarted during operation; inspect availability before retrying"
				now := time.Now().UTC()
				j.FinishedAt = &now
				if err := s.save(j); err != nil {
					return nil, err
				}
			}
		}
	}
	return s, nil
}
func (s *Service) Job(id string) (Job, error) {
	if !jobID.MatchString(id) {
		return Job{}, fmt.Errorf("invalid harness job id")
	}
	raw, err := os.ReadFile(filepath.Join(s.dir, id+".json"))
	if err != nil {
		return Job{}, err
	}
	if len(raw) > 1<<20 {
		return Job{}, fmt.Errorf("job metadata exceeds limit")
	}
	var j Job
	err = json.Unmarshal(raw, &j)
	return j, err
}
func (s *Service) save(j Job) error {
	raw, err := json.Marshal(j)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(s.dir, ".job-")
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
	if err := os.Rename(name, filepath.Join(s.dir, j.ID+".json")); err != nil {
		return err
	}
	d, err := os.Open(s.dir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}
func (s *Service) phase(j *Job, phase string) error {
	j.Phase = phase
	j.Log = append(j.Log, Log{time.Now().UTC(), phase})
	if len(j.Log) > 100 {
		j.Log = j.Log[len(j.Log)-100:]
	}
	return s.save(*j)
}
func (s *Service) Start(r Request, actor string, authorize func(bool) bool) (Job, error) {
	if err := r.Validate(r.Credentials != nil); err != nil {
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
	names := []string{r.Harness}
	if r.All {
		names = []string{}
		for _, h := range s.hooks.Probe().Harnesses {
			if h.Installed {
				if _, err := recipe(h.Name); err == nil {
					names = append(names, h.Name)
				}
			}
		}
		if len(names) == 0 {
			return Job{}, fmt.Errorf("no installed harnesses to update")
		}
	}
	warnings := []string{}
	for _, name := range names {
		busy, err := s.hooks.Busy(name)
		if err != nil {
			return Job{}, fmt.Errorf("could not confirm harness activity")
		}
		if busy && r.Mode == "" {
			return Job{}, ErrWorkersBusy
		}
		if busy {
			warnings = append(warnings, name+" has active sessions; updating may affect running agents")
		}
	}
	if !authorize(r.CopyCredentials) {
		return Job{}, fmt.Errorf("harness operation authority revoked")
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return Job{}, err
	}
	j := Job{ID: hex.EncodeToString(nonce[:]), Actor: actor, Action: r.Action, Harnesses: names, Mode: r.Mode, State: "running", Phase: "queued", StartedAt: time.Now().UTC(), Warnings: warnings, Log: []Log{}, Results: []Result{}}
	if r.CopyCredentials {
		j.Warnings = append(j.Warnings, "Credential copy: target agents will act as you with this provider")
	}
	if err := s.save(j); err != nil {
		return Job{}, err
	}
	s.active = true
	s.wg.Add(1)
	go s.run(r, j, authorize)
	return j, nil
}
func (s *Service) run(r Request, j Job, authorize func(bool) bool) {
	defer func() { s.mu.Lock(); s.active = false; s.mu.Unlock(); s.wg.Done() }()
	ctx, cancel := context.WithTimeout(s.ctx, 30*time.Minute)
	defer cancel()
	var final error
	for _, name := range j.Harnesses {
		result := Result{Harness: name, State: "failed"}
		err := s.perform(ctx, r, &j, name, &result, authorize)
		if err == nil {
			result.State = "succeeded"
		} else {
			result.Error = err.Error()
			var manual *ManualRequired
			if errors.As(err, &manual) {
				result.State = "manual_required"
				result.ManualCommand = manual.Command
			}
			final = errors.Join(final, err)
		}
		j.Results = append(j.Results, result)
		if err := s.save(j); err != nil {
			final = errors.Join(final, fmt.Errorf("could not persist harness result"))
			break
		}
	}
	availability := s.hooks.Probe()
	j.Availability = &availability
	now := time.Now().UTC()
	j.FinishedAt = &now
	j.State = "succeeded"
	j.Phase = "complete"
	if final != nil {
		j.State = "failed"
		j.Error = final.Error()
	}
	if err := s.save(j); err != nil {
		j.State = "failed"
		j.Error = "could not persist harness result"
		_ = s.save(j)
	}
	if s.hooks.Finished != nil {
		s.hooks.Finished(j)
	}
}
func (s *Service) perform(ctx context.Context, r Request, j *Job, name string, result *Result, authorize func(bool) bool) error {
	if r.Mode == "when_idle" {
		j.State = "waiting_idle"
		if err := s.phase(j, "waiting_idle:"+name); err != nil {
			return err
		}
		for {
			if !authorize(r.CopyCredentials) {
				return fmt.Errorf("harness operation authority revoked")
			}
			busy, err := s.hooks.Busy(name)
			if err != nil {
				return fmt.Errorf("could not confirm harness activity")
			}
			if !busy {
				break
			}
			select {
			case <-ctx.Done():
				return fmt.Errorf("timed out waiting for idle harness")
			case <-time.After(5 * time.Second):
			}
		}
	}
	j.State = "running"
	if !authorize(r.CopyCredentials) {
		return fmt.Errorf("harness operation authority revoked")
	}
	if err := s.phase(j, "planning:"+name); err != nil {
		return err
	}
	command, err := s.plan(ctx, s.home, r.Action, name)
	if err != nil {
		return err
	}
	busy, err := s.hooks.Busy(name)
	if err != nil {
		return fmt.Errorf("could not confirm harness activity")
	}
	if busy && r.Mode != "now" {
		return ErrWorkersBusy
	}
	if !authorize(r.CopyCredentials) {
		return fmt.Errorf("harness operation authority revoked")
	}
	if err := s.phase(j, "official_recipe:"+name); err != nil {
		return err
	}
	recipeInfo, _ := recipe(name)
	result.Recipe = "npm:" + recipeInfo.Package + "@latest"
	if len(command.Args) == 1 {
		result.Recipe = name + ":" + command.Args[0]
	}
	if err := s.execute(ctx, command); err != nil {
		return err
	}
	if r.CopyCredentials {
		if r.Credentials == nil {
			return fmt.Errorf("credential copy requires an encrypted sender bundle")
		}
		if err := s.phase(j, "copying_credentials:"+name); err != nil {
			return err
		}
		receipt, err := s.receive(s.home, filepath.Join(s.dir, "credential-backups"), *r.Credentials, r.OverwriteCredentials, func() bool { return authorize(true) })
		result.Credentials = &receipt
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) Close() { s.mu.Lock(); s.closed = true; s.cancel(); s.mu.Unlock(); s.wg.Wait() }
