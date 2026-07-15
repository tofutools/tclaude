package agentd

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	tclcommon "github.com/tofutools/tclaude/pkg/common"
)

var agentDirectoryLaunchKeyRE = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// agentDirsMountParentEnabled reports whether shared-parent grants are enabled.
// A failed load fails closed to individual per-directory grants. Read at
// materialization/resume so flipping the setting takes effect on the next
// launch without a daemon restart.
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
	for _, b := range bindings {
		path := b.canonical
		if mountParent {
			// Several directories can share one parent root; ensureAgentDirWriteGrant
			// grants it once and unions the provenance across every contributor.
			path = filepath.Dir(b.canonical)
		}
		ensureAgentDirWriteGrant(effective, path, b.sources)
	}
}

// ensureAgentDirWriteGrant makes path writable exactly once, mirroring
// normalizeFilesystem's "one entry per path, highest access rank wins" rule so
// the materialized snapshot survives RevalidateSnapshot (which normalizes and
// then rejects any pre-normalization drift). Blindly appending a grant for a
// path that already carries one — a sibling agent dir sharing a parent, or a
// user who explicitly granted the agent-dirs base as a manual workaround — would
// leave a duplicate that normalization collapses, failing revalidation. A
// pre-existing read grant is upgraded to write; an existing write or deny grant
// is left as-is (write is already sufficient; a user deny must still win).
func ensureAgentDirWriteGrant(effective *sandboxpolicy.EffectiveProfile, path string, sources []sandboxpolicy.ProfileSource) {
	for i := range effective.Filesystem {
		if effective.Filesystem[i].Path != path {
			continue
		}
		if effective.Filesystem[i].Access == sandboxpolicy.AccessRead {
			effective.Filesystem[i].Access = sandboxpolicy.AccessWrite
		}
		unionFilesystemProvenance(effective, path, sources)
		return
	}
	effective.Filesystem = append(effective.Filesystem, sandboxpolicy.FilesystemGrant{
		Path: path, Access: sandboxpolicy.AccessWrite,
	})
	unionFilesystemProvenance(effective, path, sources)
}

// unionFilesystemProvenance adds sources to the provenance for path without
// duplicates, preserving any sources already recorded (e.g. a user grant the
// agent-dir grant now shares a path with).
func unionFilesystemProvenance(effective *sandboxpolicy.EffectiveProfile, path string, sources []sandboxpolicy.ProfileSource) {
	if len(sources) == 0 {
		return
	}
	existing := effective.Provenance.Filesystem[path]
	seen := make(map[sandboxpolicy.ProfileSource]bool, len(existing))
	for _, source := range existing {
		seen[source] = true
	}
	for _, source := range sources {
		if seen[source] {
			continue
		}
		seen[source] = true
		existing = append(existing, source)
	}
	effective.Provenance.Filesystem[path] = existing
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
	cleanup := func() { _, _ = removeDirAtNoFollow(base, launchKey) }
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

// removeMaterializedAgentDirectories removes every cache root represented by
// the frozen agent-owned directory bindings in snapshot. A single root can
// contain several declared variables, so removing the root also sweeps stale
// siblings left behind when a resumed profile stopped declaring one of them.
//
// Recursive deletion is deliberately limited to direct children of agentd's
// own agent-dirs cache root. The frozen environment values are durable state,
// not trusted deletion authority: a missing, malformed, or out-of-tree binding
// fails validation before any root is removed.
func removeMaterializedAgentDirectories(snapshot sandboxpolicy.Snapshot) (int, error) {
	base, roots, err := materializedAgentDirectoryRoots(snapshot)
	if err != nil {
		return 0, err
	}
	return removeMaterializedAgentDirectoryRoots(base, roots)
}

func removeAgentDirectoriesForConv(convID string) (int, error) {
	snapshot, err := db.AgentEffectiveSandboxConfigForConv(convID)
	if err != nil || snapshot == nil {
		return 0, err
	}
	return removeMaterializedAgentDirectories(*snapshot)
}

// cleanupAgentDirectoriesAfterRetire removes an offline retired agent's cache
// roots immediately. When a soft shutdown is in flight it waits for the pane to
// exit first, avoiding recursive deletion while the harness is still writing.
// An explicit retire-without-shutdown retains the historical immediate cleanup
// contract: the cache is disposable even though the conversation keeps running.
func cleanupAgentDirectoriesAfterRetire(convID string, shutdownRequested bool) {
	cleanupLocked := func() {
		state, err := db.AgentState(convID)
		if err != nil || state != db.AgentStateRetired {
			return
		}
		removed, cleanupErr := removeAgentDirectoriesForConv(convID)
		if cleanupErr != nil {
			slog.Warn("retire: agent-owned directory cleanup failed",
				"conv", convID, "removed_roots", removed, "error", cleanupErr)
			postRetireWorktreeNotice(agent.FreshTitle(convID),
				"Retire agent-owned directory cleanup failed",
				"agent-owned cache directories were kept: "+cleanupErr.Error())
		}
	}
	launchLock := resumeLaunchLock(convID)
	launchLock.Lock()
	if pickAliveSession(convID) == nil || !shutdownRequested {
		cleanupLocked()
		launchLock.Unlock()
		return
	}
	launchLock.Unlock()
	goBackground(func() {
		if waitForConvOffline(convID, retireWorktreeExitGrace) {
			launchLock.Lock()
			defer launchLock.Unlock()
			cleanupLocked()
			return
		}
		slog.Warn("retire: agent-owned directories kept because agent did not exit within grace",
			"conv", convID, "grace", retireWorktreeExitGrace)
		postRetireWorktreeNotice(agent.FreshTitle(convID),
			"Retire agent-owned directories kept",
			"agent-owned cache directories were kept because the agent did not exit within "+retireWorktreeExitGrace.String())
	})
}

func materializedAgentDirectoryRoots(snapshot sandboxpolicy.Snapshot) (string, map[string]string, error) {
	cacheDir := tclcommon.CacheDir()
	if !filepath.IsAbs(cacheDir) {
		return "", nil, fmt.Errorf("resolve tclaude cache directory for agent-owned directories")
	}
	cacheDir, err := canonicalizeForSecureMkdir(cacheDir)
	if err != nil {
		return "", nil, fmt.Errorf("resolve tclaude cache directory for agent-owned directories: %w", err)
	}
	base := filepath.Clean(filepath.Join(cacheDir, "agent-dirs"))
	if len(snapshot.Effective.AgentDirectories) == 0 {
		return base, map[string]string{}, nil
	}

	bindings := make(map[string]string, len(snapshot.Effective.Environment))
	for _, entry := range snapshot.Effective.Environment {
		bindings[entry.Name] = entry.Value
	}
	roots := make(map[string]string, len(snapshot.Effective.AgentDirectories))
	for _, name := range snapshot.Effective.AgentDirectories {
		path := filepath.Clean(bindings[name])
		if path == "." || !filepath.IsAbs(path) || filepath.Base(path) != name {
			return "", nil, fmt.Errorf("agent-owned directory binding for %s is missing or malformed", name)
		}
		root := filepath.Dir(path)
		rel, err := filepath.Rel(base, root)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) ||
			strings.Contains(rel, string(filepath.Separator)) || !agentDirectoryLaunchKeyRE.MatchString(rel) {
			return "", nil, fmt.Errorf("agent-owned directory binding for %s escapes its launch root", name)
		}
		roots[root] = rel
	}
	return base, roots, nil
}

func removeMaterializedAgentDirectoryRoots(base string, roots map[string]string) (int, error) {
	var removed int
	var errs []error
	for _, rel := range roots {
		didRemove, err := removeDirAtNoFollow(base, rel)
		if err != nil {
			errs = append(errs, fmt.Errorf("remove agent-owned directory root: %w", err))
			continue
		}
		if didRemove {
			removed++
		}
	}
	return removed, errors.Join(errs...)
}

// removeSupersededMaterializedAgentDirectories removes roots referenced only
// by previous. Resume calls this before replacing the persisted actor snapshot,
// so profile removal cannot discard the sole durable reference to an old root.
func removeSupersededMaterializedAgentDirectories(previous, current sandboxpolicy.Snapshot) (int, error) {
	base, previousRoots, err := materializedAgentDirectoryRoots(previous)
	if err != nil {
		return 0, err
	}
	_, currentRoots, err := materializedAgentDirectoryRoots(current)
	if err != nil {
		return 0, err
	}
	for root := range currentRoots {
		delete(previousRoots, root)
	}
	return removeMaterializedAgentDirectoryRoots(base, previousRoots)
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
