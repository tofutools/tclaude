package product

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers/opencode"
	"github.com/tofutools/tclaude/internal/backend/server"
)

type opencodeNativeConfig struct {
	config, data string
	environment  []string
}

func (native opencodeNativeConfig) providerConfig(state string) (opencode.Config, error) {
	if native.config == "" {
		base := os.Getenv("XDG_CONFIG_HOME")
		if base == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return opencode.Config{}, err
			}
			base = filepath.Join(home, ".config")
		}
		native.config = filepath.Join(base, "opencode")
	}
	if native.data == "" {
		base := os.Getenv("XDG_DATA_HOME")
		if base == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return opencode.Config{}, err
			}
			base = filepath.Join(home, ".local", "share")
		}
		native.data = filepath.Join(base, "opencode")
	}
	if !filepath.IsAbs(native.config) || !filepath.IsAbs(native.data) {
		return opencode.Config{}, fmt.Errorf("OpenCode native config and data directories must be absolute")
	}
	environment := model.Environment{}
	for _, name := range native.environment {
		value, exists := os.LookupEnv(name)
		if !exists {
			return opencode.Config{}, fmt.Errorf("requested OpenCode environment variable %q is unset", name)
		}
		environment[name] = value
	}
	if err := environment.Validate(); err != nil {
		return opencode.Config{}, err
	}
	planner, err := configuredHostSandbox(state)
	if err != nil {
		return opencode.Config{}, err
	}
	return opencode.Config{PrivateRoot: filepath.Join(state, "opencode"),
		NativeConfigDirectory: filepath.Clean(native.config), NativeDataDirectory: filepath.Clean(native.data),
		HostSandbox: planner, AgentSocket: server.AgentSocketPath(state), AgentSocketDirectory: server.AgentSocketDirectory(state),
		Environment: environment.Entries()}, nil
}
