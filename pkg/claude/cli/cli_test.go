package cli

import (
	"bytes"
	"errors"
	"strings"
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
	root.AddCommand(&cobra.Command{
		Use:  "succeeds",
		RunE: func(*cobra.Command, []string) error { return nil },
	})
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
	printed := stderr.String()
	assert.Contains(t, printed, "Usage:",
		"a command an operator typed is one they can be told how to type")
	assert.Contains(t, printed, "Error: the launch died")
	assert.Less(t, strings.Index(printed, "Usage:"), strings.Index(printed, "Error:"),
		"boa's convention puts the usage block first, and panes and scripts read the tail")
}

// The rationale for an annotation over cobra's SilenceUsage is that the field is
// true tree-wide under boa, so reading it would silence the invocations usage
// exists for. These are those invocations: cobra attributes them to a command
// that never opted out, which is what has to keep them printing.
func TestExecuteKeepsTheUsageBlockForAMisspelledInvocation(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"unknown command", []string{"nosuchcmd"}},
		{"unknown flag", []string{"typed", "--nosuchflag"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, stderr := failingRoot(t, tc.args...)

			require.Error(t, execute(stderr, root))
			assert.Contains(t, stderr.String(), "Usage:",
				"an operator who mistyped the invocation is exactly who usage is for")
		})
	}
}

// A rendered command's own flags are silenced too, and deliberately. cobra
// attributes a parse error to the command whose flags failed to parse, so an
// unknown flag here reaches the same branch a RunE failure does — and it should:
// tclaude wrote that argv, so a flag it got wrong is a tclaude bug, not a typo an
// operator can be shown how to correct. What they can act on is the error naming
// the flag; the usage block would only push it out of view, which is the whole
// reason this command declined it. `--help` is unaffected, as TestExecuteLeaves-
// HelpToCobra pins.
func TestExecuteSilencesTheUsageBlockForARenderedCommandsOwnFlags(t *testing.T) {
	root, stderr := failingRoot(t, "rendered", "--nosuchflag")

	require.Error(t, execute(stderr, root))
	assert.NotContains(t, stderr.String(), "Usage:")
	assert.Contains(t, stderr.String(), "Error: unknown flag: --nosuchflag",
		"the flag that was not understood is the actionable half, and all of it")
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
	root, stderr := failingRoot(t, "succeeds")

	require.NoError(t, execute(stderr, root))
	assert.Empty(t, stderr.String(), "a command that worked has nothing to report")
}

func TestExecuteLeavesHelpToCobra(t *testing.T) {
	root, stderr := failingRoot(t, "rendered", "--help")

	require.NoError(t, execute(stderr, root))
	assert.Contains(t, stderr.String(), "argv tclaude writes for itself",
		"declining a usage block on failure does not decline the help someone asked for")
	assert.NotContains(t, stderr.String(), "Error:")
}
