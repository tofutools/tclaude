package plan

import (
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

func TestResolveFailEdgeTrimsRetryOnFail(t *testing.T) {
	next := model.Next{"retry-failed": "failed"}
	got := ResolveFailEdge(next, &model.RetryPolicy{OnFail: " retry-failed "})
	if got != "failed" {
		t.Fatalf("target = %q, want failed", got)
	}
}

func TestResolveFailEdgeFallsBackWhenRetryOnFailEmpty(t *testing.T) {
	next := model.Next{"fail": "failed"}
	got := ResolveFailEdge(next, &model.RetryPolicy{})
	if got != "failed" {
		t.Fatalf("target = %q, want failed", got)
	}
}
