package selfupdate

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Pending returns the durable supervisor journal without changing it. The
// guardian calls this before starting a candidate following its own restart.
func (s *Service) Pending() *Job { return s.Status().Job }

// HealthDeadline is durable before the serving child is replaced.
func (s *Service) HealthDeadline(deadline time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status.Job == nil {
		return fmt.Errorf("missing supervised update job")
	}
	j := *s.status.Job
	j.State = "restarting"
	j.Phase = "health_check"
	j.Deadline = &deadline
	return s.saveJob(j)
}

// FinishSupervised is called only by the host guardian after a live health
// check. No serving worker may infer health merely from a binary hash.
func (s *Service) FinishSupervised(version string, rolledBack bool, cause error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status.Job == nil {
		return fmt.Errorf("missing supervised update job")
	}
	j := *s.status.Job
	now := time.Now().UTC()
	j.FinishedAt = &now
	j.Deadline = nil
	j.State = "completed"
	j.Phase = "complete"
	if rolledBack {
		j.State = "rolled_back"
		j.Phase = "rolled_back"
		j.RolledBack = true
	}
	if cause != nil {
		j.Error = cause.Error()
		if !rolledBack {
			j.State = "failed"
			j.Phase = "failed"
		}
	}
	if cause == nil || rolledBack {
		s.status.CurrentVersion = version
		for i := range s.binaries {
			s.binaries[i].Version = version
		}
		s.status.Binaries = append([]Binary{}, s.binaries...)
		s.updateAvailable()
	}
	if err := s.saveJob(j); err != nil {
		return err
	}
	s.active = false
	return nil
}

// RestoreSupervised verifies both the installed image and backup before
// restoring it. It intentionally bypasses remote authority: restoring safety
// after replacement must remain possible even if the actor has been revoked.
func (s *Service) RestoreSupervised() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status.Job == nil {
		return "", fmt.Errorf("missing supervised job")
	}
	name := "backup.json"
	if s.status.Job.Action == "rollback" {
		name = "rollback-undo.json"
	}
	var manifest backupManifest
	if err := readJSON(filepath.Join(s.dir, name), &manifest); err != nil {
		return "", err
	}
	if manifest.ID != s.status.Job.ID {
		return "", fmt.Errorf("backup does not belong to pending update")
	}
	if len(manifest.Entries) != len(s.binaries) {
		return "", fmt.Errorf("backup does not match supervised binaries")
	}
	for i, e := range manifest.Entries {
		if e.Binary.Path != s.binaries[i].Path || e.Binary.Name != s.binaries[i].Name {
			return "", fmt.Errorf("backup target does not match supervised binary")
		}
		before, err := fileHash(e.Backup)
		if err != nil || before != e.BeforeHash {
			return "", fmt.Errorf("rollback backup checksum mismatch")
		}
		installed, err := fileHash(e.Binary.Path)
		if err != nil || installed != e.AfterHash && installed != e.BeforeHash {
			return "", fmt.Errorf("installed binary changed outside updater")
		}
	}
	if s.status.Job != nil {
		j := *s.status.Job
		j.State = "restarting"
		j.Phase = "rolling_back"
		if err := s.saveJob(j); err != nil {
			return "", err
		}
	}
	for _, e := range manifest.Entries {
		if err := replaceFile(e.Backup, e.Binary.Path, os.FileMode(e.Mode)); err != nil {
			return "", err
		}
	}
	return manifest.Entries[0].Binary.Version, nil
}

func (s *Service) VerifySupervised() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status.Job == nil {
		return fmt.Errorf("missing supervised update job")
	}
	return s.verifyRestart(*s.status.Job)
}
