package host

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const maxObservationEventSize = 1 << 20

// ObservationSpool is an attempt-private native hook ingress. Writers publish
// complete event files by same-directory rename; the provider drains only
// regular files from this exact directory. The path is an ordinary same-UID
// capability, not a claim of hostile-child isolation.
type ObservationSpool struct {
	root      string
	directory string
	mu        sync.Mutex
}

type ObservationSpoolEvent struct {
	RecordedAt time.Time
	Order      string
	Payload    []byte
}

func PrepareObservationSpool(privateRoot string) (*ObservationSpool, error) {
	root := filepath.Clean(privateRoot)
	if root == "." || !filepath.IsAbs(root) {
		return nil, fmt.Errorf("observation spool private root must be absolute")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create observation spool root: %w", err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return nil, fmt.Errorf("protect observation spool root: %w", err)
	}
	directory, err := os.MkdirTemp(root, "observation-")
	if err != nil {
		return nil, fmt.Errorf("prepare observation spool: %w", err)
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		_ = os.RemoveAll(directory)
		return nil, fmt.Errorf("protect observation spool: %w", err)
	}
	return &ObservationSpool{root: root, directory: directory}, nil
}

func RecoverObservationSpool(privateRoot, directory string) (*ObservationSpool, error) {
	root := filepath.Clean(privateRoot)
	directory = filepath.Clean(directory)
	relative, err := filepath.Rel(root, directory)
	if err != nil || strings.Contains(relative, string(filepath.Separator)) ||
		!strings.HasPrefix(relative, "observation-") || relative == "observation-" {
		return nil, fmt.Errorf("observation spool is outside private storage")
	}
	info, err := os.Lstat(directory)
	if err != nil {
		return nil, fmt.Errorf("inspect observation spool: %w", err)
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		return nil, fmt.Errorf("observation spool is not a protected directory")
	}
	return &ObservationSpool{root: root, directory: directory}, nil
}

// RemoveObservationSpool idempotently removes an exact attempt spool, including
// after the workload has exited and an earlier observation already cleaned it.
func RemoveObservationSpool(privateRoot, directory string) error {
	root := filepath.Clean(privateRoot)
	directory = filepath.Clean(directory)
	relative, err := filepath.Rel(root, directory)
	if err != nil || strings.Contains(relative, string(filepath.Separator)) ||
		!strings.HasPrefix(relative, "observation-") || relative == "observation-" {
		return fmt.Errorf("observation spool is outside private storage")
	}
	if err := os.RemoveAll(directory); err != nil {
		return fmt.Errorf("remove observation spool: %w", err)
	}
	return nil
}

func (s *ObservationSpool) Directory() string { return s.directory }

// ReadPending returns completed events without consuming them. Callers
// acknowledge each event only after durable admission so a transient sink
// failure can be retried.
func (s *ObservationSpool) ReadPending() ([]ObservationSpoolEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.directory)
	if err != nil {
		return nil, fmt.Errorf("list observation spool: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "event-") {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	result := make([]ObservationSpoolEvent, 0, len(names))
	for _, name := range names {
		path := filepath.Join(s.directory, name)
		info, statErr := os.Lstat(path)
		if statErr != nil {
			return nil, fmt.Errorf("inspect observation event: %w", statErr)
		}
		if !info.Mode().IsRegular() || info.Size() > maxObservationEventSize {
			return nil, fmt.Errorf("observation event %q is not a bounded regular file", name)
		}
		value, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil, fmt.Errorf("read observation event: %w", readErr)
		}
		result = append(result, ObservationSpoolEvent{Order: name, Payload: value, RecordedAt: info.ModTime().UTC()})
	}
	// Hook filenames are random; process completed native events chronologically.
	sort.SliceStable(result, func(i, j int) bool { return result[i].RecordedAt.Before(result[j].RecordedAt) })
	return result, nil
}

func (s *ObservationSpool) Acknowledge(order string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if filepath.Base(order) != order || !strings.HasPrefix(order, "event-") {
		return fmt.Errorf("observation event order is invalid")
	}
	if err := os.Remove(filepath.Join(s.directory, order)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("acknowledge observation event: %w", err)
	}
	return nil
}

func (s *ObservationSpool) Remove() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return RemoveObservationSpool(s.root, s.directory)
}
