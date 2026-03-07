// Package credentials provides the tclaude credentials command for managing
// separate API credentials that don't interfere with Claude Code.
package credentials

import (
	"fmt"
	"os"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/common/usageapi"
	"github.com/tofutools/tclaude/pkg/common"
)

type SplitParams struct{}

func Cmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "credentials",
		Short: "Manage tclaude API credentials",
	}
	cmd.AddCommand(splitCmd())
	cmd.AddCommand(statusCmd())
	return cmd
}

func splitCmd() *cobra.Command {
	return boa.CmdT[SplitParams]{
		Use:   "split",
		Short: "Move Claude credentials to tclaude's own file",
		Long: `Moves Claude Code's OAuth credentials into tclaude's own credential file
(~/.tclaude/api-credentials.json) and removes them from Claude Code's store.

After this, Claude Code will prompt you to log in again on next start,
creating a separate token. This means tclaude and Claude Code each have
their own independent OAuth tokens, avoiding 429 rate limit conflicts
when tclaude fetches usage data.`,
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(params *SplitParams, cmd *cobra.Command, args []string) {
			if err := runSplit(); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
		},
	}.ToCobra()
}

type StatusParams struct{}

func statusCmd() *cobra.Command {
	return boa.CmdT[StatusParams]{
		Use:   "status",
		Short: "Show which credential store tclaude is using",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(params *StatusParams, cmd *cobra.Command, args []string) {
			if err := runStatus(); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
		},
	}.ToCobra()
}

func runSplit() error {
	// Check if tclaude already has its own credentials
	tcPath := usageapi.TclaudeCredentialsPath()
	if tcPath == "" {
		return fmt.Errorf("cannot determine tclaude credentials path")
	}
	if _, err := os.Stat(tcPath); err == nil {
		return fmt.Errorf("tclaude credentials already exist at %s\nRemove the file first if you want to re-split", tcPath)
	}

	// Read from Claude's stores
	data, store, err := usageapi.ReadClaudeCredentials()
	if err != nil {
		return fmt.Errorf("cannot read Claude credentials: %w", err)
	}
	fmt.Printf("Found credentials in: %s\n", store)

	// Write to tclaude's file
	if err := usageapi.WriteTclaudeCredentials(data); err != nil {
		return fmt.Errorf("failed to write tclaude credentials: %w", err)
	}
	fmt.Printf("Wrote credentials to: %s\n", tcPath)

	// Delete from Claude's store
	if err := usageapi.DeleteClaudeCredentials(store); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to remove credentials from %s: %v\n", store, err)
		fmt.Println("tclaude credentials were written successfully, but Claude's credentials remain.")
		fmt.Println("You may want to remove them manually so Claude Code creates a fresh token on next login.")
		return nil
	}
	fmt.Printf("Removed credentials from: %s\n", store)

	fmt.Println()
	fmt.Println("Done! Credentials split successfully.")
	fmt.Println("- tclaude will now use its own token from", tcPath)
	fmt.Println("- Claude Code will prompt you to log in on next start (creating a separate token)")
	fmt.Println("- Token refreshes from tclaude will no longer interfere with Claude Code")
	return nil
}

func runStatus() error {
	tcPath := usageapi.TclaudeCredentialsPath()
	if tcPath == "" {
		return fmt.Errorf("cannot determine tclaude credentials path")
	}

	ttl := usageapi.CacheTTL()
	if _, err := os.Stat(tcPath); err == nil {
		fmt.Printf("Using tclaude's own credentials: %s\n", tcPath)
		fmt.Println("Token refresh on 429: enabled (safe, independent from Claude Code)")
		fmt.Printf("Usage refresh interval: %s\n", ttl)
	} else {
		fmt.Println("Using Claude Code's credentials (shared)")
		fmt.Println("Token refresh on 429: disabled (would conflict with Claude Code)")
		fmt.Printf("Usage refresh interval: %s\n", ttl)
		fmt.Printf("\nRun 'tclaude credentials split' to create separate credentials.\n")
	}
	return nil
}
