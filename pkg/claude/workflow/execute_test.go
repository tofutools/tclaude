package workflow

import (
	"context"
	"errors"
	"testing"
)

// fakeRunner records the commands it was asked to run and returns scripted
// results keyed by the (interpolated) command string.
type fakeRunner struct {
	ran     []string
	results map[string]fakeResult
}

type fakeResult struct {
	output string
	exit0  bool
	err    error
}

func (f *fakeRunner) Run(_ context.Context, command, _ string) (string, bool, error) {
	f.ran = append(f.ran, command)
	if r, ok := f.results[command]; ok {
		return r.output, r.exit0, r.err
	}
	return "", true, nil // default: silent success
}

func TestRunExecutor_ToolInterpolatesAndCaptures(t *testing.T) {
	n := &Node{Executor: Executor{Kind: ExecTool, Run: "echo {{name}}"}}
	runner := &fakeRunner{results: map[string]fakeResult{
		"echo billing": {output: "billing", exit0: true},
	}}
	res := RunExecutor(context.Background(), n, Scope{"name": "billing"}, runner)
	if res.Outcome != ExecRan || !res.Success || res.Output != "billing" {
		t.Fatalf("got %+v", res)
	}
	if len(runner.ran) != 1 || runner.ran[0] != "echo billing" {
		t.Errorf("ran = %v, want [echo billing]", runner.ran)
	}
}

func TestRunExecutor_ToolNonZeroExitIsFailureNotError(t *testing.T) {
	n := &Node{Executor: Executor{Kind: ExecTool, Run: "false"}}
	runner := &fakeRunner{results: map[string]fakeResult{
		"false": {output: "boom", exit0: false, err: errors.New("exit status 1")},
	}}
	res := RunExecutor(context.Background(), n, Scope{}, runner)
	if res.Outcome != ExecRan {
		t.Fatalf("non-zero exit should still be ExecRan, got %v", res.Outcome)
	}
	if res.Success {
		t.Error("Success should be false on non-zero exit")
	}
	if res.Output != "boom" {
		t.Errorf("output = %q", res.Output)
	}
}

func TestRunExecutor_UnresolvedRefIsError(t *testing.T) {
	n := &Node{Executor: Executor{Kind: ExecTool, Run: "deploy {{missing}}"}}
	res := RunExecutor(context.Background(), n, Scope{}, &fakeRunner{})
	if res.Outcome != ExecError || res.Err == "" {
		t.Fatalf("want ExecError, got %+v", res)
	}
}

func TestRunExecutor_AIandHumanDefer(t *testing.T) {
	for _, k := range []ExecutorKind{ExecAI, ExecHuman} {
		n := &Node{Executor: Executor{Kind: k}}
		res := RunExecutor(context.Background(), n, Scope{}, &fakeRunner{})
		if res.Outcome != ExecDefer {
			t.Errorf("kind %s: want ExecDefer, got %v", k, res.Outcome)
		}
	}
}

func TestRunExecutor_UnknownKindIsError(t *testing.T) {
	n := &Node{Executor: Executor{Kind: "weird"}}
	res := RunExecutor(context.Background(), n, Scope{}, &fakeRunner{})
	if res.Outcome != ExecError {
		t.Fatalf("want ExecError, got %v", res.Outcome)
	}
}

func TestRunVerifier_NoneRidesExecSuccess(t *testing.T) {
	n := &Node{Verify: Verify{Kind: VerifyNone}}
	ok := RunVerifier(context.Background(), n, Scope{}, "out", true, &fakeRunner{})
	if !ok.Done || ok.Outcome != OutcomePass {
		t.Errorf("exec-ok none: got %+v", ok)
	}
	bad := RunVerifier(context.Background(), n, Scope{}, "out", false, &fakeRunner{})
	if bad.Done || bad.Outcome != OutcomeFail {
		t.Errorf("exec-fail none: got %+v", bad)
	}
}

func TestRunVerifier_ToolExitDecides(t *testing.T) {
	n := &Node{Verify: Verify{Kind: VerifyTool, Run: "go test ./..."}}
	pass := RunVerifier(context.Background(), n, Scope{}, "", true, &fakeRunner{
		results: map[string]fakeResult{"go test ./...": {exit0: true}},
	})
	if !pass.Done || pass.Outcome != OutcomePass {
		t.Errorf("pass: %+v", pass)
	}
	fail := RunVerifier(context.Background(), n, Scope{}, "", true, &fakeRunner{
		results: map[string]fakeResult{"go test ./...": {exit0: false, err: errors.New("exit 1")}},
	})
	if fail.Done || fail.Outcome != OutcomeFail {
		t.Errorf("fail: %+v", fail)
	}
}

func TestRunVerifier_EnumSelectsBranch(t *testing.T) {
	n := &Node{Verify: Verify{Kind: VerifyEnum, Values: []string{"approved", "changes"}}}
	// last non-empty line is the verdict
	d := RunVerifier(context.Background(), n, Scope{}, "thinking...\napproved\n", true, &fakeRunner{})
	if !d.Done || d.Outcome != "approved" {
		t.Errorf("approved: %+v", d)
	}
	d2 := RunVerifier(context.Background(), n, Scope{}, "changes", true, &fakeRunner{})
	if !d2.Done || d2.Outcome != "changes" {
		t.Errorf("changes: %+v", d2)
	}
	// value not in the set → fail
	bad := RunVerifier(context.Background(), n, Scope{}, "maybe", true, &fakeRunner{})
	if bad.Done || bad.Outcome != OutcomeFail {
		t.Errorf("out-of-set: %+v", bad)
	}
	// empty output → fail
	empty := RunVerifier(context.Background(), n, Scope{}, "  \n  ", true, &fakeRunner{})
	if empty.Done {
		t.Errorf("empty: %+v", empty)
	}
}

func TestRunVerifier_FormatRegex(t *testing.T) {
	n := &Node{Verify: Verify{Kind: VerifyFormat, Pattern: `^v\d+\.\d+\.\d+$`}}
	ok := RunVerifier(context.Background(), n, Scope{}, "v1.2.3", true, &fakeRunner{})
	if !ok.Done {
		t.Errorf("match: %+v", ok)
	}
	no := RunVerifier(context.Background(), n, Scope{}, "nope", true, &fakeRunner{})
	if no.Done || no.Outcome != OutcomeFail {
		t.Errorf("no-match: %+v", no)
	}
}

func TestRunVerifier_EmptyFormatPatternFails(t *testing.T) {
	// An empty pattern must NOT silently pass-verify everything.
	n := &Node{Verify: Verify{Kind: VerifyFormat, Pattern: "  "}}
	d := RunVerifier(context.Background(), n, Scope{}, "anything", true, &fakeRunner{})
	if d.Done || d.Outcome != OutcomeFail {
		t.Errorf("empty format pattern should fail, got %+v", d)
	}
}

func TestRunVerifier_HumanAndAIDefer(t *testing.T) {
	for _, k := range []VerifyKind{VerifyHuman, VerifyAI} {
		n := &Node{Verify: Verify{Kind: k}}
		d := RunVerifier(context.Background(), n, Scope{}, "", true, &fakeRunner{})
		if !d.Defer {
			t.Errorf("kind %s: want Defer, got %+v", k, d)
		}
	}
}

func TestRunVerifier_UnresolvedVerifyRefFails(t *testing.T) {
	n := &Node{Verify: Verify{Kind: VerifyTool, Run: "check {{missing}}"}}
	d := RunVerifier(context.Background(), n, Scope{}, "", true, &fakeRunner{})
	if d.Done || d.Outcome != OutcomeFail {
		t.Errorf("want fail on unresolved verify ref: %+v", d)
	}
}
