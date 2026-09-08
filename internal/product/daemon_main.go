package product

import (
	"fmt"
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
	var agentDirsMountParent bool
	var claudeConfigDir string
	var opencodeConfigDir, opencodeDataDir string
	var opencodeEnvironment []string
	var harnesses []string
	var sources []string
	var githubSources []string
	var githubTokenFile string
	var workspaces bool
	var shell string
	cmd.PersistentFlags().StringVar(&state, "state-dir", "", "Absolute private state directory (required)")
	cmd.PersistentFlags().BoolVar(&agentDirsMountParent, "agent-dirs-mount-parent", true, "Grant each agent its generated directory parent; false grants only the named directories")
	cmd.PersistentFlags().BoolVar(&initialize, "init", false, "Initialize a new directory and exit")
	cmd.PersistentFlags().StringVar(&claudeConfigDir, "claude-config-dir", "", "Explicit persistent Claude configuration directory; retains existing login and history")
	cmd.PersistentFlags().StringVar(&opencodeConfigDir, "opencode-config-dir", "", "Native OpenCode app configuration directory; defaults to XDG_CONFIG_HOME/opencode")
	cmd.PersistentFlags().StringVar(&opencodeDataDir, "opencode-data-dir", "", "Native OpenCode app data directory for independent login copies; defaults to XDG_DATA_HOME/opencode")
	cmd.PersistentFlags().StringArrayVar(&opencodeEnvironment, "opencode-env", nil, "Explicit environment variable name to pass to OpenCode, including confined launches (repeatable)")
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
		sandboxOptions := sandboxHostOptions{mountAgentDirectoriesIndividually: !agentDirsMountParent}
		registry, err := registeredProvidersWithNativeConfig(state, harnesses, claudeConfigDir, opencodeNativeConfig{config: opencodeConfigDir, data: opencodeDataDir, environment: opencodeEnvironment}, sandboxOptions)
		if err != nil {
			return err
		}
		journey, err := journeyServices(state, harnesses, sources, workspaces, shell, sandboxOptions)
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
	return registeredProvidersWithClaudeHome(state, harnesses, "")
}
func registeredProvidersWithClaudeHome(state string, harnesses []string, claudeHome string) (ports.ProviderRegistry, error) {
	return registeredProvidersWithNativeConfig(state, harnesses, claudeHome, opencodeNativeConfig{})
}

func registeredProvidersWithNativeConfig(state string, harnesses []string, claudeHome string, opencodeNative opencodeNativeConfig, sandboxOptions ...sandboxHostOptions) (ports.ProviderRegistry, error) {
	var entries []ports.Provider
	seen := map[string]bool{}
	for _, name := range harnesses {
		if seen[name] {
			return nil, fmt.Errorf("duplicate harness %q", name)
		}
		seen[name] = true
		switch name {
		case "claude":
			sandbox, err := configuredHostSandbox(state, sandboxOptions...)
			if err != nil {
				return nil, err
			}
			p, err := claude.New(claude.Config{NativeHome: claudeHome, HostSandbox: sandbox, AgentSocketDirectory: server.AgentSocketDirectory(state), PrivateRoot: filepath.Join(state, "claude"), AgentSocket: server.AgentSocketPath(state)})
			if err != nil {
				return nil, err
			}
			entries = append(entries, p)
		case "codex":
			sandbox, err := configuredHostSandbox(state, sandboxOptions...)
			if err != nil {
				return nil, err
			}
			p, err := codex.New(codex.Config{HostSandbox: sandbox, AgentSocketDirectory: server.AgentSocketDirectory(state), PrivateRoot: filepath.Join(state, "codex"), AgentSocket: server.AgentSocketPath(state)})
			if err != nil {
				return nil, err
			}
			entries = append(entries, p)
		case "copilot":
			sandbox, err := configuredHostSandbox(state, sandboxOptions...)
			if err != nil {
				return nil, err
			}
			p, err := copilot.New(copilot.Config{HostSandbox: sandbox, AgentSocketDirectory: server.AgentSocketDirectory(state), PrivateRoot: filepath.Join(state, "copilot"), AgentSocket: server.AgentSocketPath(state)})
			if err != nil {
				return nil, err
			}
			entries = append(entries, p)
		case "opencode":
			config, err := opencodeNative.providerConfig(state, sandboxOptions...)
			if err != nil {
				return nil, err
			}
			p, err := opencode.New(config)
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
