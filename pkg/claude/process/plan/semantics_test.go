package plan

import (
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

func TestResolveFailEdgeUsesFailAliases(t *testing.T) {
	next := model.Next{"fail": "failed", "pass": "done"}
	if got := ResolveFailEdge(next); got != "failed" {
		t.Fatalf("target = %q, want failed", got)
	}
	if got := ResolveFailEdge(model.Next{"error": "escalate"}); got != "escalate" {
		t.Fatalf("target = %q, want escalate", got)
	}
}

func TestResolveFailEdgeIgnoresRetryModeVocabulary(t *testing.T) {
	// retry.onFail is the retry-mode policy axis, never an edge: a template
	// using the mode vocabulary must not route failures to a node named after
	// the mode.
	next := model.Next{"pass": "done"}
	if got := ResolveFailEdge(next); got != "" {
		t.Fatalf("target = %q, want empty", got)
	}
	if got := ResolveFailEdge(model.Next{model.RetryModeFeedbackSameSession: "trap"}); got != "" {
		t.Fatalf("target = %q, want empty (mode names are not fail aliases)", got)
	}
}
