package plan

import (
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
)

func ResolvePassEdge(next model.Next, verdict string) string {
	for _, key := range []string{verdict, strings.ToLower(strings.TrimSpace(verdict)), "pass", "done", "success", model.DefaultOutcome} {
		if target := next[key]; target != "" {
			return target
		}
	}
	if len(next) == 1 {
		for _, target := range next {
			return target
		}
	}
	return ""
}

// ResolveFailEdge resolves where a finally-failed node routes to. Fail edges
// come only from next keys: retry.onFail is the retry-mode policy axis
// (feedback-same-session | fresh-attempt), never an edge target.
func ResolveFailEdge(next model.Next) string {
	for _, key := range []string{"fail", "failed", "failure", "error"} {
		if target := next[key]; target != "" {
			return target
		}
	}
	return ""
}

func DecisionEdge(next model.Next, verdict string) (string, bool) {
	target, ok := next[verdict]
	if !ok {
		target, ok = next[strings.ToLower(strings.TrimSpace(verdict))]
	}
	return target, ok
}

func TerminalRunStatus(node model.Node) state.RunStatus {
	switch strings.ToLower(strings.TrimSpace(node.Result)) {
	case "fail", "failed", "failure", "error":
		return state.RunStatusFailed
	case "cancel", "canceled", "cancelled":
		return state.RunStatusCanceled
	default:
		return state.RunStatusCompleted
	}
}

func IsPassVerdict(verdict string) bool {
	return state.IsPassOutcome(verdict)
}

func IsFailOutcome(outcome string) bool {
	return state.IsFailOutcome(outcome)
}

func SettleNodeStatus(outcome string, attempt int, retry *model.RetryPolicy) state.NodeStatus {
	return state.SettleNodeStatus(outcome, attempt, retry)
}
