package agentd

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	tclcommon "github.com/tofutools/tclaude/pkg/common"
)

var agentDirectoryLaunchKeyRE = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// agentDirsMountParentEnabled reports whether the experimental
// features.agent_dirs_mount_parent flag is set. A failed load degrades to the
// default (individual per-directory grants). Read at materialization/resume so
// flipping the flag takes effect on the next launch without a daemon restart.
func agentDirsMountParentEnabled() bool {
	cfg, err := config.Load()
	return err == nil && cfg.AgentDirsMountParentEnabled()
}

// agentDirBinding pairs a materialized agent-owned directory with the profile
// sources that declared it, so write grants can carry provenance regardless of
// whether they land per-directory or on a shared parent root.
type agentDirBinding struct {
	canonical string
	sources   []sandboxpolicy.ProfileSource
}

// addAgentDirectoryWriteGrants appends the write grants for the given agent-owned
// directories. When mountParent is set, each directory's parent root is granted
// once (deduped) so the agent can create, rewrite, and delete its own env-var'd
// directories; otherwise each directory is granted individually. Environment
// entries are set by the caller either way — only the write surface differs.
func addAgentDirectoryWriteGrants(effective *sandboxpolicy.EffectiveProfile, mountParent bool, bindings []agentDirBinding) {
	if mountParent {
		granted := map[string]bool{}
		for _, b := range bindings {
			parent := filepath.Dir(b.canonical)
			if granted[parent] {
				continue
			}
			granted[parent] = true
			effective.Filesystem = append(effective.Filesystem, sandboxpolicy.FilesystemGrant{
				Path: parent, Access: sandboxpolicy.AccessWrite,
			})
			if len(b.sources) > 0 {
				effective.Provenance.Filesystem[parent] = append([]sandboxpolicy.ProfileSource(nil), b.sources...)
			}
		}
		return
	}
	for _, b := range bindings {
		effective.Filesystem = append(effective.Filesystem, sandboxpolicy.FilesystemGrant{
			Path: b.canonical, Access: sandboxpolicy.AccessWrite,
		})
		if len(b.sources) > 0 {
			effective.Provenance.Filesystem[b.canonical] = append([]sandboxpolicy.ProfileSource(nil), b.sources...)
		}
	}
}

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
	// rejects that collision. The source may have been materialized under either
	// grant mode, so strip both the per-directory grants (env values) and the
	// mount-parent grants (their parent roots).
	oldPaths := map[string]bool{}
	agentNames := map[string]bool{}
	for _, name := range effective.AgentDirectories {
		agentNames[name] = true
	}
	filteredEnvironment := effective.Environment[:0]
	for _, entry := range effective.Environment {
		if agentNames[entry.Name] {
			oldPaths[entry.Value] = true
			oldPaths[filepath.Dir(entry.Value)] = true
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
	bindings := make([]agentDirBinding, 0, len(effective.AgentDirectories))
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
		effective.Environment = append(effective.Environment, sandboxpolicy.EnvironmentEntry{
			Name: name, Value: canonical,
		})
		sources := effective.Provenance.AgentDirectories[name]
		if len(sources) > 0 {
			effective.Provenance.Environment[name] = sources[len(sources)-1]
		}
		bindings = append(bindings, agentDirBinding{canonical: canonical, sources: sources})
	}
	addAgentDirectoryWriteGrants(&effective, agentDirsMountParentEnabled(), bindings)
	materialized := sandboxpolicy.NewSnapshot(effective, snapshot.Applied)
	materialized.ResolutionGroupID = snapshot.ResolutionGroupID
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

// reconcileAgentDirectoriesForResume applies a freshly-resolved profile while
// retaining the private directory bindings that belong to this stable agent.
// Existing declarations keep their frozen paths; newly-added declarations get
// a stable actor-keyed path. Removed declarations disappear with the rest of
// the old profile rather than leaking into the relaunched sandbox.
func reconcileAgentDirectoriesForResume(current, previous sandboxpolicy.Snapshot, agentID string) (sandboxpolicy.Snapshot, error) {
	if len(current.Effective.AgentDirectories) == 0 {
		return sandboxpolicy.RevalidateSnapshot(current)
	}
	if len(previous.Effective.AgentDirectories) > 0 {
		var err error
		previous, err = ensureAgentDirectoriesForRelaunch(previous)
		if err != nil {
			return sandboxpolicy.Snapshot{}, err
		}
	}

	oldBindings := make(map[string]string, len(previous.Effective.Environment))
	oldNames := make(map[string]bool, len(previous.Effective.AgentDirectories))
	for _, name := range previous.Effective.AgentDirectories {
		oldNames[name] = true
	}
	for _, entry := range previous.Effective.Environment {
		if oldNames[entry.Name] {
			oldBindings[entry.Name] = entry.Value
		}
	}

	agentID = strings.TrimSpace(agentID)
	if !agentDirectoryLaunchKeyRE.MatchString(agentID) {
		return sandboxpolicy.Snapshot{}, fmt.Errorf("invalid stable agent key for resumed agent-owned directories")
	}
	cacheDir, err := canonicalizeForSecureMkdir(tclcommon.CacheDir())
	if err != nil {
		return sandboxpolicy.Snapshot{}, fmt.Errorf("resolve tclaude cache directory for agent-owned directories: %w", err)
	}
	root := filepath.Join(cacheDir, "agent-dirs", agentID)
	if err := mkdirAllNoFollow(root, 0o700); err != nil {
		return sandboxpolicy.Snapshot{}, fmt.Errorf("create resumed agent-owned directory root: %w", err)
	}

	effective := current.Effective
	effective.Filesystem = append([]sandboxpolicy.FilesystemGrant(nil), effective.Filesystem...)
	effective.Environment = append([]sandboxpolicy.EnvironmentEntry(nil), effective.Environment...)
	bindings := make([]agentDirBinding, 0, len(effective.AgentDirectories))
	for _, name := range effective.AgentDirectories {
		path := oldBindings[name]
		if path == "" {
			path = filepath.Join(root, name)
			if err := mkdirAllNoFollow(path, 0o700); err != nil {
				return sandboxpolicy.Snapshot{}, fmt.Errorf("create resumed agent-owned directory for %s: %w", name, err)
			}
			path, err = filepath.EvalSymlinks(path)
			if err != nil {
				return sandboxpolicy.Snapshot{}, fmt.Errorf("canonicalize resumed agent-owned directory for %s: %w", name, err)
			}
		}
		effective.Environment = append(effective.Environment, sandboxpolicy.EnvironmentEntry{
			Name: name, Value: path,
		})
		sources := effective.Provenance.AgentDirectories[name]
		if len(sources) > 0 {
			effective.Provenance.Environment[name] = sources[len(sources)-1]
		}
		bindings = append(bindings, agentDirBinding{canonical: path, sources: sources})
	}
	// Resume re-reads the flag: existing declarations keep their frozen paths
	// (grant shape follows the current flag), so flipping it takes effect on the
	// next relaunch. Old bindings may sit under a different root than newly-added
	// ones, so mount-parent grants each distinct parent (deduped by the helper).
	addAgentDirectoryWriteGrants(&effective, agentDirsMountParentEnabled(), bindings)
	resumed := sandboxpolicy.NewSnapshot(effective, current.Applied)
	resumed.ResolutionGroupID = current.ResolutionGroupID
	return sandboxpolicy.RevalidateSnapshot(resumed)
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
