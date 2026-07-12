package agentd

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	tclcommon "github.com/tofutools/tclaude/pkg/common"
)

var agentDirectoryLaunchKeyRE = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// materializeAgentDirectories turns profile declarations into literal frozen
// environment values and writable filesystem grants. The generated root is
// durable across resume; the returned cleanup removes it only if launch fails.
func materializeAgentDirectories(snapshot sandboxpolicy.Snapshot, launchKey string) (sandboxpolicy.Snapshot, func(), error) {
	if len(snapshot.Effective.AgentDirectories) == 0 {
		return snapshot, func() {}, nil
	}
	if !agentDirectoryLaunchKeyRE.MatchString(launchKey) {
		return sandboxpolicy.Snapshot{}, func() {}, fmt.Errorf("invalid agent-directory launch key %q", launchKey)
	}
	cacheDir := tclcommon.CacheDir()
	if !filepath.IsAbs(cacheDir) {
		return sandboxpolicy.Snapshot{}, func() {}, fmt.Errorf("resolve tclaude cache directory for agent-owned directories")
	}
	cacheDir, err := canonicalizeForSecureMkdir(cacheDir)
	if err != nil {
		return sandboxpolicy.Snapshot{}, func() {}, fmt.Errorf("resolve tclaude cache directory for agent-owned directories: %w", err)
	}
	base := filepath.Join(cacheDir, "agent-dirs")
	root := filepath.Join(base, launchKey)
	cleanup := func() { _ = os.RemoveAll(root) }
	if err := mkdirAllNoFollow(root, 0o700); err != nil {
		return sandboxpolicy.Snapshot{}, func() {}, fmt.Errorf("create agent-owned directory root: %w", err)
	}

	effective := snapshot.Effective
	effective.Filesystem = append([]sandboxpolicy.FilesystemGrant(nil), effective.Filesystem...)
	effective.Environment = append([]sandboxpolicy.EnvironmentEntry(nil), effective.Environment...)
	// A clone can carry the source agent's already-materialized snapshot. Strip
	// those generated bindings before assigning this launch a new root; literal
	// environment entries cannot use these names because profile validation
	// rejects that collision.
	oldPaths := map[string]bool{}
	agentNames := map[string]bool{}
	for _, name := range effective.AgentDirectories {
		agentNames[name] = true
	}
	filteredEnvironment := effective.Environment[:0]
	for _, entry := range effective.Environment {
		if agentNames[entry.Name] {
			oldPaths[entry.Value] = true
			delete(effective.Provenance.Environment, entry.Name)
			continue
		}
		filteredEnvironment = append(filteredEnvironment, entry)
	}
	effective.Environment = filteredEnvironment
	filteredFilesystem := effective.Filesystem[:0]
	for _, grant := range effective.Filesystem {
		if oldPaths[grant.Path] {
			delete(effective.Provenance.Filesystem, grant.Path)
			continue
		}
		filteredFilesystem = append(filteredFilesystem, grant)
	}
	effective.Filesystem = filteredFilesystem
	for _, name := range effective.AgentDirectories {
		dir := filepath.Join(root, name)
		if err := mkdirAllNoFollow(dir, 0o700); err != nil {
			cleanup()
			return sandboxpolicy.Snapshot{}, func() {}, fmt.Errorf("create agent-owned directory for %s: %w", name, err)
		}
		canonical, err := filepath.EvalSymlinks(dir)
		if err != nil {
			cleanup()
			return sandboxpolicy.Snapshot{}, func() {}, fmt.Errorf("canonicalize agent-owned directory for %s: %w", name, err)
		}
		effective.Filesystem = append(effective.Filesystem, sandboxpolicy.FilesystemGrant{
			Path: canonical, Access: sandboxpolicy.AccessWrite,
		})
		effective.Environment = append(effective.Environment, sandboxpolicy.EnvironmentEntry{
			Name: name, Value: canonical,
		})
		sources := effective.Provenance.AgentDirectories[name]
		if len(sources) > 0 {
			effective.Provenance.Filesystem[canonical] = append([]sandboxpolicy.ProfileSource(nil), sources...)
			effective.Provenance.Environment[name] = sources[len(sources)-1]
		}
	}
	materialized := sandboxpolicy.NewSnapshot(effective, snapshot.Applied)
	validated, err := sandboxpolicy.RevalidateSnapshot(materialized)
	if err != nil {
		cleanup()
		return sandboxpolicy.Snapshot{}, func() {}, fmt.Errorf("validate materialized agent-owned directories: %w", err)
	}
	return validated, cleanup, nil
}

// ensureAgentDirectoriesForRelaunch recreates deleted cache directories at
// their frozen paths before resume/reincarnate. It never retargets a binding:
// every declared variable must point beneath agentd's own cache root and end
// in that variable name, and descriptor-relative creation rejects symlinks.
func ensureAgentDirectoriesForRelaunch(snapshot sandboxpolicy.Snapshot) (sandboxpolicy.Snapshot, error) {
	if len(snapshot.Effective.AgentDirectories) == 0 {
		return sandboxpolicy.RevalidateSnapshot(snapshot)
	}
	cacheDir, err := canonicalizeForSecureMkdir(tclcommon.CacheDir())
	if err != nil {
		return sandboxpolicy.Snapshot{}, fmt.Errorf("resolve tclaude cache directory for agent-owned directories: %w", err)
	}
	base := filepath.Clean(filepath.Join(cacheDir, "agent-dirs"))
	if !filepath.IsAbs(base) {
		return sandboxpolicy.Snapshot{}, fmt.Errorf("resolve tclaude cache directory for agent-owned directories")
	}
	if err := mkdirAllNoFollow(base, 0o700); err != nil {
		return sandboxpolicy.Snapshot{}, fmt.Errorf("recreate agent-owned directory cache root: %w", err)
	}
	canonicalBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		return sandboxpolicy.Snapshot{}, fmt.Errorf("canonicalize agent-owned directory cache root: %w", err)
	}
	base = canonicalBase
	bindings := make(map[string]string, len(snapshot.Effective.Environment))
	for _, entry := range snapshot.Effective.Environment {
		bindings[entry.Name] = entry.Value
	}
	for _, name := range snapshot.Effective.AgentDirectories {
		path := filepath.Clean(bindings[name])
		if path == "." || filepath.Base(path) != name {
			return sandboxpolicy.Snapshot{}, fmt.Errorf("agent-owned directory binding for %s is missing or malformed", name)
		}
		rel, err := filepath.Rel(base, path)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return sandboxpolicy.Snapshot{}, fmt.Errorf("agent-owned directory binding for %s escapes tclaude's cache root", name)
		}
		if err := mkdirAllNoFollow(path, 0o700); err != nil {
			return sandboxpolicy.Snapshot{}, fmt.Errorf("recreate agent-owned directory for %s: %w", name, err)
		}
	}
	return sandboxpolicy.RevalidateSnapshot(snapshot)
}

// canonicalizeForSecureMkdir resolves symlinks in the existing prefix while
// retaining any missing suffix for descriptor-relative creation. This matters
// on macOS, where /var is normally a symlink to /private/var.
func canonicalizeForSecureMkdir(path string) (string, error) {
	path = filepath.Clean(path)
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("path is not absolute")
	}
	missing := []string{}
	existing := path
	for {
		if _, err := os.Lstat(existing); err == nil {
			break
		} else if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			return "", fmt.Errorf("no existing ancestor")
		}
		missing = append(missing, filepath.Base(existing))
		existing = parent
	}
	canonical, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return "", err
	}
	for i := len(missing) - 1; i >= 0; i-- {
		canonical = filepath.Join(canonical, missing[i])
	}
	return canonical, nil
}
