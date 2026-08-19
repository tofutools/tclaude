package cli

import (
	"bytes"
	"errors"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
)

// failingRoot builds a root carrying one ordinary subcommand and one that has
// declined its usage block, both failing the same way.
func failingRoot(t *testing.T, args ...string) (*cobra.Command, *bytes.Buffer) {
	t.Helper()
	fail := func(*cobra.Command, []string) error { return errors.New("the launch died") }
	root := &cobra.Command{Use: "tclaude"}
	root.AddCommand(&cobra.Command{Use: "typed", Short: "typed by a human", RunE: fail})
	root.AddCommand(clcommon.SilenceUsageOnError(
		&cobra.Command{Use: "rendered", Short: "argv tclaude writes for itself", RunE: fail}))
	root.SetArgs(args)
	stderr := &bytes.Buffer{}
	// cobra's own writers are separate from the one execute prints through;
	// pointing them at the same buffer proves nothing else reaches the operator.
	root.SetOut(stderr)
	root.SetErr(stderr)
	return root, stderr
}

func TestExecuteKeepsTheUsageBlockForAnOrdinaryCommand(t *testing.T) {
	root, stderr := failingRoot(t, "typed")

	require.Error(t, execute(stderr, root))
	assert.Contains(t, stderr.String(), "Usage:",
		"a command an operator typed is one they can be told how to type")
	assert.Contains(t, stderr.String(), "Error: the launch died")
}

func TestExecuteSilencesTheUsageBlockForARenderedCommand(t *testing.T) {
	root, stderr := failingRoot(t, "rendered")

	require.Error(t, execute(stderr, root))
	assert.NotContains(t, stderr.String(), "Usage:",
		"the operator chose none of these flags, and the usage block pushes the failure out of view")
	assert.NotContains(t, stderr.String(), "--help")
	assert.Equal(t, "Error: the launch died\n", stderr.String(),
		"the error is the whole of what this command may print")
}

func TestExecuteReportsSuccessWithoutPrinting(t *testing.T) {
	root, stderr := failingRoot(t, "rendered", "--help")

	require.NoError(t, execute(stderr, root))
	assert.NotContains(t, stderr.String(), "Error:")
}
