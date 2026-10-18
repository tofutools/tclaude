package config

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestLoadShared_ServesWatchedConfigFromMemory(t *testing.T) {
	isolateConfigHome(t)
	require.NoError(t, os.MkdirAll(DataDir(), 0o700))
	require.NoError(t, os.WriteFile(ConfigPath(), []byte(`{"log_level":"debug"}`), 0o600))

	// Without a watcher every call reads the file.
	cfg, err := LoadShared()
	require.NoError(t, err)
	require.Equal(t, "debug", cfg.LogLevel)
	require.NoError(t, os.WriteFile(ConfigPath(), []byte(`{"log_level":"info"}`), 0o600))
	cfg, _ = LoadShared()
	require.Equal(t, "info", cfg.LogLevel)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	WatchShared(ctx, 20*time.Millisecond)
	first, _ := LoadShared()
	again, _ := LoadShared()
	require.Same(t, first, again, "watched reads share one parsed config")

	// An external edit is picked up by the poller.
	require.NoError(t, os.WriteFile(ConfigPath(), []byte(`{"log_level":"warn"}`), 0o600))
	require.Eventually(t, func() bool {
		cfg, _ := LoadShared()
		return cfg.LogLevel == "warn"
	}, 2*time.Second, 10*time.Millisecond)

	// An in-process save is visible immediately.
	_, err = Update(func(cfg *Config, _ error) error {
		cfg.LogLevel = "error"
		return nil
	})
	require.NoError(t, err)
	cfg, _ = LoadShared()
	require.Equal(t, "error", cfg.LogLevel)
}
