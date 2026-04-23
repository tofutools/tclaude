package git

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
)

type FetchParams struct{}

func FetchCmd() *cobra.Command {
	return boa.CmdT[FetchParams]{
		Use:         "fetch",
		Short:       "Fetch remote changes without merging",
		Long:        "Fetch remote conversation changes into the sync repository without merging them into your local conversations.",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(params *FetchParams, cmd *cobra.Command, args []string) {
			if err := runFetch(params); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
		},
	}.ToCobra()
}

func runFetch(_ *FetchParams) error {
	syncDir := SyncDir()

	if !IsInitialized() {
		return fmt.Errorf("git sync not initialized. Run 'tclaude git init <repo-url>' first")
	}

	fmt.Printf("Fetching remote changes...\n")

	fetchCmd := exec.Command("git", "fetch", "origin")
	fetchCmd.Dir = syncDir
	fetchCmd.Stdout = os.Stdout
	fetchCmd.Stderr = os.Stderr
	if err := fetchCmd.Run(); err != nil {
		return fmt.Errorf("git fetch failed: %w", err)
	}

	// Show ahead/behind
	remoteBranch := getRemoteBranch(syncDir)
	if remoteBranch == "" {
		fmt.Printf("Done.\n")
		return nil
	}

	revList := exec.Command("git", "rev-list", "--left-right", "--count", "HEAD...origin/"+remoteBranch)
	revList.Dir = syncDir
	output, err := revList.Output()
	if err != nil {
		fmt.Printf("Done.\n")
		return nil
	}

	parts := strings.Fields(strings.TrimSpace(string(output)))
	if len(parts) != 2 {
		fmt.Printf("Done.\n")
		return nil
	}

	ahead := parts[0]
	behind := parts[1]

	if ahead == "0" && behind == "0" {
		fmt.Printf("Already up to date.\n")
	} else {
		if behind != "0" {
			fmt.Printf("Remote has %s new commit(s) available.\n", behind)

			// Show what's incoming: list changed files from remote
			diffCmd := exec.Command("git", "diff", "--stat", "HEAD...origin/"+remoteBranch)
			diffCmd.Dir = syncDir
			diffOutput, err := diffCmd.Output()
			if err == nil && len(diffOutput) > 0 {
				fmt.Printf("\n%s", diffOutput)
			}
		}
		if ahead != "0" {
			fmt.Printf("Local has %s commit(s) not yet pushed.\n", ahead)
		}
		fmt.Printf("\nRun 'tclaude git sync' to merge and push changes.\n")
	}

	return nil
}
