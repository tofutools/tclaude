package memoryfiles

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
)

// PruneIndexParams configures `memory-files prune-index`.
type PruneIndexParams struct {
	Dir string `pos:"true" optional:"true" help:"Project directory whose MEMORY.md index to tidy (defaults to current directory)"`

	// Mirror clean's short-flag layout: -y / -n bind the same way.
	Yes    bool `short:"y" long:"yes" help:"Skip the confirmation prompt and prune."`
	DryRun bool `short:"n" long:"dry-run" help:"Show which dangling entries would be pruned, without changing anything."`

	Prefix     bool `long:"prefix" help:"Scan sibling project dirs by encoded-name prefix instead of git worktrees (may over-match child dirs / dotted siblings)."`
	NoSiblings bool `long:"no-siblings" help:"Tidy only the exact project dir; ignore worktrees and prefix siblings. Takes precedence over --prefix."`
}

// PruneIndexCmd returns the `memory-files prune-index` subcommand.
func PruneIndexCmd() *cobra.Command {
	return boa.CmdT[PruneIndexParams]{
		Use:     "prune-index",
		Aliases: []string{"tidy-index", "gc-index"},
		Short:   "Remove dangling entries from a project's MEMORY.md index",
		Long: "Remove dangling entries from Claude Code's per-project memory index\n" +
			"(MEMORY.md) — list items that link to a memory file which no longer\n" +
			"exists on disk. `clean` already tidies entries for the files it deletes;\n" +
			"this command sweeps up entries orphaned some other way (a file deleted by\n" +
			"hand, or by Claude itself).\n\n" +
			"Only list-item lines with a link to a missing local file are removed;\n" +
			"headers, prose, blockquotes, and links to URLs are left untouched. The\n" +
			"sibling-scan strategy matches the other subcommands: default = the target\n" +
			"repo's live git worktrees, --prefix = encoded-name-prefix scan, --no-siblings\n" +
			"= the exact dir only. The full list of entries to prune is shown first;\n" +
			"without --dry-run or --yes you are asked to confirm.",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(params *PruneIndexParams, _ *cobra.Command, _ []string) {
			if code := RunPruneIndex(params, os.Stdout, os.Stderr, os.Stdin); code != 0 {
				os.Exit(code)
			}
		},
	}.ToCobra()
}

// RunPruneIndex is the testable core of `memory-files prune-index`.
func RunPruneIndex(params *PruneIndexParams, stdout, stderr, stdin *os.File) int {
	targetDir, err := resolveTargetDir(params.Dir)
	if err != nil {
		fmt.Fprintf(stderr, "Error getting current directory: %v\n", err)
		return 1
	}

	res, err := resolveProjectDirs(targetDir, scanModeFrom(params.NoSiblings, params.Prefix))
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}
	if res.note != "" {
		fmt.Fprintf(stderr, "Note: %s\n", res.note)
	}
	if len(res.dirs) == 0 {
		fmt.Fprintf(stdout, "No Claude project directories found for %s (encoded: %s).\n", targetDir, res.encoded)
		return 0
	}

	// Compute dangling entries per dir (dry-run read; nothing written yet).
	var plans []indexPrunePlan
	total := 0
	for _, pd := range res.dirs {
		memDir := filepath.Join(pd, "memory")
		entries, perr := pruneIndexFile(memDir, nil, true, true) // treat absent-on-disk as gone; dry-run read
		if perr != nil {
			fmt.Fprintf(stderr, "Error reading %s: %v\n", filepath.Join(memDir, memoryIndexFile), perr)
			continue
		}
		if len(entries) > 0 {
			plans = append(plans, indexPrunePlan{memDir: memDir, entries: entries})
			total += len(entries)
		}
	}

	if total == 0 {
		fmt.Fprintf(stdout, "No dangling MEMORY.md entries found across %d project dir(s).\n", len(res.dirs))
		return 0
	}

	fmt.Fprintf(stdout, "Dangling MEMORY.md %s across %d index file(s):\n", nEntries(total), len(plans))
	printIndexPlan(stdout, plans)

	if params.DryRun {
		fmt.Fprintf(stdout, "\nDry run — no entries pruned.\n")
		return 0
	}

	if !params.Yes {
		fmt.Fprintf(stdout, "\nPrune %s? [y/N]: ", nEntries(total))
		reader := bufio.NewReader(stdin)
		// EOF / empty stdin yields "" → treated as "no": fails safe to abort.
		resp, _ := reader.ReadString('\n')
		resp = strings.TrimSpace(strings.ToLower(resp))
		if resp != "y" && resp != "yes" {
			fmt.Fprintf(stdout, "Aborted.\n")
			return 0
		}
	}

	pruned, failed := 0, 0
	for _, p := range plans {
		removed, perr := pruneIndexFile(p.memDir, nil, true, false) // treat absent-on-disk as gone; write
		if perr != nil {
			fmt.Fprintf(stderr, "Error pruning %s: %v\n", filepath.Join(p.memDir, memoryIndexFile), perr)
			failed++
			continue
		}
		pruned += len(removed)
	}

	fmt.Fprintf(stdout, "Pruned %s from %d index file(s).\n", nEntries(pruned), len(plans))
	if failed > 0 {
		fmt.Fprintf(stderr, "Failed to prune %d index file(s).\n", failed)
		return 1
	}
	return 0
}
