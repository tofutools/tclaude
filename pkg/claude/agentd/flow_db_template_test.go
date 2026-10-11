package agentd_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testutil"
)

func TestFlowDBTemplateCopiesIndependentNodes(t *testing.T) {
	for _, name := range []string{"first-node", "second-node"} {
		t.Run(name, func(t *testing.T) {
			f := newFlow(t)
			groups, err := db.ListAgentGroups()
			require.NoError(t, err)
			require.Empty(t, groups, "a prior node must not contaminate the template")
			f.HaveGroup(name)
			groups, err = db.ListAgentGroups()
			require.NoError(t, err)
			require.Len(t, groups, 1)
			require.Equal(t, name, groups[0].Name)
			backups, err := filepath.Glob(db.DBPath() + ".pre-*")
			require.NoError(t, err)
			require.Empty(t, backups, "a node copied from the current template must not replay migrations")
		})
	}
}

// Sandboxed helpers inherit the environment but may hide the parent's /tmp.
func TestMainDBTemplateUnavailableInSandbox(t *testing.T) {
	if os.Getenv("TCLAUDE_TEST_TEMPLATE_HIDDEN_CHILD") == "1" {
		return // Getting here proves TestMain did not panic before the helper ran.
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestMainDBTemplateUnavailableInSandbox$")
	cmd.Env = append(os.Environ(), "TCLAUDE_TEST_TEMPLATE_HIDDEN_CHILD=1",
		"TCLAUDE_TEST_DB_TEMPLATE="+filepath.Join(testutil.CanonicalTempDir(t), "hidden", "template.sqlite"))
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", output)
}
