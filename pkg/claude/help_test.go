package claude

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
)

func walkCommands(c *cobra.Command, visit func(*cobra.Command)) {
	visit(c)
	for _, sub := range c.Commands() {
		walkCommands(sub, visit)
	}
}

// Boa appends positionals to Use, so a Use that spells them prints them twice.
func TestHelpNamesEachPositionalOnce(t *testing.T) {
	walkCommands(Cmd(), func(c *cobra.Command) {
		seen := map[string]bool{}
		fields := strings.Fields(c.Use)
		for i, field := range fields {
			if i == 0 || !strings.HasPrefix(field, "<") && !strings.HasPrefix(field, "[") || strings.HasPrefix(field, "[--") {
				continue
			}
			if strings.Contains(fields[i-1], "--") {
				continue // a flag's value, like --file <path>
			}
			name := strings.Trim(field, "<>[].")
			assert.Falsef(t, seen[name], "%s: %q repeats %s", c.CommandPath(), c.Use, name)
			seen[name] = true
		}
	})
}

// short:"-" means no shorthand; registered literally, help prints "--, --flag".
func TestHelpPrintsNoEmptyShorthand(t *testing.T) {
	walkCommands(Cmd(), func(c *cobra.Command) {
		c.Flags().VisitAll(func(f *pflag.Flag) {
			assert.NotEqualf(t, "-", f.Shorthand, "%s --%s", c.CommandPath(), f.Name)
		})
	})
}

func TestSiblingCommandsHaveTheirOwnHelp(t *testing.T) {
	walkCommands(Cmd(), func(c *cobra.Command) {
		seen := map[string]string{}
		for _, sub := range c.Commands() {
			if sub.Hidden || sub.Short == "" {
				continue
			}
			if other, dup := seen[sub.Short]; dup {
				assert.Failf(t, "shared help", "%s: %s and %s both say %q", c.CommandPath(), other, sub.Name(), sub.Short)
			}
			seen[sub.Short] = sub.Name()
		}
	})
}
