package host

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
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
	Order   string
	Payload []byte
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

func (s *ObservationSpool) Directory() string { return s.directory }

func (s *ObservationSpool) Drain() ([]ObservationSpoolEvent, error) {
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
		if removeErr := os.Remove(path); removeErr != nil {
			return nil, fmt.Errorf("consume observation event: %w", removeErr)
		}
		result = append(result, ObservationSpoolEvent{Order: name, Payload: value})
	}
	return result, nil
}

func (s *ObservationSpool) Remove() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	relative, err := filepath.Rel(s.root, s.directory)
	if err != nil || strings.Contains(relative, string(filepath.Separator)) || !strings.HasPrefix(relative, "observation-") {
		return fmt.Errorf("observation spool is outside private storage")
	}
	if err := os.RemoveAll(s.directory); err != nil {
		return fmt.Errorf("remove observation spool: %w", err)
	}
	return nil
}
