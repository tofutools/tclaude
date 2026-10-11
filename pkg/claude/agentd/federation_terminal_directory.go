package agentd

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"

	"golang.org/x/sys/unix"
)

const terminalDirectoryMaxEntries = 100

// Enumerate only one directory level on the attachment's pinned root. Child
// metadata is read relative to the directory descriptor, never through a path
// that a concurrent rename or symlink could redirect.
func terminalDirectoryListing(root *os.File, rootPath, path string) (*os.File, error) {
	dir, err := openTerminalProjectPath(root, rootPath, path, true)
	if err != nil {
		return nil, err
	}
	defer func() { _ = dir.Close() }()
	names, err := dir.Readdirnames(terminalDirectoryMaxEntries + 1)
	if err != nil && err != io.EOF {
		return nil, err
	}
	truncated := len(names) > terminalDirectoryMaxEntries
	if truncated {
		names = names[:terminalDirectoryMaxEntries]
	}
	sort.Strings(names)
	type entry struct {
		Path string `json:"path"`
		Kind string `json:"kind"`
		Size int64  `json:"size"`
	}
	entries := []entry{}
	for _, name := range names {
		child := filepath.Join(path, name)
		if _, err := terminalFileRelative(rootPath, child); err != nil {
			continue
		}
		var stat unix.Stat_t
		if err := unix.Fstatat(int(dir.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			continue
		}
		kind := "file"
		switch stat.Mode & unix.S_IFMT {
		case unix.S_IFDIR:
			kind = "directory"
		case unix.S_IFREG:
			if stat.Size > terminalFileMaxBytes {
				continue
			}
		default:
			continue
		}
		entries = append(entries, entry{Path: child, Kind: kind, Size: stat.Size})
	}
	raw, err := json.Marshal(map[string]any{"entries": entries, "truncated": truncated})
	if err != nil {
		return nil, err
	}
	f, err := os.CreateTemp("", "tclaude-terminal-directory-")
	if err != nil {
		return nil, err
	}
	_ = os.Remove(f.Name())
	if _, err = f.Write(raw); err == nil {
		_, err = f.Seek(0, io.SeekStart)
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}
