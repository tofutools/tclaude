package conv

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/convops"
	"github.com/tofutools/tclaude/pkg/common"
)

type CpParams struct {
	ConvID   string `pos:"true" help:"Conversation ID to copy"`
	DestPath string `pos:"true" help:"Destination directory path (real path, not Claude project path)"`
	Force    bool   `short:"f" help:"Force overwrite without confirmation"`
	Global   bool   `short:"g" help:"Search for conversation across all projects"`
}

func CpCmd() *cobra.Command {
	return boa.CmdT[CpParams]{
		Use:         "cp",
		Short:       "Copy a Claude conversation to another project directory",
		Long:        "Copy a Claude Code conversation from the current directory to another project directory.\nThe destination path should be a real filesystem path (e.g., /home/user/myproject).\nThe copied conversation gets a new UUID.",
		ParamEnrich: common.DefaultParamEnricher(),
		ValidArgsFunc: func(p *CpParams, cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			if len(args) == 0 {
				global, _ := cmd.Flags().GetBool("global")
				return clcommon.GetConversationCompletions(global), cobra.ShellCompDirectiveKeepOrder | cobra.ShellCompDirectiveNoFileComp
			}
			return nil, cobra.ShellCompDirectiveDefault
		},
		RunFunc: func(params *CpParams, cmd *cobra.Command, args []string) {
			exitCode := RunCp(params, cliStdout, cliStderr, os.Stdin)
			if exitCode != 0 {
				os.Exit(exitCode)
			}
		},
	}.ToCobra()
}

func RunCp(params *CpParams, stdout, stderr io.Writer, stdin *os.File) int {
	// Extract just the ID from autocomplete format (e.g., "0459cd73_[myproject]_prompt..." -> "0459cd73")
	convID := clcommon.ExtractIDFromCompletion(params.ConvID)

	src, err := FindSessionInProjects(convID, params.Global)
	if err != nil {
		fmt.Fprintf(stderr, "Error %v\n", err)
		return 1
	}
	if src.Entry == nil {
		fmt.Fprintf(stderr, "Conversation %s not found\n", convID)
		if !params.Global {
			fmt.Fprintf(stderr, "Hint: use -g to search all projects\n")
		}
		return 1
	}

	copied, err := convops.CopyFoundConversation(src, params.DestPath, convops.CopyOptions{
		ConfirmOverwrite: func() bool {
			if params.Force {
				return true
			}
			fmt.Fprintf(stdout, "Destination file already exists (unlikely UUID collision!).\n")
			fmt.Fprintf(stdout, "Overwrite? [y/N]: ")
			response, _ := bufio.NewReader(stdin).ReadString('\n')
			response = strings.TrimSpace(strings.ToLower(response))
			return response == "y" || response == "yes"
		},
		IndexWarning: func(dstProjectPath string, err error) {
			fmt.Fprintf(stderr, "Warning: failed to update sessions-index.json for %s: %v\n", dstProjectPath, err)
		},
	})
	if errors.Is(err, convops.ErrOverwriteDeclined) {
		fmt.Fprintf(stdout, "Aborted.\n")
		return 0
	}
	if err != nil {
		fmt.Fprintf(stderr, "Error %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "Copied conversation %s -> %s in %s\n", copied.OldConvID[:8], copied.NewConvID[:8], params.DestPath)
	return 0
}
