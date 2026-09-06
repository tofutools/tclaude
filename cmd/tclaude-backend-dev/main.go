// Command tclaude-backend-dev runs the intentional replacement backend against
// an explicitly initialized development directory. It does not mount legacy commands.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/internal/backend/ports"
	"github.com/tofutools/tclaude/internal/backend/providers"
	"github.com/tofutools/tclaude/internal/backend/providers/claude"
	"github.com/tofutools/tclaude/internal/backend/providers/opencode"
	"github.com/tofutools/tclaude/internal/backend/server"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := command().ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func command() *cobra.Command {
	// This root intentionally has no legacy parameter enrichment or ambient state.
	cmd := boa.CmdT[struct{}]{Use: "tclaude-backend-dev", Short: "Run the isolated replacement backend"}.ToCobra()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	var state string
	var initialize bool
	var harnesses []string
	cmd.Flags().StringVar(&state, "state-dir", "", "Absolute private development state directory (required)")
	cmd.Flags().BoolVar(&initialize, "init", false, "Initialize a new directory and exit")
	cmd.Flags().StringSliceVar(&harnesses, "harness", nil, "Providers to register: claude,opencode; omit for offline catalog only")
	_ = cmd.MarkFlagRequired("state-dir")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if len(args) != 0 {
			return fmt.Errorf("unexpected positional arguments")
		}
		if initialize {
			return server.Initialize(state)
		}
		var entries []ports.Provider
		seen := map[string]bool{}
		for _, name := range harnesses {
			if seen[name] {
				return fmt.Errorf("duplicate harness %q", name)
			}
			seen[name] = true
			switch name {
			case "claude":
				p, err := claude.New(claude.Config{PrivateRoot: filepath.Join(state, "claude"), AgentSocket: filepath.Join(state, "api.sock")})
				if err != nil {
					return err
				}
				entries = append(entries, p)
			case "opencode":
				p, err := opencode.New(opencode.Config{PrivateRoot: filepath.Join(state, "opencode"), AgentSocket: filepath.Join(state, "api.sock"), Environment: os.Environ()})
				if err != nil {
					return err
				}
				entries = append(entries, p)
			default:
				return fmt.Errorf("unsupported harness %q", name)
			}
		}
		return server.Serve(cmd.Context(), state, providers.NewRegistry(entries...))
	}
	return cmd
}
