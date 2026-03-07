package git

import (
	"github.com/tofutools/tclaude/pkg/claude/syncutil"
)

// SyncConfig is an alias for syncutil.SyncConfig
type SyncConfig = syncutil.SyncConfig

// ConfigPath returns the path to the sync config file
func ConfigPath() string {
	return syncutil.ConfigPath()
}

// LoadConfig loads the sync config, returning empty config if not found
func LoadConfig() (*SyncConfig, error) {
	return syncutil.LoadConfig()
}

// SaveConfig saves the sync config
func SaveConfig(config *SyncConfig) error {
	return syncutil.SaveConfig(config)
}

// ProjectDirToPath converts a Claude project dir name back to a path
func ProjectDirToPath(projectDir string) string {
	return syncutil.ProjectDirToPath(projectDir)
}

// PathToProjectDir converts a path to Claude project dir name
func PathToProjectDir(path string) string {
	return syncutil.PathToProjectDir(path)
}
