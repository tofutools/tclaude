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
			CleanCmd(),
		},
		// No RunFunc: invoking the bare group prints help (cobra default).
	}.ToCobra()
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
