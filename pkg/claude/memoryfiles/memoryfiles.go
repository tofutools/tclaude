// Package memoryfiles provides utilities for managing Claude Code's
// per-project memory files — the markdown files Claude persists under
// ~/.claude/projects/<encoded-dir>/memory/.
//
// Over a long-lived project these files can drift, contradict each
// other, or leak stale context between agents that share the encoded
// project prefix (worktree siblings). The `tclaude memory-files`
// command group lets you preview and prune them safely.
package memoryfiles

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/common/convops"
	"github.com/tofutools/tclaude/pkg/common"
)

// Params is the (empty) parameter set for the parent group.
type Params struct{}

// Cmd returns the `memory-files` command group.
func Cmd() *cobra.Command {
	return boa.CmdT[Params]{
		Use:     "memory-files",
		Aliases: []string{"mem-files", "memory"},
		Short:   "Inspect and clean Claude Code per-project memory files",
		Long: "Inspect and clean the markdown memory files Claude Code persists under\n" +
			"~/.claude/projects/<encoded-dir>/memory/.\n\n" +
			"Memory can drift, become contradictory, or cross-poison agents that share\n" +
			"a project's encoded prefix (its worktree siblings). These utilities let you\n" +
			"preview and prune those files for a project and, by default, its siblings.",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds: []*cobra.Command{
			LsCmd(),
			CatCmd(),
			CleanCmd(),
			PruneIndexCmd(),
		},
		// No RunFunc: invoking the bare group prints help (cobra default).
	}.ToCobra()
}

// memFile is a single top-level .md memory file. `del` is only used by
// `clean`, where it carries the include/exclude classification; `ls`
// and `cat` ignore it.
type memFile struct {
	projectDir string    // ~/.claude/projects/<encoded...>
	memoryDir  string    // <projectDir>/memory
	rel        string    // file name (memory/ is treated as flat; we never recurse)
	abs        string    // absolute path on disk
	size       int64     // file size in bytes
	modTime    time.Time // last-modified time
	del        bool      // clean: true => matches the delete filters
}

// listMemoryMD returns the top-level .md files directly inside each
// project dir's memory/ subdir. Subdirectories are NOT traversed (a
// stray .idea/ that some editors drop into memory/ is ignored) and
// non-.md files are skipped, so the only thing any subcommand ever
// touches is the markdown memory itself. Project dirs without a
// readable memory/ subdir are skipped. The result is sorted by
// (projectDir, name) for deterministic output.
func listMemoryMD(projectDirs []string) []memFile {
	var out []memFile
	for _, pd := range projectDirs {
		memDir := filepath.Join(pd, "memory")
		entries, err := os.ReadDir(memDir)
		if err != nil {
			continue // no memory/ dir here (or it's a file / unreadable)
		}
		for _, e := range entries {
			if e.IsDir() {
				continue // never descend into subdirs
			}
			name := e.Name()
			if !strings.EqualFold(filepath.Ext(name), ".md") {
				continue // .md files only
			}
			f := memFile{
				projectDir: pd,
				memoryDir:  memDir,
				rel:        name,
				abs:        filepath.Join(memDir, name),
			}
			if info, infoErr := e.Info(); infoErr == nil {
				f.size = info.Size()
				f.modTime = info.ModTime()
			}
			out = append(out, f)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].projectDir != out[j].projectDir {
			return out[i].projectDir < out[j].projectDir
		}
		return out[i].rel < out[j].rel
	})
	return out
}

// resolveTargetDir returns dir, or the current working directory when
// dir is empty (the positional arg is optional across the subcommands).
func resolveTargetDir(dir string) (string, error) {
	if dir != "" {
		return dir, nil
	}
	return os.Getwd()
}

// scanMode selects how a target project dir is expanded into the set of
// Claude projects dirs to operate on.
type scanMode int

const (
	// scanWorktrees (default) maps the target to its repo's live git
	// worktrees and operates on each worktree's projects dir. Precise:
	// no child-dir / dotted-sibling false positives. Does NOT find
	// memory orphaned by worktrees that were already removed.
	scanWorktrees scanMode = iota
	// scanPrefix matches every projects dir whose encoded name equals
	// or is prefixed by "<encoded>-". Greedy: catches leftover memory
	// from deleted worktrees, but because PathToProjectDir collapses
	// '/' and '.' to '-' it can also match child dirs (<target>/sub) and
	// dotted siblings (<target>.bak). Opt in with --prefix.
	scanPrefix
	// scanExact operates only on the target's own projects dir.
	scanExact
)

// scanModeFrom derives the strategy from the shared flags. --no-siblings
// (exact) takes precedence over --prefix; otherwise the default is the
// git-worktrees strategy.
func scanModeFrom(noSiblings, prefix bool) scanMode {
	switch {
	case noSiblings:
		return scanExact
	case prefix:
		return scanPrefix
	default:
		return scanWorktrees
	}
}

// scanResult is the outcome of resolving a target to projects dirs.
type scanResult struct {
	dirs    []string // matched ~/.claude/projects/<encoded...> dirs (sorted, existing)
	encoded string   // the target's encoded name (handy when nothing matched)
	note    string   // optional human-facing note (e.g. non-repo fallback)
}

// resolveProjectDirs maps a real project directory to the matching
// Claude projects dirs on disk, per the chosen scan mode. Claude encodes
// an absolute path by replacing each '/' and '.' with '-' and dropping
// ':' (see convops.PathToProjectDir).
func resolveProjectDirs(targetDir string, mode scanMode) (scanResult, error) {
	abs, err := filepath.Abs(targetDir)
	if err != nil {
		abs = targetDir
	}
	encoded := convops.PathToProjectDir(abs)
	res := scanResult{encoded: encoded}

	projectsDir := convops.ClaudeProjectsDir()
	if projectsDir == "" {
		return res, fmt.Errorf("could not determine Claude projects directory (no home dir?)")
	}

	switch mode {
	case scanExact:
		if dir := filepath.Join(projectsDir, encoded); isDir(dir) {
			res.dirs = []string{dir}
		}
	case scanWorktrees:
		paths, gitErr := gitWorktreePaths(abs)
		if gitErr != nil {
			// Not a git repo (or git unavailable): operate on the exact
			// dir only, and point the user at --prefix for a broader scan.
			res.note = fmt.Sprintf("%s is not a git worktree (or git is unavailable); scanning the exact project dir only — use --prefix to scan sibling dirs by encoded-name prefix.", targetDir)
			if dir := filepath.Join(projectsDir, encoded); isDir(dir) {
				res.dirs = []string{dir}
			}
		} else {
			seen := map[string]bool{}
			add := func(dir string) {
				if !seen[dir] && isDir(dir) {
					seen[dir] = true
					res.dirs = append(res.dirs, dir)
				}
			}
			// git worktree list reports git's view of each worktree ROOT
			// (a canonical/real path). In normal usage — a project tree not
			// living under a symlinked prefix — that equals the path Claude
			// saw, so the encodings match.
			for _, p := range paths {
				add(filepath.Join(projectsDir, convops.PathToProjectDir(p)))
			}
			// Always include the dir the user actually named. git only
			// reports worktree roots, so when the target is a SUBDIR of the
			// repo (e.g. repo/sub, where Claude stored memory under
			// ...-repo-sub) its own dir is not in `paths` and would
			// otherwise be silently dropped. Encoded via Abs to match how
			// the exact/prefix modes — and Claude — name the dir, so the
			// named target is covered regardless of symlink canonicalization.
			add(filepath.Join(projectsDir, encoded))
		}
	case scanPrefix:
		// Refuse a degenerate target whose encoded form is empty or all
		// dashes (e.g. "/", which encodes to "-"): its sibling prefix
		// would match a huge swath of ~/.claude/projects. No real
		// project path encodes this short.
		if strings.Trim(encoded, "-") == "" {
			return res, fmt.Errorf("refusing to operate on root/degenerate target %q (encoded %q)", targetDir, encoded)
		}
		entries, readErr := os.ReadDir(projectsDir)
		if readErr != nil {
			return res, fmt.Errorf("reading %s: %w", projectsDir, readErr)
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			name := e.Name()
			if name == encoded || strings.HasPrefix(name, encoded+"-") {
				res.dirs = append(res.dirs, filepath.Join(projectsDir, name))
			}
		}
	}

	sort.Strings(res.dirs)
	return res, nil
}

// isDir reports whether path exists and is a directory.
func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// gitWorktreePaths returns the absolute root paths of every worktree of
// the git repo containing targetDir, via `git worktree list`. It errors
// when targetDir is not inside a git repo or git is unavailable.
func gitWorktreePaths(targetDir string) ([]string, error) {
	cmd := exec.Command("git", "-C", targetDir, "worktree", "list", "--porcelain")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, line := range strings.Split(string(out), "\n") {
		// Each worktree block starts with: "worktree <abs-path>".
		if p, ok := strings.CutPrefix(line, "worktree "); ok {
			// git emits forward slashes on every platform; convert to the
			// OS separator so PathToProjectDir (which splits on
			// filepath.Separator) encodes Windows paths the same way Claude
			// does. No-op on Unix.
			if p = filepath.FromSlash(strings.TrimSpace(p)); p != "" {
				paths = append(paths, p)
			}
		}
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("no worktrees reported for %s", targetDir)
	}
	return paths, nil
}
