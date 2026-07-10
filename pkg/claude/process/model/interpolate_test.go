package model

import "testing"

func TestInterpolatePerformerRuntimeFields(t *testing.T) {
	original := Performer{
		Kind: PerformerProgram, Profile: "{{ params.issue }}", Prompt: "Implement {{params.issue}}",
		Ask: "Approve {{ params.issue }}?", Run: "tools/{{ params.tool }}",
		Args: []string{"--issue={{ params.issue }}", "{{ params.missing }}"}, Timeout: "{{ params.timeout }}",
	}
	got := InterpolatePerformer(original, map[string]string{"issue": "TCL-278", "tool": "check"})
	if got.Prompt != "Implement TCL-278" || got.Ask != "Approve TCL-278?" || got.Run != "tools/check" {
		t.Fatalf("interpolated performer = %#v", got)
	}
	if len(got.Args) != 2 || got.Args[0] != "--issue=TCL-278" || got.Args[1] != "" {
		t.Fatalf("interpolated args = %#v", got.Args)
	}
	if got.Profile != original.Profile || got.Timeout != original.Timeout {
		t.Fatalf("inert configuration fields changed: %#v", got)
	}
	if original.Args[0] != "--issue={{ params.issue }}" {
		t.Fatalf("input args mutated: %#v", original.Args)
	}
}
