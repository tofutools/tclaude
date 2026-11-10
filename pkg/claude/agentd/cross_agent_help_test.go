package agentd_test

import (
	"regexp"
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

			cmd, rest, err := root.Find(strings.Fields(path))
			require.NoError(t, err)
			require.NotEqual(t, root, cmd, "no `agent %s` command", path)
			require.Empty(t, rest, "no `agent %s` command (Find stopped at %q)", path, cmd.CommandPath())
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

	// A command outside the table must not hand-write a cross-agent auth
	// rule: neither a --target that offers to act on another agent or names
	// an agent.* slug, nor help that names a manager-pattern slug (one with a
	// group sibling) next to group ownership. Positional-selector verbs are
	// caught by the second check.
	slugRef := regexp.MustCompile(`\bagent\.[a-z][a-z-]*[a-z]`)
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		path := strings.TrimPrefix(c.CommandPath(), root.CommandPath()+" ")
		_, listed := verbs[path]
		if f := c.Flags().Lookup("target"); f != nil && c != root && !listed {
			u := strings.ToLower(f.Usage)
			assert.False(t, strings.Contains(u, "another agent") || slugRef.MatchString(u),
				"`agent %s --target` reads as a cross-agent verb; add it to crossAgentAuthVerbs: %q", path, f.Usage)
		}
		if c != root && !listed && !crossAgentHelpExempt[path] {
			long := strings.ToLower(c.Long)
			if strings.Contains(long, "owner") || strings.Contains(long, "ownership") || strings.Contains(long, "owning") {
				for _, slug := range slugRef.FindAllString(long, -1) {
					if strings.Contains(c.Long, agent.CrossAgentAuth(strings.TrimPrefix(slug, "agent."))) {
						continue // a parent group's Long built from the shared sentence
					}
					assert.Empty(t, agentd.GroupSiblingForSlug(slug),
						"`agent %s` hand-writes the cross-agent rule for %s; add it to crossAgentAuthVerbs", path, slug)
				}
			}
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(root)
}

// crossAgentHelpExempt lists commands whose help names a manager-pattern
// slug next to group ownership but describes a different gate.
var crossAgentHelpExempt = map[string]bool{}

// The by-id cron verbs share one auth sentence (authCronJob's two gates).
func TestCronJobHelp_StatesTheDaemonGate(t *testing.T) {
	root := agent.Cmd()
	for _, path := range []string{"cron rm", "cron enable", "cron disable", "cron run-now"} {
		cmd, rest, err := root.Find(strings.Fields(path))
		require.NoError(t, err)
		require.Empty(t, rest, "no `agent %s` command", path)
		assert.Contains(t, cmd.Long, agent.CronJobAuth(), "`agent %s` help", path)
	}
	assert.Contains(t, agent.CronJobAuth(), agentd.PermGroupsMessagesSchedule)
	assert.Equal(t, "groups.members.schedule", agentd.GroupSiblingForSlug(agentd.PermAgentSchedule))
}
