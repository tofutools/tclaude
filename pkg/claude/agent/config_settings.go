package agent

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/common"
)

// configSetting is one config key `tclaude config get|set` exposes. The list
// is deliberately short: a key belongs here when it shapes how a human's
// terminal `tclaude` launch joins (or names) a group agent, and the dashboard
// Config tab already edits it.
// Everything else stays a dashboard (or config-file) edit.
type configSetting struct {
	key   string
	descr string
	// get returns the effective value and whether it is set explicitly
	// (false = the built-in default applies).
	get func(cfg *config.Config) (value bool, explicit bool)
	// set stores an explicit value, or clears it back to the default (nil).
	set func(cfg *config.Config, value *bool)
}

var configSettings = []configSetting{
	{
		key:   "session.auto_join_group",
		descr: "a fresh terminal `tclaude` joins the active group whose default directory is the launch directory (default true)",
		get: func(cfg *config.Config) (bool, bool) {
			return cfg.AutoJoinGroupEnabled(), cfg.Session != nil && cfg.Session.AutoJoinGroup != nil
		},
		set: func(cfg *config.Config, value *bool) { sessionConfig(cfg).AutoJoinGroup = value },
	},
	{
		key:   "session.auto_join_or_create_group",
		descr: "when no group matches the launch directory, create one and join it (default false)",
		get: func(cfg *config.Config) (bool, bool) {
			return cfg.AutoJoinOrCreateGroupEnabled(), cfg.Session != nil && cfg.Session.AutoJoinOrCreateGroup != nil
		},
		set: func(cfg *config.Config, value *bool) { sessionConfig(cfg).AutoJoinOrCreateGroup = value },
	},
	{
		// The group-launch equivalent of -l: with it on, -n NAME is also the
		// joined agent's tmux session name.
		key:   "agent.spawn_label_from_name",
		descr: "a spawned agent's tmux session is named after it (so -n NAME on a group launch is also its tmux name) instead of spwn-XXXXXX (default false)",
		get: func(cfg *config.Config) (bool, bool) {
			// A plain bool in config: only true is distinguishable from absent.
			on := cfg.SpawnLabelFromNameEnabled()
			return on, on
		},
		set: func(cfg *config.Config, value *bool) {
			if cfg.Agent == nil {
				cfg.Agent = &config.AgentConfig{}
			}
			cfg.Agent.SpawnLabelFromName = value != nil && *value
		},
	},
}

func sessionConfig(cfg *config.Config) *config.SessionConfig {
	if cfg.Session == nil {
		cfg.Session = &config.SessionConfig{}
	}
	return cfg.Session
}

func lookupConfigSetting(key string) (configSetting, error) {
	key = strings.TrimSpace(key)
	for _, s := range configSettings {
		if s.key == key {
			return s, nil
		}
	}
	keys := make([]string, 0, len(configSettings))
	for _, s := range configSettings {
		keys = append(keys, s.key)
	}
	sort.Strings(keys)
	return configSetting{}, fmt.Errorf("unknown or unsupported config key %q; supported: %s (edit other settings in the dashboard Config tab)", key, strings.Join(keys, ", "))
}

type configGetParams struct {
	Key  string `pos:"true" optional:"true" help:"Config key to show (omit to list every supported key, or with --node every key set on the peer)"`
	Node string `long:"node" optional:"true" help:"Trusted peer label or instance ID: read that node's config (needs the peer's node.config.read grant)"`
	JSON bool   `long:"json" help:"With --node: output JSON"`
}

type configSetParams struct {
	Key   string `pos:"true" help:"Config key to set"`
	Value string `pos:"true" help:"true, false, or default (clear the explicit value); with --node, a JSON value or a plain string (use clear for the default)"`
	Node  string `long:"node" optional:"true" help:"Trusted peer label or instance ID: change that node's config (needs node.config.read and the key class's grant)"`
}

type configClearParams struct {
	Key  string `pos:"true" help:"Config key to clear"`
	Node string `long:"node" optional:"true" help:"Trusted peer label or instance ID: clear the key on that node (a withheld secret is removed, any other key goes back to its default)"`
}

// configExit ends a config subcommand; RunConfigCLI swaps it for tests.
var configExit = os.Exit

type configExitCode int

// RunConfigCLI runs `tclaude config <args>` in-process with the given
// writers and returns its exit code (for tests).
func RunConfigCLI(args []string, stdout, stderr io.Writer) (rc int) {
	prevExit, prevOut, prevErr := configExit, cliStdout, cliStderr
	configExit = func(code int) { panic(configExitCode(code)) }
	cliStdout, cliStderr = stdout, stderr
	defer func() {
		configExit, cliStdout, cliStderr = prevExit, prevOut, prevErr
		if r := recover(); r != nil {
			code, ok := r.(configExitCode)
			if !ok {
				panic(r)
			}
			rc = int(code)
		}
	}()
	cmd := ConfigCmd()
	cmd.SetArgs(args)
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	if err := cmd.Execute(); err != nil {
		return rcInvalidArg
	}
	return rcOK
}

const configNodeLong = " With --node, reads or changes a linked node's config through the peer view: the peer decides per key class what you may change (node.config.write, .agents, .network, .credentials), keeps some keys to itself, and never shows a secret value (it shows withheld: set or not set). Human-only."

func configGetCmd() *cobra.Command {
	return boa.CmdT[configGetParams]{
		Use:         "get",
		Short:       "Show terminal-launch settings (e.g. session.auto_join_group), or a linked node's config with --node",
		Long:        "Show the effective value of a supported config key, and whether it is set explicitly or comes from the default. With no key, lists every supported key." + configNodeLong + " Each key shows its class and whether you may change it (write or read).",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *configGetParams, _ *cobra.Command, _ []string) {
			configExit(runConfigGet(p, cliStdout, cliStderr))
		},
	}.ToCobra()
}

func configSetCmd() *cobra.Command {
	return boa.CmdT[configSetParams]{
		Use:         "set",
		Short:       "Change a terminal-launch setting (e.g. session.auto_join_group), or a linked node's config with --node",
		Long:        "Set a supported config key to true or false, or to default to clear the explicit value. The same settings are in the dashboard under Config › Agents › Terminal startup groups. Human-only: a managed agent cannot change the operator config." + configNodeLong + " The value is JSON (true, 8080, [\"a\",\"b\"], \"text\") or else a plain string; default is the string \"default\" there, so use config clear --node to go back to the default. A secret inside a map or list is replaced by setting that whole map or list; one left empty is kept.",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *configSetParams, _ *cobra.Command, _ []string) {
			configExit(runConfigSet(p, cliStdout, cliStderr))
		},
	}.ToCobra()
}

func configClearCmd() *cobra.Command {
	return boa.CmdT[configClearParams]{
		Use:         "clear",
		Short:       "Clear a config key back to its default (with --node, also remove a withheld secret)",
		Long:        "Clear a supported config key back to its default, as set KEY default does." + configNodeLong,
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *configClearParams, _ *cobra.Command, _ []string) {
			configExit(runConfigClear(p, cliStdout, cliStderr))
		},
	}.ToCobra()
}

func runConfigClear(p *configClearParams, stdout, stderr io.Writer) int {
	if p.Node != "" {
		if agentipc.ManagedAgentProcess() {
			fmt.Fprintln(stderr, "Error: config clear is human-only; a managed agent cannot change a node's config")
			return rcAuth
		}
		return runNodeConfigChange(p.Node, p.Key, nil, stdout, stderr)
	}
	return runConfigSet(&configSetParams{Key: p.Key, Value: "default"}, stdout, stderr)
}

func runConfigGet(p *configGetParams, stdout, stderr io.Writer) int {
	if agentipc.ManagedAgentProcess() {
		fmt.Fprintln(stderr, "Error: the operator config is not readable from a managed agent")
		return rcAuth
	}
	if p.Node != "" {
		return runNodeConfigGet(p.Node, p.Key, p.JSON, stdout, stderr)
	}
	if p.JSON {
		fmt.Fprintln(stderr, "Error: --json needs --node")
		return rcInvalidArg
	}
	settings := configSettings
	if strings.TrimSpace(p.Key) != "" {
		s, err := lookupConfigSetting(p.Key)
		if err != nil {
			fmt.Fprintf(stderr, "Error: %v\n", err)
			return rcInvalidArg
		}
		settings = []configSetting{s}
	}
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(stderr, "Error: load config: %v\n", err)
		return rcIOFailure
	}
	for _, s := range settings {
		value, explicit := s.get(cfg)
		source := "default"
		if explicit {
			source = "set"
		}
		if len(settings) == 1 {
			fmt.Fprintf(stdout, "%s = %t (%s)\n", s.key, value, source)
			continue
		}
		fmt.Fprintf(stdout, "%s = %t (%s): %s\n", s.key, value, source, s.descr)
	}
	return rcOK
}

func runConfigSet(p *configSetParams, stdout, stderr io.Writer) int {
	if agentipc.ManagedAgentProcess() {
		fmt.Fprintln(stderr, "Error: config set is human-only; a managed agent cannot change the operator config")
		return rcAuth
	}
	if p.Node != "" {
		return runNodeConfigChange(p.Node, p.Key, &p.Value, stdout, stderr)
	}
	s, err := lookupConfigSetting(p.Key)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcInvalidArg
	}
	var value *bool
	raw := strings.ToLower(strings.TrimSpace(p.Value))
	if raw != "default" {
		parsed, parseErr := strconv.ParseBool(raw)
		if parseErr != nil {
			fmt.Fprintf(stderr, "Error: %s takes true, false, or default; got %q\n", s.key, p.Value)
			return rcInvalidArg
		}
		value = &parsed
	}
	cfg, err := config.Update(func(cfg *config.Config, loadErr error) error {
		if loadErr != nil {
			// Saving over a file we could not read would reset every other
			// setting to its default.
			return fmt.Errorf("load config: %w", loadErr)
		}
		s.set(cfg, value)
		return nil
	})
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcIOFailure
	}
	effective, explicit := s.get(cfg)
	source := "default"
	if explicit {
		source = "set"
	}
	fmt.Fprintf(stdout, "%s = %t (%s)\n", s.key, effective, source)
	return rcOK
}
