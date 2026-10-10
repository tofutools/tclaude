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
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/common/buildversion"
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
	stageGo      func(context.Context, string, string, string) (string, error)
	inspect      func(context.Context, string) (buildversion.Info, error)
}

func New(dir string, binaries []Binary, hooks Hooks) (*Service, error) {
	if err := os.MkdirAll(filepath.Join(dir, "jobs"), 0700); err != nil {
		return nil, err
	}
	s := &Service{dir: dir, binaries: binaries, hooks: hooks, release: loadRelease, stageRelease: stageRelease, stageGo: stageGoInstall, inspect: inspectStaged}
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
		}
		if err := s.saveJob(job); err != nil {
			return nil, err
		}
	}
	s.updateAvailable()
	return s, nil
}
func (s *Service) updateAvailable() {
	if semver.IsValid(s.status.CurrentVersion) && semver.IsValid(s.status.LatestVersion) {
		v := semver.Compare(s.status.LatestVersion, s.status.CurrentVersion) > 0
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
	if s.hooks.Started != nil {
		if err := s.hooks.Started(job); err != nil {
			return Job{}, err
		}
	}
	if err := s.saveJob(job); err != nil {
		return Job{}, err
	}
	s.active = true
	go s.run(req, job, authorize)
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
func (s *Service) run(req Request, job Job, authorize func() bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	var err error
	if req.Action == "rollback" {
		err = s.rollback(&job, authorize)
	} else {
		err = s.phase(&job, "checking_release")
		var release Release
		if err == nil {
			release, err = s.release(ctx, req.Version)
		}
		if err == nil {
			job.Version = release.Tag
			if semver.IsValid(job.CurrentVersion) {
				available := semver.Compare(release.Tag, job.CurrentVersion) > 0
				job.UpdateAvailable = &available
			}
			s.mu.Lock()
			now := time.Now().UTC()
			if req.Version == "" {
				s.status.LatestVersion = release.Tag
				s.status.CheckedAt = &now
			}
			s.updateAvailable()
			err = s.saveJob(job)
			s.mu.Unlock()
		}
		if err == nil && req.Action == "apply" && s.hooks.NoDowngrade && semver.Compare(release.Tag, job.CurrentVersion) < 0 {
			err = fmt.Errorf("downgrades are refused; use rollback to restore the journal backup")
		}
		if err == nil && req.Action == "apply" {
			err = s.apply(ctx, release, &job, authorize, req.Version)
		}
	}
	s.mu.Lock()
	now := time.Now().UTC()
	job.FinishedAt = &now
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
			_ = s.saveJob(job)
			s.mu.Unlock()
		}
	}
}

type backupEntry struct {
	Binary     Binary `json:"binary"`
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
func (s *Service) apply(ctx context.Context, release Release, job *Job, authorize func() bool, pin string) error {
	current := len(s.binaries) > 0
	for _, b := range s.binaries {
		current = current && b.Version == release.Tag
	}
	if current {
		return nil
	}
	if err := s.phase(job, "staging"); err != nil {
		return err
	}
	dir, err := os.MkdirTemp(s.dir, "stage-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	staged := map[string]string{}
	for _, b := range s.binaries {
		if _, ok := staged[b.Name]; ok {
			continue
		}
		var path string
		if b.Method == "release" || s.hooks.ReleaseOnly {
			path, err = s.stageRelease(ctx, release, b.Name, dir)
		} else {
			version := release.Tag
			if b.Method == "source" && pin == "" {
				version = "latest"
			}
			path, err = s.stageGo(ctx, b.Name, version, dir)
		}
		if err != nil {
			return err
		}
		info, err := s.inspect(ctx, path)
		if err != nil {
			return err
		}
		if info.Version != release.Tag {
			return fmt.Errorf("staged %s version does not match %s", b.Name, release.Tag)
		}
		if info.ProtocolVersion != s.status.ProtocolVersion {
			return fmt.Errorf("target federation protocol %d differs from current %d; linked peers may be incompatible", info.ProtocolVersion, s.status.ProtocolVersion)
		}
		staged[b.Name] = path
	}
	if !authorize() {
		return fmt.Errorf("update authority was revoked before replacement")
	}
	if err := s.phase(job, "backing_up"); err != nil {
		return err
	}
	backupDir := filepath.Join(s.dir, "backup-"+job.ID)
	if err := os.Mkdir(backupDir, 0700); err != nil {
		return err
	}
	manifest := backupManifest{ID: job.ID, Version: release.Tag, Entries: []backupEntry{}}
	for i, b := range s.binaries {
		info, err := os.Lstat(b.Path)
		if err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("update target is not a regular file")
		}
		hash, err := fileHash(b.Path)
		if err != nil {
			return err
		}
		after, err := fileHash(staged[b.Name])
		if err != nil {
			return err
		}
		backup := filepath.Join(backupDir, fmt.Sprint(i))
		if err := copyExclusive(b.Path, backup, 0600); err != nil {
			return fmt.Errorf("backup failed: %w", err)
		}
		copied, err := fileHash(backup)
		if err != nil || copied != hash {
			return fmt.Errorf("update target changed during backup")
		}
		manifest.Entries = append(manifest.Entries, backupEntry{Binary: b, Backup: backup, BeforeHash: hash, AfterHash: after, Mode: uint32(info.Mode().Perm())})
	}
	if err := syncDir(backupDir); err != nil {
		return fmt.Errorf("could not persist backup directory: %w", err)
	}
	if err := writeJSONFile(filepath.Join(s.dir, "backup.json"), manifest); err != nil {
		return err
	}
	s.mu.Lock()
	s.status.RollbackAvailable = true
	s.mu.Unlock()
	if err := s.phase(job, "replacing"); err != nil {
		return err
	}
	replaced := []backupEntry{}
	for _, e := range manifest.Entries {
		hash, err := fileHash(e.Binary.Path)
		if err != nil || hash != e.BeforeHash {
			return s.undo(replaced, fmt.Errorf("update target changed before replacement"))
		}
		if !authorize() {
			return s.undo(replaced, fmt.Errorf("update authority revoked"))
		}
		if err := replaceFile(staged[e.Binary.Name], e.Binary.Path, os.FileMode(e.Mode)); err != nil {
			return s.undo(append(replaced, e), err)
		}
		replaced = append(replaced, e)
	}
	job.RestartRequired = true
	return nil
}
func (s *Service) undo(entries []backupEntry, cause error) error {
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if err := replaceFile(e.Backup, e.Binary.Path, os.FileMode(e.Mode)); err != nil {
			cause = errors.Join(cause, fmt.Errorf("automatic rollback failed: %w", err))
		}
	}
	return cause
}
func (s *Service) verifyRestart(job Job) error {
	var manifest backupManifest
	if err := readJSON(filepath.Join(s.dir, "backup.json"), &manifest); err != nil {
		return err
	}
	if s.hooks.Supervised && job.Action == "apply" && manifest.ID != job.ID {
		return fmt.Errorf("backup does not match pending update")
	}
	for _, e := range manifest.Entries {
		expected := e.AfterHash
		if job.Action == "rollback" {
			expected = e.BeforeHash
		}
		hash, err := fileHash(e.Binary.Path)
		if err != nil || hash != expected {
			return fmt.Errorf("installed binaries do not match the completed update")
		}
	}
	return nil
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
	for i, e := range manifest.Entries {
		hash, err := fileHash(e.Backup)
		if err != nil || hash != e.BeforeHash {
			return fmt.Errorf("rollback backup verification failed")
		}
		hash, err = fileHash(e.Binary.Path)
		if err != nil || (hash != e.AfterHash && hash != e.BeforeHash) {
			return fmt.Errorf("binary changed outside updater; refusing rollback")
		}
		current := filepath.Join(dir, fmt.Sprint(i))
		if err := copyExclusive(e.Binary.Path, current, 0600); err != nil {
			return err
		}
		copied, err := fileHash(current)
		if err != nil || copied != hash {
			return fmt.Errorf("binary changed during rollback preparation")
		}
		binary := e.Binary
		binary.Version = job.CurrentVersion
		undo = append(undo, backupEntry{Binary: binary, Backup: current, BeforeHash: hash, AfterHash: e.BeforeHash, Mode: e.Mode})
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
	for i, e := range manifest.Entries {
		hash, err := fileHash(e.Binary.Path)
		if err != nil || hash != undo[i].BeforeHash {
			return s.undo(undo[:i], fmt.Errorf("binary changed before rollback"))
		}
		if !authorize() {
			return s.undo(undo[:i], fmt.Errorf("update authority revoked"))
		}
		if err := replaceFile(e.Backup, e.Binary.Path, os.FileMode(e.Mode)); err != nil {
			return s.undo(undo[:i+1], err)
		}
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
