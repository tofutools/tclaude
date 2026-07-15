package pathv1

import (
	"bytes"
	"reflect"
	"testing"
)

func TestExecutionCheckpointBackwardCompatibleCurrentAggregateAndABARevision(t *testing.T) {
	source := []byte(`apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: execution-head
start: work
nodes:
  work:
    type: task
    performer: {kind: agent, prompt: work}
    next: done
  done: {type: end}
`)
	legacyBytes := initializedExclusiveCheckpoint(t, source)
	legacy, err := DecodeCheckpointV7(legacyBytes)
	if err != nil {
		t.Fatal(err)
	}
	if legacy.Execution != nil || CheckpointRevision(legacy) != 0 {
		t.Fatalf("legacy execution head = %#v", legacy.Execution)
	}
	legacyBinding := CurrentCheckpointBinding(legacy)
	if legacyBinding.Generation != uint64(legacy.Initialize.EventSeq) || legacyBinding.Digest != legacy.Digest {
		t.Fatalf("legacy binding = %#v", legacyBinding)
	}
	current, err := CurrentAggregateCheckpoint(legacy)
	if err != nil || !reflect.DeepEqual(current, legacy.Initialize.Aggregate) {
		t.Fatalf("legacy current aggregate differs from genesis: %v", err)
	}

	input, err := VerifyExclusiveInput(t.Context(), legacyBytes, source)
	if err != nil {
		t.Fatal(err)
	}
	pathID := current.Authority.Genesis.OutputPathID
	observation := ExclusiveObservation{SourcePathID: pathID, Attempt: 1, Outcome: "pass"}
	command, err := PlanExclusiveRoute(t.Context(), input, observation)
	if err != nil {
		t.Fatal(err)
	}
	projection, err := ReduceExclusiveRoute(t.Context(), input, observation, command)
	if err != nil {
		t.Fatal(err)
	}
	genesis := legacy.Initialize
	first, err := advanceCheckpointV7(legacy, projection.aggregate, "running")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first.Initialize, genesis) {
		t.Fatal("mutable append rewrote immutable initialization event")
	}
	if CheckpointRevision(first) != 1 || first.Execution.PreviousDigest != legacyBinding.Digest || first.Digest == legacy.Digest {
		t.Fatalf("first execution head = %#v digest=%q", first.Execution, first.Digest)
	}
	firstBytes, err := EncodeCheckpointV7(first)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeCheckpointV7(firstBytes)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := VerifyExclusiveInput(t.Context(), firstBytes, source)
	if err != nil {
		t.Fatal(err)
	}
	if verified.binding != CurrentCheckpointBinding(decoded) {
		t.Fatalf("verified current binding = %#v want %#v", verified.binding, CurrentCheckpointBinding(decoded))
	}
	if _, err := PlanExclusiveRoute(t.Context(), verified, observation); err == nil {
		t.Fatal("planner silently read immutable genesis instead of routed current aggregate")
	}

	second, err := advanceCheckpointV7(first, projection.aggregate, "running")
	if err != nil {
		t.Fatal(err)
	}
	if CheckpointRevision(second) != 2 || second.Digest == first.Digest || second.Execution.PreviousDigest != first.Digest {
		t.Fatalf("ABA-safe second head = %#v digest=%q", second.Execution, second.Digest)
	}
	if bytes.Equal(mustCheckpointBytes(t, first), mustCheckpointBytes(t, second)) {
		t.Fatal("successive identical-state appends produced identical checkpoint bytes")
	}
}

func TestExecutionCheckpointTamperFailsClosed(t *testing.T) {
	source := []byte(`apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: execution-tamper
start: done
nodes:
  done: {type: end}
`)
	data := initializedExclusiveCheckpoint(t, source)
	checkpoint, err := DecodeCheckpointV7(data)
	if err != nil {
		t.Fatal(err)
	}
	aggregate, err := CurrentAggregateCheckpoint(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	next, err := advanceCheckpointV7(checkpoint, aggregate, "running")
	if err != nil {
		t.Fatal(err)
	}

	for name, mutate := range map[string]func(*CheckpointV7){
		"revision":        func(c *CheckpointV7) { c.Execution.Revision++ },
		"previous digest": func(c *CheckpointV7) { c.Execution.PreviousDigest = c.Digest },
		"status":          func(c *CheckpointV7) { c.Execution.Status = "paused" },
		"log checksum":    func(c *CheckpointV7) { c.Execution.LogChecksum = checkpoint.Digest },
		"current digest":  func(c *CheckpointV7) { c.Digest = checkpoint.Digest },
	} {
		t.Run(name, func(t *testing.T) {
			copy, err := DecodeCheckpointV7(mustCheckpointBytes(t, next))
			if err != nil {
				t.Fatal(err)
			}
			mutate(copy)
			if err := ValidateCheckpointV7(copy); err == nil {
				t.Fatal("tampered execution checkpoint accepted")
			}
		})
	}
}

func mustCheckpointBytes(t *testing.T, checkpoint *CheckpointV7) []byte {
	t.Helper()
	data, err := EncodeCheckpointV7(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
