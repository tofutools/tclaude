package agent

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// CrossAgentAuth is the one sentence that tells a reader who may act on
// ANOTHER agent with a manager-pattern verb. It names what the daemon enforces
// (requireCrossAgentPermission): the global agent.<verb> slug, or that slug's
// group sibling groups.members.<verb> covering every current active group
// containing the target, which group ownership contributes for owned groups.
//
// Every cross-agent verb's help is built from it, never hand-written: the
// copies had drifted, and two told readers that owning ANY group containing
// the target was enough. A test in agentd walks this command tree and checks
// each verb against CrossAgentAuthVerbs and the daemon's permission registry.
func CrossAgentAuth(verb string) string {
	return "Requires agent." + verb + ", or groups.members." + verb +
		" covering every current active group containing the target (group ownership contributes the latter for owned groups)."
}

// crossAgentAuthVerbs maps each cross-agent command (its path below
// `tclaude agent`) to the verb its permission slugs are named after. A
// command with a --target flag gets CrossAgentAuth appended to that flag's
// help by applyCrossAgentTargetHelp; the others (positional selectors) carry
// it in their Long text.
var crossAgentAuthVerbs = map[string]string{
	"clone":                   "clone",
	"codex-app-server status": "context-info",
	"compact":                 "compact",
	"context-info":            "context-info",
	"delete":                  "delete",
	"inbox ls":                "inbox-watch",
	"inbox read":              "inbox-watch",
	"inbox watch":             "inbox-watch",
	"interrupt":               "interrupt",
	"present-pr":              "pr",
	"promote":                 "promote",
	"reincarnate":             "reincarnate",
	"reinstate":               "promote",
	"remote-control":          "remote-control",
	"rename":                  "rename",
	"resume":                  "resume",
	"retire":                  "retire",
	"stop":                    "stop",
	"tags add":                "tags",
	"tags rm":                 "tags",
	"tags set":                "tags",
	"tags show":               "tags",
	"task clear":              "task",
	"task set":                "task",
	"task show":               "task",
}

// CrossAgentAuthVerbs returns a copy of the command-path → verb table, for
// the test that pins every cross-agent verb's help.
func CrossAgentAuthVerbs() map[string]string {
	out := make(map[string]string, len(crossAgentAuthVerbs))
	for k, v := range crossAgentAuthVerbs {
		out[k] = v
	}
	return out
}

// applyCrossAgentTargetHelp appends CrossAgentAuth to the --target flag of
// every registered command that has one. A registered path that does not
// exist is a programming error, so it panics at construction rather than
// silently leaving a verb's help without its auth sentence.
func applyCrossAgentTargetHelp(root *cobra.Command) {
	for path, verb := range crossAgentAuthVerbs {
		cmd, _, err := root.Find(strings.Fields(path))
		if err != nil || cmd == nil || cmd == root {
			panic(fmt.Sprintf("cross-agent help: no `agent %s` command", path))
		}
		if f := cmd.Flags().Lookup("target"); f != nil {
			f.Usage = strings.TrimSpace(f.Usage) + " " + CrossAgentAuth(verb)
		}
	}
}
