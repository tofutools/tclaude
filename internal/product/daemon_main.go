package product

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/internal/backend/ports"
	"github.com/tofutools/tclaude/internal/backend/providers"
	"github.com/tofutools/tclaude/internal/backend/providers/claude"
	"github.com/tofutools/tclaude/internal/backend/providers/codex"
	"github.com/tofutools/tclaude/internal/backend/providers/copilot"
	"github.com/tofutools/tclaude/internal/backend/providers/opencode"
	"github.com/tofutools/tclaude/internal/backend/server"
)

// DaemonCommand constructs the shared replacement product command.
func DaemonCommand() *cobra.Command {
	// This root intentionally has no legacy parameter enrichment or ambient state.
	cmd := boa.CmdT[struct{}]{Use: "tclaude-agentd", Short: "Run the agentic work backend"}.ToCobra()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	var state string
	var initialize bool
	var harnesses []string
	var sources []string
	var githubSources []string
	var githubTokenFile string
	var workspaces bool
	var shell string
	cmd.PersistentFlags().StringVar(&state, "state-dir", "", "Absolute private state directory (required)")
	cmd.PersistentFlags().BoolVar(&initialize, "init", false, "Initialize a new directory and exit")
	cmd.PersistentFlags().StringSliceVar(&harnesses, "harness", nil, "Providers to register: claude,codex,opencode,copilot; omit for offline catalog only")
	cmd.PersistentFlags().StringArrayVar(&sources, "history-source", nil, "Named native history source: harness:name=/absolute/path")
	cmd.PersistentFlags().BoolVar(&workspaces, "workspaces", false, "Enable owned Git checkout operations")
	cmd.PersistentFlags().StringVar(&shell, "shell", "", "Enable standalone shells with this operator-selected executable")
	cmd.PersistentFlags().StringArrayVar(&githubSources, "github-source", nil, "Read-only exact pull request source: name=owner/repository#number (maximum four)")
	cmd.PersistentFlags().StringVar(&githubTokenFile, "github-token-file", "", "Private credential file for configured GitHub sources; omit for public unauthenticated reads")
	_ = cmd.MarkPersistentFlagRequired("state-dir")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if len(args) != 0 {
			return fmt.Errorf("unexpected positional arguments")
		}
		if initialize {
			return server.Initialize(state)
		}
		registry, err := registeredProviders(state, harnesses)
		if err != nil {
			return err
		}
		journey, err := journeyServices(state, harnesses, sources, workspaces, shell)
		if err != nil {
			return err
		}
		journey.FactSources, err = configuredGitHubSources(githubSources, githubTokenFile)
		if err != nil {
			return err
		}
		return server.Serve(cmd.Context(), state, registry, journey)
	}
	serve := boa.CmdT[struct{}]{Use: "serve", Short: "Serve the initialized agentic work backend"}.ToCobra()
	serve.Args = cobra.NoArgs
	serve.RunE = cmd.RunE
	cmd.AddCommand(serve)
	return cmd
}

func registeredProviders(state string, harnesses []string) (ports.ProviderRegistry, error) {
	var entries []ports.Provider
	seen := map[string]bool{}
	for _, name := range harnesses {
		if seen[name] {
			return nil, fmt.Errorf("duplicate harness %q", name)
		}
		seen[name] = true
		switch name {
		case "claude":
			p, err := claude.New(claude.Config{PrivateRoot: filepath.Join(state, "claude"), AgentSocket: filepath.Join(state, "api.sock")})
			if err != nil {
				return nil, err
			}
			entries = append(entries, p)
		case "codex":
			sandbox, err := configuredHostSandbox(state)
			if err != nil {
				return nil, err
			}
			p, err := codex.New(codex.Config{HostSandbox: sandbox, PrivateRoot: filepath.Join(state, "codex"), AgentSocket: filepath.Join(state, "api.sock")})
			if err != nil {
				return nil, err
			}
			entries = append(entries, p)
		case "copilot":
			sandbox, err := configuredHostSandbox(state)
			if err != nil {
				return nil, err
			}
			p, err := copilot.New(copilot.Config{HostSandbox: sandbox, PrivateRoot: filepath.Join(state, "copilot"), AgentSocket: filepath.Join(state, "api.sock")})
			if err != nil {
				return nil, err
			}
			entries = append(entries, p)
		case "opencode":
			p, err := opencode.New(opencode.Config{PrivateRoot: filepath.Join(state, "opencode"), AgentSocket: filepath.Join(state, "api.sock"), Environment: os.Environ()})
			if err != nil {
				return nil, err
			}
			entries = append(entries, p)
		default:
			return nil, fmt.Errorf("unsupported harness %q", name)
		}
	}
	return providers.NewRegistry(entries...), nil
}
