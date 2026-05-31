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

// resolveProjectDirs maps a real project directory to the matching
// Claude projects dir(s) on disk. The exact dir is always included if
// present; when includeSiblings is true, worktree-sibling dirs are too.
//
// Claude encodes an absolute path by replacing each '/' and '.' with
// '-' and dropping ':' (see convops.PathToProjectDir). A worktree that
// `tclaude worktree add` creates at <repo>-<branch> therefore encodes
// to <encoded-repo>-<branch>. So the siblings are exactly the projects
// dir entries whose name equals the encoded target OR begins with the
// encoded target followed by '-'. The trailing '-' guard keeps a
// distinct project like "<repo>2" (encodes to "<encoded>2", no dash)
// from matching, while still catching every real worktree.
//
// It returns the matched absolute dirs (sorted) and the encoded name
// used for matching (handy for diagnostics when nothing matched).
func resolveProjectDirs(targetDir string, includeSiblings bool) ([]string, string, error) {
	abs, err := filepath.Abs(targetDir)
	if err != nil {
		abs = targetDir
	}
	encoded := convops.PathToProjectDir(abs)

	projectsDir := convops.ClaudeProjectsDir()
	if projectsDir == "" {
		return nil, encoded, fmt.Errorf("could not determine Claude projects directory (no home dir?)")
	}

	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		return nil, encoded, fmt.Errorf("reading %s: %w", projectsDir, err)
	}

	var dirs []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		switch {
		case name == encoded:
			dirs = append(dirs, filepath.Join(projectsDir, name))
		case includeSiblings && strings.HasPrefix(name, encoded+"-"):
			dirs = append(dirs, filepath.Join(projectsDir, name))
		}
	}
	sort.Strings(dirs)
	return dirs, encoded, nil
}
