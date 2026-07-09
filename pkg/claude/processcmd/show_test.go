package processcmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
)

func TestRenderMermaidUsesStableEscapedIDs(t *testing.T) {
	snapshot := store.Snapshot{
		State: &state.State{
			Nodes: map[string]state.NodeState{
				"a-b": {Status: state.NodeStatusReady},
				"a_b": {Status: state.NodeStatusPending},
			},
		},
	}
	tmpl := &model.Template{
		Nodes: map[string]model.Node{
			"a-b": {Next: model.Next{"ok| x --> pwned[evil": "a_b"}},
			"a_b": {},
		},
	}

	var out bytes.Buffer
	renderMermaid(&out, snapshot, tmpl)
	text := out.String()
	if strings.Contains(text, "x --> pwned[evil") {
		t.Fatalf("raw injectable mermaid label leaked:\n%s", text)
	}
	if !strings.Contains(text, "n_612d62") || !strings.Contains(text, "n_615f62") {
		t.Fatalf("expected collision-free generated ids:\n%s", text)
	}
}
