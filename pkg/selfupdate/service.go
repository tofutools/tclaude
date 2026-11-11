package selfupdate

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
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/common/buildversion"
	gomodule "golang.org/x/mod/module"
	"golang.org/x/mod/semver"
)

var jobIDPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)
var ErrBusy = errors.New("an update job is already running")

type Hooks struct {
	// Guardian owns health confirmation and recovery; ordinary node updates retain
	// their existing restart verification.
	Supervised  bool
	ReleaseOnly bool
	NoDowngrade bool
	BeforeStart func(Job) error // runs under admission lock; must not re-enter Service
	Started     func(Job) error
	Progress    func(Job)
	Finished    func(Job)
	Restart     func() error
	// ToolEnv returns the environment a new terminal would have, for the
	// custom command and the go builds (launcher.TerminalEnv). nil, or an
	// error (reported as a job warning), runs them with this process's
	// environment.
	ToolEnv func(context.Context) ([]string, error)
}
type Service struct {
	mu           sync.Mutex
	dir          string
	binaries     []Binary
	status       Status
	active       bool
	hooks        Hooks
	release      func(context.Context, string) (Release, error)
	stageRelease func(context.Context, Release, string, string) (string, error)
	// stageGo, stageCheckout and buildCheckout run their tools through
	// the job's toolRunner (Service.runner).
	stageGo func(context.Context, string, string, string, toolRunner) (string, error)
	// stageCheckout builds every binary from a local checkout into dir.
	stageCheckout func(context.Context, CheckoutOptions, string, string, toolRunner) (map[string]string, error)
	// buildCheckout builds a checkout path's binaries into a dir (branch mode).
	buildCheckout func(context.Context, string, string, toolRunner) (map[string]string, error)
	// envJob and env memoize toolEnv for one job, under envMu.
	envMu  sync.Mutex
	envJob string
	// planEnv* keep Plan's last login-shell answer for planEnvTTL, under
	// planEnvMu, so repeated Test method calls (a peer's included) run one
	// shell at a time and not one per call.
	planEnvMu  sync.Mutex
	planEnvAt  time.Time
	planEnvVal []string
	planEnvErr error
	env        []string
	// revisionOf reads a built binary's vcs.revision and vcs.modified.
	revisionOf func(string) (string, bool, error)
	inspect    func(context.Context, string) (buildversion.Info, error)
	// identify confirms an externally installed file is a tclaude binary.
	identify func(string) error
	// checkArgs is the pre-flight check of a new binary against the
	// daemon's current flags.
	checkArgs argsChecker
}

func New(dir string, binaries []Binary, hooks Hooks) (*Service, error) {
	if err := os.MkdirAll(filepath.Join(dir, "jobs"), 0700); err != nil {
		return nil, err
	}
	s := &Service{dir: dir, binaries: binaries, hooks: hooks, release: loadRelease, stageRelease: stageRelease, stageGo: stageGoInstall, stageCheckout: stageCheckout, buildCheckout: buildCheckout, revisionOf: binaryRevision, inspect: inspectStaged, identify: identifyModule, checkArgs: checkLaunchArgs}
	s.status = Status{CurrentVersion: buildversion.AppVersion(), InstallMethod: buildversion.InstallMethod, ProtocolVersion: buildversion.BuildInfo().ProtocolVersion, Binaries: binaries, Warnings: []string{}}
	if s.status.InstallMethod == "" {
		s.status.InstallMethod = "source_or_unmarked"
	}
	for _, b := range binaries {
		if b.Method == "source" {
			s.status.Warnings = append(s.status.Warnings, "Source/dev or unmarked install: applying uses go install @latest (or the explicit version pin); local modifications are replaced.")
			break
		}
	}
	var saved Status
	if readJSON(filepath.Join(dir, "status.json"), &saved) == nil {
		s.status.LatestVersion = saved.LatestVersion
		s.status.CheckedAt = saved.CheckedAt
		if saved.Branch != "" {
			s.status.Branch, s.status.LatestRevision, s.status.PendingCommits, s.status.UpdateAvailable = saved.Branch, saved.LatestRevision, saved.PendingCommits, saved.UpdateAvailable
			if len(binaries) > 0 && primary(binaries).Revision == saved.LatestRevision {
				// This process runs the commit the last check found.
				current := false
				s.status.PendingCommits, s.status.UpdateAvailable = nil, &current
			}
		}
		s.status.Job = saved.Job
	}
	if _, err := os.Stat(filepath.Join(dir, "backup.json")); err == nil {
		s.status.RollbackAvailable = true
	}
	if !hooks.Supervised && s.status.Job != nil && (s.status.Job.State == "running" || s.status.Job.State == "restarting") {
		job := *s.status.Job
		now := time.Now().UTC()
		job.FinishedAt = &now
		if job.State == "restarting" {
			job.State = "succeeded"
			job.Phase = "restarted"
			if err := s.verifyRestart(job); err != nil {
				job.State = "failed"
				job.Error = err.Error()
			}
		} else {
			job.State = "failed"
			job.Error = "daemon exited during update; rollback is available when a backup was made"
			s.endLaunchCheck(&job)
		}
		if err := s.saveJob(job); err != nil {
			return nil, err
		}
	}
	s.updateAvailable()
	return s, nil
}

// A pseudo-version is a development build at the release prefix. Do not
// advertise its matching release as an update merely because semver ranks
// a pseudo-version as a prerelease.
func releaseNewerThanBuild(release, current string) bool {
	if gomodule.IsPseudoVersion(current) {
		current = strings.TrimSuffix(semver.Canonical(current), semver.Prerelease(current))
	}
	return semver.Compare(release, current) > 0
}
func (s *Service) updateAvailable() {
	if s.status.Branch != "" {
		return // a branch check sets it from commits, not release versions
	}
	if semver.IsValid(s.status.CurrentVersion) && semver.IsValid(s.status.LatestVersion) {
		v := releaseNewerThanBuild(s.status.LatestVersion, s.status.CurrentVersion)
		s.status.UpdateAvailable = &v
	} else {
		s.status.UpdateAvailable = nil
	}
}
func (s *Service) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, _ := json.Marshal(s.status)
	var out Status
	_ = json.Unmarshal(raw, &out)
	return out
}
func (s *Service) Job(id string) (Job, error) {
	if !jobIDPattern.MatchString(id) {
		return Job{}, fmt.Errorf("invalid update job id")
	}
	var job Job
	err := readJSON(filepath.Join(s.dir, "jobs", id+".json"), &job)
	return job, err
}
func (s *Service) Start(req Request, actor string, authorize func() bool) (Job, error) {
	return s.StartWith(req, actor, authorize, ApplyOptions{})
}

// StartWith starts a job; opts carry the operator's updates.self choices
// (never request input) and matter only for apply.
func (s *Service) StartWith(req Request, actor string, authorize func() bool, opts ApplyOptions) (Job, error) {
	if err := req.Validate(); err != nil {
		return Job{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hooks.NoDowngrade && req.Action == "apply" {
		if !semver.IsValid(s.status.CurrentVersion) {
			return Job{}, fmt.Errorf("release version unknown; supervised updates require a stamped current release")
		}
		if req.Version != "" && semver.Compare(req.Version, s.status.CurrentVersion) < 0 {
			return Job{}, fmt.Errorf("downgrades are refused; use rollback to restore the journal backup")
		}
	}
	if s.active || s.hooks.Supervised && s.status.Job != nil && (s.status.Job.State == "running" || s.status.Job.State == "restarting") {
		return Job{}, ErrBusy
	}
	if s.status.Job != nil && s.hooks.BeforeStart != nil {
		if err := s.hooks.BeforeStart(*s.status.Job); err != nil {
			return Job{}, err
		}
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return Job{}, err
	}
	job := Job{CurrentVersion: s.status.CurrentVersion, ID: hex.EncodeToString(nonce[:]), Action: req.Action, Version: req.Version, Actor: actor, State: "running", Phase: "queued", StartedAt: time.Now().UTC(), Warnings: append([]string{}, s.status.Warnings...)}
	if req.Action == "apply" {
		if err := s.admit(opts); err != nil {
			return Job{}, err
		}
		job.Method = opts.Method
		if opts.PostSetup != nil {
			job.PostSetup = &PostSetup{State: "pending", Args: append([]string{}, opts.PostSetup...)}
		}
	}
	if req.Action != "check" && opts.Launch != nil {
		// Durable before anything is replaced: the restarted daemon
		// compares itself with it.
		if err := writeJSONFile(s.launchPath(job.ID), opts.Launch.Snapshot); err != nil {
			return Job{}, err
		}
		job.Launch = opts.Launch.Snapshot.Check()
	}
	opts.Force = req.Force
	if s.hooks.Started != nil {
		if err := s.hooks.Started(job); err != nil {
			_ = os.Remove(s.launchPath(job.ID))
			return Job{}, err
		}
	}
	if err := s.saveJob(job); err != nil {
		_ = os.Remove(s.launchPath(job.ID))
		return Job{}, err
	}
	s.active = true
	go s.run(req, job, authorize, opts)
	return job, nil
}
func (s *Service) saveJob(job Job) error {
	if err := writeJSONFile(filepath.Join(s.dir, "jobs", job.ID+".json"), job); err != nil {
		return err
	}
	s.status.Job = &job
	return writeJSONFile(filepath.Join(s.dir, "status.json"), s.status)
}
func (s *Service) phase(job *Job, phase string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	job.Phase = phase
	if err := s.saveJob(*job); err != nil {
		return err
	}
	if s.hooks.Progress != nil {
		s.hooks.Progress(*job)
	}
	return nil
}
func (s *Service) run(req Request, job Job, authorize func() bool, opts ApplyOptions) {
	// A custom command or checkout build may need longer than the default.
	budget := 20 * time.Minute
	if extra := opts.CustomTimeout + 10*time.Minute; opts.Method == MethodCustom && extra > budget {
		budget = extra
	}
	if opts.Method == MethodSourceCheckout {
		budget = 60 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	// The job's tool environment is asked for at its first tool and
	// forgotten with the job: it holds the profile's variables.
	defer s.dropToolEnv()
	var err error
	branch := false
	if req.Action != "rollback" && req.Version == "" {
		branch, err = s.branchMode(ctx, &job, opts)
	}
	switch {
	case err != nil:
	case branch:
		err = s.runBranch(ctx, req, &job, authorize, opts)
	case req.Action == "rollback":
		err = s.rollback(&job, authorize)
	default:
		err = s.phase(&job, "checking_release")
		var release Release
		if err == nil {
			release, err = s.release(ctx, req.Version)
		}
		if err == nil {
			job.Version = release.Tag
			if semver.IsValid(job.CurrentVersion) {
				available := releaseNewerThanBuild(release.Tag, job.CurrentVersion)
				job.UpdateAvailable = &available
			}
			s.mu.Lock()
			now := time.Now().UTC()
			if req.Version == "" {
				s.status.LatestVersion = release.Tag
				s.status.CheckedAt = &now
			}
			s.status.Branch, s.status.LatestRevision, s.status.PendingCommits = "", "", nil
			s.updateAvailable()
			err = s.saveJob(job)
			s.mu.Unlock()
		}
		if err == nil && req.Action == "apply" && s.hooks.NoDowngrade && semver.Compare(release.Tag, job.CurrentVersion) < 0 {
			err = fmt.Errorf("downgrades are refused; use rollback to restore the journal backup")
		}
		if err == nil && req.Action == "apply" {
			err = s.apply(ctx, release, &job, authorize, req.Version, opts)
		}
	}
	s.mu.Lock()
	now := time.Now().UTC()
	job.FinishedAt = &now
	if err != nil || !job.RestartRequired {
		// Setup runs only from a new binary after a completed update.
		job.PostSetup = nil
		s.endLaunchCheck(&job)
	}
	if err != nil {
		job.State = "failed"
		job.Error = err.Error()
	} else if job.RestartRequired {
		job.State = "restarting"
		job.Phase = "restarting"
	} else {
		job.State = "succeeded"
		job.Phase = "complete"
	}
	saveErr := s.saveJob(job)
	if saveErr != nil {
		job.State = "failed"
		job.Error = "could not persist update result: " + saveErr.Error()
		_ = s.saveJob(job)
	}
	s.active = job.State == "restarting" && saveErr == nil && s.hooks.Restart != nil
	s.mu.Unlock()
	if s.hooks.Finished != nil {
		s.hooks.Finished(job)
	}
	if err == nil && saveErr == nil && job.RestartRequired && s.hooks.Restart != nil {
		// The result is durable before shutting down the serving process.
		if err := s.hooks.Restart(); err != nil {
			s.mu.Lock()
			if s.hooks.Supervised && s.status.Job != nil && s.status.Job.State != "restarting" {
				s.mu.Unlock()
				return
			}
			job.State = "failed"
			s.active = false
			job.Error = "binaries updated but daemon restart failed: " + err.Error()
			s.endLaunchCheck(&job)
			_ = s.saveJob(job)
			s.mu.Unlock()
		}
	}
}

type backupEntry struct {
	Binary Binary `json:"binary"`
	// Target is the file the update wrote; empty means Binary.Path.
	Target string `json:"target,omitempty"`
	// Created marks a file the update created (an install next to a
	// package-manager-owned binary); rollback removes it.
	Created    bool   `json:"created,omitempty"`
	Backup     string `json:"backup"`
	BeforeHash string `json:"before_hash"`
	AfterHash  string `json:"after_hash"`
	Mode       uint32 `json:"mode"`
}
type backupManifest struct {
	ID      string        `json:"id"`
	Version string        `json:"version"`
	Entries []backupEntry `json:"entries"`
}

func fileHash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func copyExclusive(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	syncErr := out.Sync()
	closeErr := out.Close()
	return errors.Join(copyErr, syncErr, closeErr)
}
func replaceFile(src, dst string, mode os.FileMode) error {
	info, err := os.Lstat(dst)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("update target is no longer a regular file")
	}
	temp, err := os.CreateTemp(filepath.Dir(dst), ".tclaude-update-")
	if err != nil {
		return err
	}
	name := temp.Name()
	_ = temp.Close()
	_ = os.Remove(name)
	defer func() { _ = os.Remove(name) }()
	if err := copyExclusive(src, name, mode); err != nil {
		return err
	}
	if err := os.Rename(name, dst); err != nil {
		return err
	}
	return syncDir(filepath.Dir(dst))
}
func (s *Service) undo(entries []backupEntry, cause error) error {
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		var err error
		if e.Created {
			err = os.Remove(e.target())
		} else {
			err = replaceFile(e.Backup, e.target(), os.FileMode(e.Mode))
		}
		if err != nil {
			cause = errors.Join(cause, fmt.Errorf("automatic rollback failed: %w", err))
		}
	}
	return cause
}
func (s *Service) verifyRestart(job Job) error {
	var manifest backupManifest
	if err := readJSON(filepath.Join(s.dir, "backup.json"), &manifest); err != nil {
		// A package-manager update keeps no tclaude backup; it was verified
		// before the restart.
		if errors.Is(err, os.ErrNotExist) && job.Action == "apply" && !s.hooks.Supervised && job.Method != "" {
			return nil
		}
		return err
	}
	if s.hooks.Supervised && job.Action == "apply" && manifest.ID != job.ID {
		return fmt.Errorf("backup does not match pending update")
	}
	if !s.hooks.Supervised && job.Action == "apply" && job.Method != "" && manifest.ID != job.ID {
		return nil // this update replaced nothing tclaude backed up
	}
	for _, e := range manifest.Entries {
		expected := e.AfterHash
		if job.Action == "rollback" {
			expected = e.BeforeHash
		}
		hash, err := fileHash(e.target())
		if job.Action == "rollback" && e.Created {
			if !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("installed binaries do not match the completed rollback")
			}
			continue
		}
		if err != nil || hash != expected {
			return fmt.Errorf("installed binaries do not match the completed update")
		}
	}
	return nil
}
func (e backupEntry) target() string {
	if e.Target != "" {
		return e.Target
	}
	return e.Binary.Path
}
func (s *Service) rollback(job *Job, authorize func() bool) error {
	if !authorize() {
		return fmt.Errorf("update authority revoked")
	}
	var manifest backupManifest
	if err := readJSON(filepath.Join(s.dir, "backup.json"), &manifest); err != nil {
		return fmt.Errorf("no readable rollback backup: %w", err)
	}
	dir, err := os.MkdirTemp(s.dir, "rollback-")
	if err != nil {
		return err
	}
	keepUndo := false
	defer func() {
		if !keepUndo {
			_ = os.RemoveAll(dir)
		}
	}()
	undo := []backupEntry{}
	replaced, created := []backupEntry{}, []backupEntry{}
	for _, e := range manifest.Entries {
		if e.Created {
			created = append(created, e)
		} else {
			replaced = append(replaced, e)
		}
	}
	if len(manifest.Entries) == 0 {
		return fmt.Errorf("the last update kept no tclaude backup; nothing to roll back")
	}
	for _, e := range created {
		hash, err := fileHash(e.target())
		if err != nil && !errors.Is(err, os.ErrNotExist) || err == nil && hash != e.AfterHash {
			return fmt.Errorf("binary changed outside updater; refusing rollback")
		}
	}
	for i, e := range replaced {
		hash, err := fileHash(e.Backup)
		if err != nil || hash != e.BeforeHash {
			return fmt.Errorf("rollback backup verification failed")
		}
		hash, err = fileHash(e.target())
		if err != nil || (hash != e.AfterHash && hash != e.BeforeHash) {
			return fmt.Errorf("binary changed outside updater; refusing rollback")
		}
		current := filepath.Join(dir, fmt.Sprint(i))
		if err := copyExclusive(e.target(), current, 0600); err != nil {
			return err
		}
		copied, err := fileHash(current)
		if err != nil || copied != hash {
			return fmt.Errorf("binary changed during rollback preparation")
		}
		binary := e.Binary
		binary.Version = job.CurrentVersion
		undo = append(undo, backupEntry{Binary: binary, Target: e.Target, Backup: current, BeforeHash: hash, AfterHash: e.BeforeHash, Mode: e.Mode})
	}
	if s.hooks.Supervised {
		if err := syncDir(dir); err != nil {
			return err
		}
		if err := writeJSONFile(filepath.Join(s.dir, "rollback-undo.json"), backupManifest{ID: job.ID, Version: job.CurrentVersion, Entries: undo}); err != nil {
			return err
		}
		keepUndo = true
	}
	if err := s.phase(job, "restoring_backup"); err != nil {
		return err
	}
	for i, e := range replaced {
		hash, err := fileHash(e.target())
		if err != nil || hash != undo[i].BeforeHash {
			return s.undo(undo[:i], fmt.Errorf("binary changed before rollback"))
		}
		if !authorize() {
			return s.undo(undo[:i], fmt.Errorf("update authority revoked"))
		}
		if err := replaceFile(e.Backup, e.target(), os.FileMode(e.Mode)); err != nil {
			return s.undo(undo[:i+1], err)
		}
	}
	// Files the update created are removed last: nothing after them can fail
	// and need undoing. The daemon then restarts from the original binary.
	for _, e := range created {
		if err := os.Remove(e.target()); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("could not remove %s installed by the update: %w", e.target(), err)
		}
		job.Relocations = append(job.Relocations, Relocation{Name: e.Binary.Name, From: e.target(), To: e.Binary.Path})
	}
	job.RestartRequired = true
	return nil
}
func readJSON(path string, out any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if len(raw) > 1<<20 {
		return fmt.Errorf("update metadata exceeds limit")
	}
	return json.Unmarshal(raw, out)
}
func writeJSONFile(path string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".update-json-")
	if err != nil {
		return err
	}
	name := temp.Name()
	defer func() { _ = os.Remove(name) }()
	if err := temp.Chmod(0600); err != nil {
		_ = temp.Close()
		return err
	}
	_, writeErr := temp.Write(raw)
	syncErr := temp.Sync()
	closeErr := temp.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}
func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return dir.Sync()
}

func (s *Service) RestartFailed(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status.Job != nil {
		job := *s.status.Job
		job.State = "failed"
		job.Error = "daemon restart failed: " + err.Error()
		s.endLaunchCheck(&job)
		_ = s.saveJob(job)
	}
}

// RefreshMetadata is a bounded background read, never a binary replacement.
func (s *Service) RefreshMetadata(ctx context.Context) error {
	s.mu.Lock()
	skip := s.active || s.status.CheckedAt != nil && time.Since(*s.status.CheckedAt) < time.Hour
	s.mu.Unlock()
	if skip {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	r, err := s.release(ctx, "")
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	s.status.LatestVersion = r.Tag
	s.status.CheckedAt = &now
	s.updateAvailable()
	return writeJSONFile(filepath.Join(s.dir, "status.json"), s.status)
}
