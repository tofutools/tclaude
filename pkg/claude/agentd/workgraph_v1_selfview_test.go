package agentd

import (
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/workgraph"
)

// nodeTaskText must pick the right instruction field per executor kind — the
// self-view's "task" is wrong for the whole node class if this picks the wrong
// field. (JOH-15 Slice A)
func TestNodeTaskText(t *testing.T) {
	cases := []struct {
		name string
		def  *workgraph.Node
		want string
	}{
		{"ai → prompt", &workgraph.Node{Executor: workgraph.Executor{Kind: workgraph.ExecAI, Prompt: "do the thing"}}, "do the thing"},
		{"human → instructions", &workgraph.Node{Executor: workgraph.Executor{Kind: workgraph.ExecHuman, Instructions: "approve it"}}, "approve it"},
		{"tool → run", &workgraph.Node{Executor: workgraph.Executor{Kind: workgraph.ExecTool, Run: "echo hi"}}, "echo hi"},
		{"program → run", &workgraph.Node{Executor: workgraph.Executor{Kind: workgraph.ExecProgram, Run: "./build.sh"}}, "./build.sh"},
		{"unknown kind → empty", &workgraph.Node{Executor: workgraph.Executor{Kind: "weird", Prompt: "ignored"}}, ""},
		{"nil node → empty", nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := nodeTaskText(c.def); got != c.want {
				t.Errorf("nodeTaskText = %q, want %q", got, c.want)
			}
		})
	}
}
