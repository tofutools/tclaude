package product

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	result := server.JourneyServices{History: sources, Programs: host.ProgramProcessHost{PrivateRoot: filepath.Join(state, "programs")}}
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
		var sandbox *host.SandboxLaunchPreparer
		wrapperName := "bwrap"
		if runtime.GOOS == "darwin" {
			wrapperName = "/usr/bin/sandbox-exec"
		}
		if wrapper, lookupErr := exec.LookPath(wrapperName); lookupErr == nil {
			inspector, err := host.NewSandboxPathInspector([]string{state})
			if err != nil {
				return result, err
			}
			artifacts := filepath.Join(state, "sandbox-launches")
			if err := os.MkdirAll(artifacts, 0700); err != nil {
				return result, err
			}
			bootstrap, err := os.Executable()
			if err != nil {
				return result, err
			}
			sandbox, err = host.NewSandboxLaunchPreparer(host.SandboxLaunchConfig{Inspector: inspector, Wrapper: wrapper, Bootstrap: bootstrap, Artifacts: artifacts})
			if err != nil {
				return result, err
			}
		}
		runtime, err := host.NewShellHost(host.ShellConfig{
			HostSandbox: sandbox,
			Terminal:    host.TerminalHost{Executable: "tmux", PrivateRoot: filepath.Join(state, "shells")},
			Executable:  shell, Environment: os.Environ(),
		})
		if err != nil {
			return result, err
		}
		result.Shells = runtime
	}
	return result, nil
}
