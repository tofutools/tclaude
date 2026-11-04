package agentd_test

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
)

// Every cross-agent verb's CLI help states its auth with agent.CrossAgentAuth,
// and the slugs that sentence names are the ones the daemon's registry pairs:
// agent.<verb> with group sibling groups.members.<verb>. Hand-written copies
// had drifted (two said owning ANY group containing the target was enough).
func TestCrossAgentHelp_EveryVerbUsesTheSharedSentence(t *testing.T) {
	root := agent.Cmd()
	verbs := agent.CrossAgentAuthVerbs()
	for path, verb := range verbs {
		t.Run(path, func(t *testing.T) {
			assert.Equal(t, "groups.members."+verb, agentd.GroupSiblingForSlug("agent."+verb),
				"the help names groups.members.%s; the daemon must pair it with agent.%s", verb, verb)

			cmd, _, err := root.Find(strings.Fields(path))
			require.NoError(t, err)
			require.NotEqual(t, root, cmd, "no `agent %s` command", path)
			want := agent.CrossAgentAuth(verb)

			f := cmd.Flags().Lookup("target")
			if f != nil {
				assert.Contains(t, f.Usage, want, "--target help")
				assert.Equal(t, 1, strings.Count(f.Usage, "Requires "), "one auth sentence in --target help: %q", f.Usage)
				assert.NotContains(t, f.Usage, "`", "pflag reads a backtick as the value placeholder")
			}
			// The Long text sits on the command or, for a sub-verb, on its
			// parent group (tags, task, inbox).
			long := cmd.Long
			if !strings.Contains(long, want) && cmd.Parent() != root {
				long = cmd.Parent().Long
			}
			if f == nil || strings.Contains(long, "ANOTHER agent") || strings.Contains(strings.ToLower(long), "auth:") {
				assert.Contains(t, long, want, "Long help")
			}
		})
	}

	// A command that offers to act on ANOTHER agent through --target, or
	// spells out an agent.* requirement there, must be in the table.
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		path := strings.TrimPrefix(c.CommandPath(), root.CommandPath()+" ")
		if f := c.Flags().Lookup("target"); f != nil && c != root {
			if _, ok := verbs[path]; !ok {
				assert.False(t, strings.Contains(f.Usage, "ANOTHER agent") || strings.Contains(f.Usage, "Requires agent."),
					"`agent %s --target` reads as a cross-agent verb; add it to crossAgentAuthVerbs: %q", path, f.Usage)
			}
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(root)
}
