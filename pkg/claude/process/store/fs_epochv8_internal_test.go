//go:build linux || darwin

package store

import (
	"errors"
	"testing"
)

func TestEpochV8SourceExactAndOverBoundary(t *testing.T) {
	base := []byte(`apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: source-boundary
start: work
nodes:
  work:
    type: task
    performer: {kind: agent, prompt: bounded}
    next: {pass: done}
  done: {type: end, result: completed}
#`)
	if len(base) >= EpochV8MaxSourceBytes {
		t.Fatal("boundary fixture base unexpectedly large")
	}
	exact := append(base, make([]byte, EpochV8MaxSourceBytes-len(base))...)
	for i := len(base); i < len(exact); i++ {
		exact[i] = 'x'
	}
	classification, parsed, err := classifyEpochV8Source(exact)
	if err != nil || classification.Candidate() == nil || parsed == nil {
		t.Fatalf("exact source boundary refused: classification=%+v parsed=%v err=%v", classification, parsed != nil, err)
	}
	_, _, err = classifyEpochV8Source(append(exact, 'x'))
	if !errors.Is(err, ErrExecutionViewOverBudget) {
		t.Fatalf("over source boundary = %v", err)
	}
}
