package claude

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// skillCommandRE finds a `tclaude <words>` invocation in a piece of code:
// the command path plus whatever follows it, up to a shell separator.
var skillCommandRE = regexp.MustCompile(`tclaude((?: [a-z][a-z0-9-]*)+)([^|;&#]*)`)

var inlineCodeRE = regexp.MustCompile("`([^`]+)`")

// skillNonCommands are quoted outputs that start like a command but are not
// one, such as the daemon's own error message.
var skillNonCommands = []string{
	"tclaude agentd is not running",
}

var skillFlagRE = regexp.MustCompile(`(?:^|\s|\[)--([a-z][a-z0-9-]*)`)

// TestBundledSkillsNameRealCommandsAndFlags keeps the installed agent skills
// honest: every `tclaude ...` command a skill names must resolve to a real
// subcommand, and every --flag written after it on the same line must be a
// flag of that subcommand. Agents copy these lines verbatim, so an invented
// verb or flag is a bug.
func TestBundledSkillsNameRealCommandsAndFlags(t *testing.T) {
	root := Cmd()
	files, err := filepath.Glob(filepath.Join("agent", "skills", "*", "SKILL.md"))
	require.NoError(t, err)
	require.NotEmpty(t, files)

	for _, file := range files {
		name := filepath.Base(filepath.Dir(file))
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(file)
			require.NoError(t, err)
			inFence := false
			for lineNo, line := range strings.Split(string(data), "\n") {
				if strings.HasPrefix(strings.TrimSpace(line), "```") {
					inFence = !inFence
					continue
				}
				// Only code is checked: prose such as "tclaude agentd is not
				// running" names no command.
				code := []string{line}
				if !inFence {
					code = nil
					for _, m := range inlineCodeRE.FindAllStringSubmatch(line, -1) {
						code = append(code, m[1])
					}
				}
				for _, c := range code {
					if slices.ContainsFunc(skillNonCommands, func(p string) bool { return strings.Contains(c, p) }) {
						continue
					}
					for _, m := range skillCommandRE.FindAllStringSubmatch(c, -1) {
						checkSkillCommand(t, root, file, lineNo+1, strings.Fields(m[1]), m[2])
					}
				}
			}
		})
	}
}

func checkSkillCommand(t *testing.T, root *cobra.Command, file string, line int, words []string, rest string) {
	t.Helper()
	cmd := root
	consumed := 0
	for _, w := range words {
		next := findSubcommand(cmd, w)
		if next == nil {
			break
		}
		cmd = next
		consumed++
	}
	if consumed == 0 {
		return // not a tclaude subcommand at all
	}
	// A command group (has subcommands, and its Use declares no arguments of
	// its own) must be followed by a real subcommand, not an invented verb.
	if consumed < len(words) && cmd.HasSubCommands() && len(strings.Fields(cmd.Use)) == 1 {
		t.Errorf("%s:%d: `tclaude %s`: %q is not a subcommand of %q", file, line,
			strings.Join(words, " "), words[consumed], cmd.CommandPath())
		return
	}
	for _, fm := range skillFlagRE.FindAllStringSubmatch(" "+strings.Join(words[consumed:], " ")+rest, -1) {
		if cmd.Flags().Lookup(fm[1]) == nil && cmd.InheritedFlags().Lookup(fm[1]) == nil {
			t.Errorf("%s:%d: `%s` has no flag --%s", file, line, cmd.CommandPath(), fm[1])
		}
	}
}

func findSubcommand(cmd *cobra.Command, name string) *cobra.Command {
	for _, c := range cmd.Commands() {
		if c.Name() == name || c.HasAlias(name) {
			return c
		}
	}
	return nil
}
