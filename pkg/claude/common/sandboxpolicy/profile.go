package sandboxpolicy

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	tclcommon "github.com/tofutools/tclaude/pkg/common"
)

const (
	MaxProfileNameBytes = 200
	MaxPathBytes        = 4096
	MaxEnvironmentName  = 128
	MaxEnvironmentValue = 16 * 1024
	MaxEnvironmentCount = 128
	MaxEnvironmentBytes = 64 * 1024
)

type Access string

const (
	AccessRead  Access = "read"
	AccessWrite Access = "write"
)

type FilesystemGrant struct {
	Path   string `json:"path"`
	Access Access `json:"access"`
}

type EnvironmentEntry struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Profile is the harness-neutral, operator-authored capability bundle. It is
// deliberately limited to additive filesystem and environment configuration;
// harness launch posture belongs to spawn profiles instead.
type Profile struct {
	Name        string             `json:"name"`
	Filesystem  []FilesystemGrant  `json:"filesystem,omitempty"`
	Environment []EnvironmentEntry `json:"environment,omitempty"`
}

var environmentNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

var reservedEnvironmentNames = map[string]struct{}{
	"HOME": {}, "PATH": {}, "SHELL": {}, "TMPDIR": {}, "TMP": {}, "TEMP": {},
	"CLAUDE_CONFIG_DIR": {}, "XDG_CONFIG_HOME": {}, "TMUX": {}, "TMUX_PANE": {},
	"NODE_OPTIONS": {}, "BASH_ENV": {}, "ENV": {},
}

var reservedEnvironmentPrefixes = []string{
	"TCLAUDE_", "CLAUDE_CODE_", "CODEX_", "LD_", "DYLD_",
}

// Normalize validates a profile and returns a canonical copy. It never mutates
// the caller's slices. Filesystem paths are fully symlink-resolved existing
// directories, duplicate paths fold with write dominating read, and output is
// sorted for deterministic persistence and export. Environment duplicates
// with the same value fold; conflicting values fail rather than depending on
// input order.
func Normalize(in Profile) (Profile, error) {
	name, err := normalizeName(in.Name)
	if err != nil {
		return Profile{}, err
	}
	filesystem, err := normalizeFilesystem(in.Filesystem)
	if err != nil {
		return Profile{}, err
	}
	environment, err := normalizeEnvironment(in.Environment)
	if err != nil {
		return Profile{}, err
	}
	return Profile{Name: name, Filesystem: filesystem, Environment: environment}, nil
}

func normalizeName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("sandbox profile name is required")
	}
	if len(name) > MaxProfileNameBytes {
		return "", fmt.Errorf("sandbox profile name is too long (maximum %d bytes)", MaxProfileNameBytes)
	}
	if strings.ContainsAny(name, `/\\`) {
		return "", fmt.Errorf("sandbox profile name must not contain slashes")
	}
	if !utf8.ValidString(name) || strings.ContainsFunc(name, isControl) {
		return "", fmt.Errorf("sandbox profile name must be valid UTF-8 without control characters")
	}
	return name, nil
}

func normalizeFilesystem(in []FilesystemGrant) ([]FilesystemGrant, error) {
	protected, err := protectedPaths()
	if err != nil {
		return nil, err
	}
	byPath := make(map[string]Access, len(in))
	for i, grant := range in {
		if grant.Access != AccessRead && grant.Access != AccessWrite {
			return nil, fmt.Errorf("filesystem[%d].access %q is invalid (want read or write)", i, grant.Access)
		}
		path, err := canonicalDirectory(grant.Path)
		if err != nil {
			return nil, fmt.Errorf("filesystem[%d].path: %w", i, err)
		}
		for _, denied := range protected {
			if pathsIntersect(path, denied) {
				return nil, fmt.Errorf("filesystem[%d].path %q intersects protected directory %q", i, path, denied)
			}
		}
		if previous, exists := byPath[path]; !exists || grant.Access == AccessWrite || previous != AccessWrite {
			byPath[path] = grant.Access
		}
	}
	out := make([]FilesystemGrant, 0, len(byPath))
	for path, access := range byPath {
		out = append(out, FilesystemGrant{Path: path, Access: access})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func canonicalDirectory(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("path is required")
	}
	if len(path) > MaxPathBytes {
		return "", fmt.Errorf("path is too long (maximum %d bytes)", MaxPathBytes)
	}
	if !utf8.ValidString(path) || strings.ContainsFunc(path, isControl) {
		return "", fmt.Errorf("path must be valid UTF-8 without control characters")
	}
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("path %q is not absolute", path)
	}
	resolved, err := filepath.EvalSymlinks(filepath.Clean(path))
	if err != nil {
		return "", fmt.Errorf("resolve symlinks for %q: %w", path, err)
	}
	resolved = filepath.Clean(resolved)
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("stat %q: %w", resolved, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("path %q is not a directory", resolved)
	}
	return resolved, nil
}

func protectedPaths() ([]string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve home directory for protected sandbox paths: %w", err)
	}
	paths := []string{
		tclcommon.TclaudeDataDir(),
		filepath.Join(home, ".claude", "sessions"),
		filepath.Join(home, ".codex"),
	}
	for i, path := range paths {
		path = filepath.Clean(path)
		if resolved, err := filepath.EvalSymlinks(path); err == nil {
			path = filepath.Clean(resolved)
		}
		paths[i] = path
	}
	return paths, nil
}

func pathsIntersect(a, b string) bool {
	return pathContainsOrEqual(a, b) || pathContainsOrEqual(b, a)
}

func pathContainsOrEqual(dir, target string) bool {
	rel, err := filepath.Rel(dir, target)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func normalizeEnvironment(in []EnvironmentEntry) ([]EnvironmentEntry, error) {
	if len(in) > MaxEnvironmentCount {
		return nil, fmt.Errorf("environment has too many entries (maximum %d)", MaxEnvironmentCount)
	}
	byName := make(map[string]string, len(in))
	total := 0
	for i, entry := range in {
		if len(entry.Name) > MaxEnvironmentName || !environmentNameRE.MatchString(entry.Name) {
			return nil, fmt.Errorf("environment[%d].name %q is invalid (want an ASCII identifier up to %d bytes)", i, entry.Name, MaxEnvironmentName)
		}
		if isReservedEnvironmentName(entry.Name) {
			return nil, fmt.Errorf("environment[%d].name %q is reserved for launch or sandbox control", i, entry.Name)
		}
		if len(entry.Value) > MaxEnvironmentValue {
			return nil, fmt.Errorf("environment[%d].value is too long (maximum %d bytes)", i, MaxEnvironmentValue)
		}
		if !utf8.ValidString(entry.Value) || strings.ContainsRune(entry.Value, '\x00') {
			return nil, fmt.Errorf("environment[%d].value must be valid UTF-8 without NUL bytes", i)
		}
		total += len(entry.Name) + len(entry.Value)
		if total > MaxEnvironmentBytes {
			return nil, fmt.Errorf("environment is too large (maximum %d bytes)", MaxEnvironmentBytes)
		}
		if previous, exists := byName[entry.Name]; exists && previous != entry.Value {
			return nil, fmt.Errorf("environment variable %q appears more than once with conflicting values", entry.Name)
		}
		byName[entry.Name] = entry.Value
	}
	out := make([]EnvironmentEntry, 0, len(byName))
	for name, value := range byName {
		out = append(out, EnvironmentEntry{Name: name, Value: value})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func isReservedEnvironmentName(name string) bool {
	if _, ok := reservedEnvironmentNames[name]; ok {
		return true
	}
	for _, prefix := range reservedEnvironmentPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

func isControl(r rune) bool { return r < 0x20 || r == 0x7f }
