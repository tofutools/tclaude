package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tofutools/tclaude/internal/backend/host"
	"github.com/tofutools/tclaude/internal/backend/ports"
	"github.com/tofutools/tclaude/internal/backend/server"
)

type historySources map[string]ports.HistoryDiscoveryScope

func (s historySources) HistorySource(harness, name string) (ports.HistoryDiscoveryScope, bool) {
	scope, ok := s[harness+":"+name]
	return scope, ok
}

func journeyServices(state string, harnesses, configured []string, workspaces bool, shell string) (server.JourneyServices, error) {
	sources := historySources{}
	enabled := map[string]bool{}
	for _, harness := range harnesses {
		enabled[harness] = true
		if harness != "claude" {
			sources[harness+":owned"] = ports.HistoryDiscoveryScope{}
		}
	}
	for _, value := range configured {
		key, path, ok := strings.Cut(value, "=")
		harness, name, named := strings.Cut(key, ":")
		if !ok || !named || name == "" || !enabled[harness] || !filepath.IsAbs(path) {
			return server.JourneyServices{}, fmt.Errorf("history source must be registered-harness:name=/absolute/path")
		}
		if harness != "claude" && harness != "opencode" {
			return server.JourneyServices{}, fmt.Errorf("%s supports only its provider-owned history source", harness)
		}
		if _, exists := sources[key]; exists {
			return server.JourneyServices{}, fmt.Errorf("duplicate history source %q", key)
		}
		sources[key] = ports.HistoryDiscoveryScope{Source: path}
	}
	result := server.JourneyServices{History: sources}
	if workspaces {
		checkout, err := host.NewCheckoutHost("")
		if err != nil {
			return result, err
		}
		result.Workspaces = checkout
	}
	if shell != "" {
		if !workspaces {
			return result, fmt.Errorf("shell requires --workspaces")
		}
		runtime, err := host.NewShellHost(host.ShellConfig{
			Terminal:   host.TerminalHost{Executable: "tmux", PrivateRoot: filepath.Join(state, "shells")},
			Executable: shell, Environment: os.Environ(),
		})
		if err != nil {
			return result, err
		}
		result.Shells = runtime
	}
	return result, nil
}
