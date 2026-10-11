package agentd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/testutil"
)

func TestNodeRunExecutionFileDataAndTimeout(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("production refuses root execution")
	}
	home := testutil.CanonicalTempDir(t)
	t.Setenv("HOME", home)
	_, err := config.Update(func(cfg *config.Config, loadErr error) error {
		if loadErr != nil {
			return loadErr
		}
		cfg.Federation = &config.FederationConfig{Scripts: &config.NodeScriptsConfig{}}
		return nil
	})
	require.NoError(t, err)
	// Linux uses the detached runner. macOS must reach the direct runner itself.
	useDirectNonInteractiveRunner(t)
	path := filepath.Join(home, "script ; $(not-argv-injection).sh")
	require.NoError(t, os.WriteFile(path, []byte("printf 'hello'; printf 'warning' >&2; exit 7"), 0600))
	result := runNodeScript(context.Background(), path, strings.Repeat("a", 32), 10)
	require.Empty(t, result.Error)
	require.Equal(t, 7, result.ExitCode)
	require.Equal(t, "hello", result.Stdout)
	require.Equal(t, "warning", result.Stderr)
	require.NoError(t, os.WriteFile(path, []byte("sleep 30"), 0600))
	result = runNodeScript(context.Background(), path, strings.Repeat("b", 32), 1)
	require.Equal(t, 124, result.ExitCode, result.Error)
}
