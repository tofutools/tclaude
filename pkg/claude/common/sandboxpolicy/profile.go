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
	MaxProfileNameBytes    = 200
	MaxPathBytes           = 4096
	MaxEnvironmentName     = 128
	MaxEnvironmentValue    = 16 * 1024
	MaxEnvironmentCount    = 128
	MaxEnvironmentBytes    = 64 * 1024
	MaxAgentDirectoryCount = 128
	MaxIncludeCount        = 32
	// MaxIncludeDepth bounds the longest include-EDGE chain reachable from a
	// profile (a profile with no includes has depth 0). Registry write paths
	// and launch-time flattening enforce the same unit and bound, so a policy
	// that persists is always resolvable.
	MaxIncludeDepth = 16
)

type Access string

const (
	AccessRead  Access = "read"
	AccessWrite Access = "write"
	AccessDeny  Access = "deny"
)

type FilesystemGrant struct {
	Path   string `json:"path"`
	Access Access `json:"access"`
}

type EnvironmentEntry struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// NetworkAccess is the operator-authored network posture. The empty value
// inherits the harness's existing behavior. Internet and None both request
// an enforced IP-network posture; the Codex adapter uses its native network
// sandbox switch while retaining the agentd control socket.
type NetworkAccess string

const (
	NetworkAccessInherit  NetworkAccess = ""
	NetworkAccessInternet NetworkAccess = "internet"
	NetworkAccessNone     NetworkAccess = "none"
)

// Profile is the harness-neutral, operator-authored capability bundle. It is
// NetworkAccess is optional so existing profiles keep their harness's current
// network behavior. Harness launch posture belongs to spawn profiles instead.
//
// Includes composes other profiles by name, recursively: included profiles
// apply first in listed order, then the profile's own entries override any
// same-path or same-name values they supplied. Unlike Filesystem and
// Environment, Includes keeps its authored order because that order carries
// the override semantics. Flatten expands Includes; Resolve refuses profiles
// that still carry them.
type Profile struct {
	Name             string             `json:"name"`
	Filesystem       []FilesystemGrant  `json:"filesystem,omitempty"`
	Environment      []EnvironmentEntry `json:"environment,omitempty"`
	AgentDirectories []string           `json:"agent_directories,omitempty"`
	NetworkAccess    NetworkAccess      `json:"network_access,omitempty"`
	Includes         []string           `json:"includes,omitempty"`
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

var reservedProfileNames = map[string]struct{}{
	"export": {},
	"import": {},
}

// Normalize validates a profile and returns a canonical copy. It never mutates
// the caller's slices. Filesystem paths are fully symlink-resolved existing
// directories, duplicate paths fold with deny dominating write dominating
// read, and output is
// sorted for deterministic persistence and export. Environment duplicates
// with the same value fold; conflicting values fail rather than depending on
// input order.
func Normalize(in Profile) (Profile, error) {
	profile, _, err := normalize(in, false)
	return profile, err
}

// NormalizeForPersistence validates profile data without requiring every
// filesystem path to exist yet. Missing paths are retained in canonical
// lexical form and returned separately so authoring/import surfaces can warn
// the operator. Existing paths receive the same symlink, directory and
// protected-root checks as Normalize. Resolution and snapshot revalidation use
// this variant so a missing rule can survive resolution and become active on a
// later launch after the directory exists and is revalidated.
func NormalizeForPersistence(in Profile) (Profile, []string, error) {
	return normalize(in, true)
}

// NormalizeForImport is kept as the portable-transfer spelling for callers
// that want to emphasize that boundary.
func NormalizeForImport(in Profile) (Profile, []string, error) {
	return NormalizeForPersistence(in)
}

func normalize(in Profile, allowMissing bool) (Profile, []string, error) {
	name, err := normalizeName(in.Name)
	if err != nil {
		return Profile{}, nil, err
	}
	filesystem, missing, err := normalizeFilesystem(in.Filesystem, allowMissing)
	if err != nil {
		return Profile{}, nil, err
	}
	environment, err := normalizeEnvironment(in.Environment)
	if err != nil {
		return Profile{}, nil, err
	}
	agentDirectories, err := normalizeAgentDirectories(in.AgentDirectories, environment)
	if err != nil {
		return Profile{}, nil, err
	}
	includes, err := normalizeIncludes(name, in.Includes)
	if err != nil {
		return Profile{}, nil, err
	}
	networkAccess, err := NormalizeNetworkAccess(in.NetworkAccess)
	if err != nil {
		return Profile{}, nil, err
	}
	return Profile{Name: name, Filesystem: filesystem, Environment: environment, AgentDirectories: agentDirectories, NetworkAccess: networkAccess, Includes: includes}, missing, nil
}

// NormalizeNetworkAccess validates one network posture without requiring a
// complete profile. Harness adapters use it at their final rendering seam.
func NormalizeNetworkAccess(in NetworkAccess) (NetworkAccess, error) {
	switch in {
	case NetworkAccessInherit, NetworkAccessInternet, NetworkAccessNone:
		return in, nil
	default:
		return "", fmt.Errorf("network_access %q is invalid (want internet, none, or omitted to inherit)", in)
	}
}

// normalizeIncludes validates include references syntactically. Whether each
// referenced profile exists (and whether the whole graph stays acyclic) is a
// registry-level invariant checked where the registry is available: at store
// time and again by Flatten at resolution time. Order is preserved verbatim
// because later includes override earlier ones.
func normalizeIncludes(profileName string, in []string) ([]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	if len(in) > MaxIncludeCount {
		return nil, fmt.Errorf("includes has too many entries (maximum %d)", MaxIncludeCount)
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for i, include := range in {
		name, err := normalizeName(include)
		if err != nil {
			return nil, fmt.Errorf("includes[%d]: %w", i, err)
		}
		if name == profileName {
			return nil, fmt.Errorf("includes[%d]: sandbox profile %q must not include itself", i, name)
		}
		if seen[name] {
			return nil, fmt.Errorf("includes[%d]: sandbox profile %q is included more than once", i, name)
		}
		seen[name] = true
		out = append(out, name)
	}
	return out, nil
}

func normalizeAgentDirectories(in []string, environment []EnvironmentEntry) ([]string, error) {
	if len(in) > MaxAgentDirectoryCount {
		return nil, fmt.Errorf("agent_directories has too many entries (maximum %d)", MaxAgentDirectoryCount)
	}
	literal := make(map[string]struct{}, len(environment))
	for _, entry := range environment {
		literal[entry.Name] = struct{}{}
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for i, name := range in {
		if len(name) > MaxEnvironmentName || !environmentNameRE.MatchString(name) {
			return nil, fmt.Errorf("agent_directories[%d] %q is invalid (want an ASCII environment-variable name up to %d bytes)", i, name, MaxEnvironmentName)
		}
		if isReservedEnvironmentName(name) {
			return nil, fmt.Errorf("agent_directories[%d] environment variable %q is reserved", i, name)
		}
		if _, conflict := literal[name]; conflict {
			return nil, fmt.Errorf("agent_directories[%d] environment variable %q also has a literal environment value", i, name)
		}
		if _, duplicate := seen[name]; duplicate {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	sort.Strings(out)
	if len(environment)+len(out) > MaxEnvironmentCount {
		return nil, fmt.Errorf("environment and agent_directories have too many entries combined (maximum %d)", MaxEnvironmentCount)
	}
	return out, nil
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
	if _, reserved := reservedProfileNames[strings.ToLower(name)]; reserved {
		return "", fmt.Errorf("sandbox profile name %q is reserved for profile transfer routes", name)
	}
	if !utf8.ValidString(name) || strings.ContainsFunc(name, isControl) {
		return "", fmt.Errorf("sandbox profile name must be valid UTF-8 without control characters")
	}
	return name, nil
}

func normalizeFilesystem(in []FilesystemGrant, allowMissing bool) ([]FilesystemGrant, []string, error) {
	protected, err := protectedPaths()
	if err != nil {
		return nil, nil, err
	}
	byPath := make(map[string]Access, len(in))
	missingPaths := map[string]bool{}
	for i, grant := range in {
		if grant.Access != AccessRead && grant.Access != AccessWrite && grant.Access != AccessDeny {
			return nil, nil, fmt.Errorf("filesystem[%d].access %q is invalid (want read, write, or deny)", i, grant.Access)
		}
		path, missing, err := canonicalDirectory(grant.Path, allowMissing)
		if err != nil {
			return nil, nil, fmt.Errorf("filesystem[%d].path: %w", i, err)
		}
		if grant.Access != AccessDeny {
			for _, denied := range protected {
				if pathsIntersect(path, denied) {
					return nil, nil, fmt.Errorf("filesystem[%d].path %q intersects protected directory %q", i, path, denied)
				}
			}
		}
		if missing {
			missingPaths[path] = true
		}
		if previous, exists := byPath[path]; !exists || accessRank(grant.Access) > accessRank(previous) {
			byPath[path] = grant.Access
		}
	}
	out := make([]FilesystemGrant, 0, len(byPath))
	for path, access := range byPath {
		out = append(out, FilesystemGrant{Path: path, Access: access})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	missing := make([]string, 0, len(missingPaths))
	for path := range missingPaths {
		missing = append(missing, path)
	}
	sort.Strings(missing)
	return out, missing, nil
}

func accessRank(access Access) int {
	switch access {
	case AccessDeny:
		return 2
	case AccessWrite:
		return 1
	default:
		return 0
	}
}

func canonicalDirectory(path string, allowMissing bool) (string, bool, error) {
	if path == "" {
		return "", false, fmt.Errorf("path is required")
	}
	if len(path) > MaxPathBytes {
		return "", false, fmt.Errorf("path is too long (maximum %d bytes)", MaxPathBytes)
	}
	if !utf8.ValidString(path) || strings.ContainsFunc(path, isControl) {
		return "", false, fmt.Errorf("path must be valid UTF-8 without control characters")
	}
	// A leading "~" or "~/" is a convenience alias for the daemon's own home
	// directory (the box these grants apply to). Only the bare-user form is
	// supported — "~otheruser/..." keeps its literal "~" and falls through to
	// the not-absolute error below, rather than guessing another account's home.
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", false, fmt.Errorf("expand %q: resolve home directory: %w", path, err)
		}
		if path == "~" {
			path = home
		} else {
			path = filepath.Join(home, path[len("~/"):])
		}
	}
	if !filepath.IsAbs(path) {
		return "", false, fmt.Errorf("path %q is not absolute", path)
	}
	clean := filepath.Clean(path)
	resolved, err := filepath.EvalSymlinks(clean)
	if err != nil {
		if allowMissing && os.IsNotExist(err) {
			resolved, err = canonicalMissingDirectory(clean)
			if err == nil {
				return resolved, true, nil
			}
		}
		return "", false, fmt.Errorf("resolve symlinks for %q: %w", path, err)
	}
	resolved = filepath.Clean(resolved)
	info, err := os.Stat(resolved)
	if err != nil {
		return "", false, fmt.Errorf("stat %q: %w", resolved, err)
	}
	if !info.IsDir() {
		return "", false, fmt.Errorf("path %q is not a directory", resolved)
	}
	return resolved, false, nil
}

// canonicalMissingDirectory resolves the longest existing ancestor so an
// imported missing path cannot disguise an existing symlink into a protected
// tree. The unresolved suffix is then appended lexically.
func canonicalMissingDirectory(path string) (string, error) {
	ancestor := path
	suffix := []string{}
	for {
		_, err := os.Lstat(ancestor)
		if err == nil {
			resolved, err := filepath.EvalSymlinks(ancestor)
			if err != nil {
				return "", err
			}
			info, err := os.Stat(resolved)
			if err != nil {
				return "", err
			}
			if !info.IsDir() {
				return "", fmt.Errorf("existing ancestor %q is not a directory", ancestor)
			}
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return filepath.Clean(resolved), nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return "", err
		}
		suffix = append(suffix, filepath.Base(ancestor))
		ancestor = parent
	}
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
		} else if os.IsNotExist(err) {
			if resolved, missingErr := canonicalMissingDirectory(path); missingErr == nil {
				path = resolved
			}
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
