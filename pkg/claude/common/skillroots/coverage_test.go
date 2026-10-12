package skillroots_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/skillroots"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/testutil"
)

// Every registered harness must declare which installed roots it discovers,
// or explicitly declare no skill loader (shell). New harnesses cannot silently
// inherit another harness's discovery paths.
func TestRegisteredHarnessSkillCoverage(t *testing.T) {
	t.Setenv("HOME", testutil.CanonicalTempDir(t))
	t.Setenv("CODEX_HOME", "")
	installed, err := skillroots.All()
	require.NoError(t, err)
	for _, name := range harness.Names() {
		t.Run(name, func(t *testing.T) {
			roots, err := skillroots.ForHarness(name)
			require.NoError(t, err)
			if name == "shell" {
				require.Empty(t, roots)
				return
			}
			require.NotEmpty(t, roots)
			for _, root := range roots {
				require.Contains(t, installed, root)
			}
		})
	}
	_, err = skillroots.ForHarness("unknown")
	require.Error(t, err)
}

func TestSharedSkillDiscovery(t *testing.T) {
	home := testutil.CanonicalTempDir(t)
	t.Setenv("HOME", home)
	for _, name := range []string{"gemini", "copilot", "opencode"} {
		roots, err := skillroots.ForHarness(name)
		require.NoError(t, err)
		require.Contains(t, roots, filepath.Join(home, ".agents", "skills"))
	}
}
