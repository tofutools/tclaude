package conv

import (
	"fmt"
	"io"
	"os"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/common"
)

// `tclaude conv archive <selector>` and `tclaude conv unarchive
// <selector>` — manual flips of `conv_index.archived_at` (schema
// v17). Reincarnate already stamps the column automatically; these
// verbs let a human archive arbitrary convs (orphans, abandoned
// experiments, etc.) without going through the rename mechanism.
//
// Same column-based archived semantics as on the group side
// (`groups archive`). Listing surfaces (`conv ls`) hide archived
// rows by default; `--show-archived` reveals them.

type ArchiveParams struct {
	ConvID string `pos:"true" help:"Conversation ID (full UUID or 8+-char prefix)"`
}

func ArchiveCmd() *cobra.Command {
	return boa.CmdT[ArchiveParams]{
		Use:   "archive",
		Short: "Mark a conversation as archived (hidden from default listings)",
		Long: "Stamps `conv_index.archived_at` so the conv is hidden from " +
			"`conv ls` by default. The .jsonl file on disk stays untouched; " +
			"reverse with `conv unarchive`. The same column reincarnate " +
			"writes on the OLD conv after spawning a successor — this " +
			"verb is for the manual cleanup case (orphans, abandoned " +
			"experiments).",
		ParamEnrich: common.DefaultParamEnricher(),
		ValidArgsFunc: func(p *ArchiveParams, cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			if len(args) == 0 {
				return clcommon.GetConversationCompletions(true), cobra.ShellCompDirectiveKeepOrder | cobra.ShellCompDirectiveNoFileComp
			}
			return nil, cobra.ShellCompDirectiveNoFileComp
		},
		RunFunc: func(p *ArchiveParams, _ *cobra.Command, _ []string) {
			os.Exit(runArchiveOrUnarchive(p.ConvID, true, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func UnarchiveCmd() *cobra.Command {
	return boa.CmdT[ArchiveParams]{
		Use:   "unarchive",
		Short: "Reverse `conv archive` — clear the archived flag on a conversation",
		Long: "Clears `conv_index.archived_at` so the conv reappears in " +
			"default `conv ls` output. Idempotent on already-active convs. " +
			"Since JOH-320 the archived_at column is the sole visibility " +
			"signal, so this reveals the conv even if its title still carries " +
			"the cosmetic `-x` reincarnation marker — no need to rename it back.",
		ParamEnrich: common.DefaultParamEnricher(),
		ValidArgsFunc: func(p *ArchiveParams, cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			if len(args) == 0 {
				return clcommon.GetConversationCompletions(true), cobra.ShellCompDirectiveKeepOrder | cobra.ShellCompDirectiveNoFileComp
			}
			return nil, cobra.ShellCompDirectiveNoFileComp
		},
		RunFunc: func(p *ArchiveParams, _ *cobra.Command, _ []string) {
			os.Exit(runArchiveOrUnarchive(p.ConvID, false, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runArchiveOrUnarchive(convSelector string, archive bool, stdout, stderr io.Writer) int {
	if convSelector == "" {
		fmt.Fprintln(stderr, "Error: a conv-id (full or 8+-char prefix) is required")
		return 1
	}
	// Strip the autocomplete decoration ("0459cd73_[title]_prompt..."
	// → "0459cd73") so we can match a bare prefix.
	convID := clcommon.ExtractIDFromCompletion(convSelector)

	// Resolve to a full conv-id via the DB. Full UUID match first,
	// then prefix.
	row, err := db.GetConvIndex(convID)
	if err != nil {
		fmt.Fprintf(stderr, "Error: lookup: %v\n", err)
		return 1
	}
	if row == nil {
		row, err = db.FindConvIndexByPrefix(convID)
		if err != nil {
			fmt.Fprintf(stderr, "Error: prefix lookup: %v\n", err)
			return 1
		}
	}
	if row == nil {
		fmt.Fprintf(stderr, "Error: no conversation matches %q (try `tclaude conv ls -g` to see all)\n", convID)
		return 1
	}

	if err := db.SetConvIndexArchived(row.ConvID, archive); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}
	verb := "Archived"
	if !archive {
		verb = "Unarchived"
	}
	title := row.CustomTitle
	if title == "" {
		title = row.Summary
	}
	if title == "" {
		title = row.FirstPrompt
	}
	if title != "" {
		fmt.Fprintf(stdout, "%s %s — %s\n", verb, row.ConvID[:8], title)
	} else {
		fmt.Fprintf(stdout, "%s %s\n", verb, row.ConvID[:8])
	}
	return 0
}
