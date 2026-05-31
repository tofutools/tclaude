package agentd

import (
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/workflow"
)

// nodeTaskText must pick the right instruction field per executor kind — the
// self-view's "task" is wrong for the whole node class if this picks the wrong
// field. (JOH-15 Slice A)
func TestNodeTaskText(t *testing.T) {
	cases := []struct {
		name string
		def  *workflow.Node
		want string
	}{
		{"ai → prompt", &workflow.Node{Executor: workflow.Executor{Kind: workflow.ExecAI, Prompt: "do the thing"}}, "do the thing"},
		{"human → instructions", &workflow.Node{Executor: workflow.Executor{Kind: workflow.ExecHuman, Instructions: "approve it"}}, "approve it"},
		{"tool → run", &workflow.Node{Executor: workflow.Executor{Kind: workflow.ExecTool, Run: "echo hi"}}, "echo hi"},
		{"program → run", &workflow.Node{Executor: workflow.Executor{Kind: workflow.ExecProgram, Run: "./build.sh"}}, "./build.sh"},
		{"unknown kind → empty", &workflow.Node{Executor: workflow.Executor{Kind: "weird", Prompt: "ignored"}}, ""},
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
