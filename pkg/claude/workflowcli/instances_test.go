package workflowcli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/workflow"
)

// daemonStub routes the CLI's daemon calls through a test handler instead of a
// real Unix socket. handler receives the (method, path, body) the wrapper built
// and returns the value to JSON-round-trip into the wrapper's `out`, or an error
// (e.g. a *agent.DaemonError to exercise error→rc mapping). The last request's
// method+path are recorded in *lastPath for path-shape assertions.
func daemonStub(t *testing.T, lastPath *string, handler func(method, path string, in any) (any, error)) {
	t.Helper()
	prevAvail := agent.DaemonAvailableImpl
	prevReq := agent.DaemonRequestImpl
	agent.DaemonAvailableImpl = func() bool { return true }
	agent.DaemonRequestImpl = func(method, path string, in, out any, _ agent.DaemonOpts) error {
		if lastPath != nil {
			*lastPath = method + " " + path
		}
		v, err := handler(method, path, in)
		if err != nil {
			return err
		}
		if out != nil && v != nil {
			b, mErr := json.Marshal(v)
			if mErr != nil {
				return mErr
			}
			return json.Unmarshal(b, out)
		}
		return nil
	}
	t.Cleanup(func() {
		agent.DaemonAvailableImpl = prevAvail
		agent.DaemonRequestImpl = prevReq
	})
}

func TestRunLs_RendersInstancesAndTemplates(t *testing.T) {
	var path string
	daemonStub(t, &path, func(_, _ string, _ any) (any, error) {
		return map[string]any{"instances": []wfInstanceRow{
			{ID: 1, Title: "ship it", TemplateName: "implement-microservice", Status: "running", Total: 6, Done: 2, Running: 1, GroupName: "tclaude-dev"},
		}}, nil
	})
	var out, errBuf bytes.Buffer
	if rc := runLs(&lsParams{}, &out, &errBuf); rc != rcOK {
		t.Fatalf("runLs rc=%d, stderr=%s", rc, errBuf.String())
	}
	if path != "GET /v1/workflows" {
		t.Errorf("ls hit %q, want GET /v1/workflows", path)
	}
	s := out.String()
	// "example" is the templates table's untruncated SOURCE cell — asserting it
	// confirms the templates section rendered without depending on the
	// terminal-width-sensitive truncation of the longer REF/NAME columns (the
	// exact refs are covered by TestRunLs_JSON).
	for _, want := range []string{"INSTANCES", "ship it", "running", "2/6", "TEMPLATES", "example"} {
		if !strings.Contains(s, want) {
			t.Errorf("ls output missing %q\n---\n%s", want, s)
		}
	}
}

func TestRunLs_JSON(t *testing.T) {
	daemonStub(t, nil, func(_, _ string, _ any) (any, error) {
		return map[string]any{"instances": []wfInstanceRow{{ID: 7, Title: "x", Status: "completed"}}}, nil
	})
	var out, errBuf bytes.Buffer
	if rc := runLs(&lsParams{JSON: true}, &out, &errBuf); rc != rcOK {
		t.Fatalf("runLs --json rc=%d", rc)
	}
	var got struct {
		Instances []wfInstanceRow      `json:"instances"`
		Templates []workflow.ListEntry `json:"templates"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("ls --json invalid: %v\n%s", err, out.String())
	}
	if len(got.Instances) != 1 || got.Instances[0].ID != 7 {
		t.Errorf("instances = %+v", got.Instances)
	}
	if len(got.Templates) == 0 {
		t.Error("templates should include at least the embedded example")
	}
}

func TestRunStatus_Renders(t *testing.T) {
	var path string
	daemonStub(t, &path, func(_, _ string, _ any) (any, error) {
		return wfDetail{
			Instance: wfInstanceMeta{ID: 3, Title: "deploy svc", TemplateRef: "example:implement-microservice", Status: "running"},
			Mermaid:  "flowchart TD\n a-->b",
			Params:   json.RawMessage(`{"service_name":"billing"}`),
			Vars:     json.RawMessage(`{}`),
			Nodes: []wfNode{
				{NodeID: "plan", Label: "Plan the service", ExecutorKind: "ai", Agent: "architect", Status: "done", Outcome: "pass"},
				{NodeID: "impl", Label: "Implement", ExecutorKind: "ai", Status: "running", Assignee: "abcdef12-3456-7890-abcd-ef1234567890"},
			},
			Events:   []wfEvent{{ID: 1, Kind: "instance_created", At: "2026-05-30T10:00:00Z"}},
			Warnings: []string{"node x has no outgoing edge"},
		}, nil
	})
	var out, errBuf bytes.Buffer
	if rc := runStatus(&statusParams{Instance: "3"}, &out, &errBuf); rc != rcOK {
		t.Fatalf("runStatus rc=%d stderr=%s", rc, errBuf.String())
	}
	if path != "GET /v1/workflows/3" {
		t.Errorf("status hit %q, want GET /v1/workflows/3", path)
	}
	s := out.String()
	for _, want := range []string{"#3", "deploy svc", "example:implement-microservice", "Plan the service", "running", "billing", "⚠ warnings", "abcdef12"} {
		if !strings.Contains(s, want) {
			t.Errorf("status output missing %q\n---\n%s", want, s)
		}
	}
	if strings.Contains(s, "abcdef12-3456-7890-abcd-ef1234567890") {
		t.Error("a UUID conv-id assignee should be shortened to its 8-char prefix in the table")
	}
}

func TestRunStatus_BadIDNeverHitsDaemon(t *testing.T) {
	called := false
	daemonStub(t, nil, func(_, _ string, _ any) (any, error) {
		called = true
		return nil, nil
	})
	var out, errBuf bytes.Buffer
	if rc := runStatus(&statusParams{Instance: "not-an-int"}, &out, &errBuf); rc != rcInvalidArg {
		t.Fatalf("runStatus bad id rc=%d, want %d", rc, rcInvalidArg)
	}
	if called {
		t.Error("a non-integer instance id must be rejected client-side, before any daemon call")
	}
}

func TestRunStatus_NotFoundMapsRC(t *testing.T) {
	daemonStub(t, nil, func(_, _ string, _ any) (any, error) {
		return nil, &agent.DaemonError{Status: 404, Code: "not_found", Msg: "workflow 9 not found"}
	})
	var out, errBuf bytes.Buffer
	if rc := runStatus(&statusParams{Instance: "9"}, &out, &errBuf); rc != rcNotFound {
		t.Fatalf("runStatus not-found rc=%d, want %d", rc, rcNotFound)
	}
}

func TestRunEvents_NodeFilterPath(t *testing.T) {
	var path string
	daemonStub(t, &path, func(_, _ string, _ any) (any, error) {
		return map[string]any{"events": []wfEvent{{ID: 1, NodeID: "plan", Kind: "node_done", At: "2026-05-30T10:00:00Z"}}}, nil
	})
	var out, errBuf bytes.Buffer
	if rc := runEvents(&eventsParams{Instance: "5", Node: "plan"}, &out, &errBuf); rc != rcOK {
		t.Fatalf("runEvents rc=%d stderr=%s", rc, errBuf.String())
	}
	if path != "GET /v1/workflows/5/events?node=plan" {
		t.Errorf("events hit %q, want the per-node path", path)
	}
	if !strings.Contains(out.String(), "node_done") {
		t.Errorf("events output missing the event kind\n%s", out.String())
	}
}

func TestRunEvents_NoNodeNoQuery(t *testing.T) {
	var path string
	daemonStub(t, &path, func(_, _ string, _ any) (any, error) {
		return map[string]any{"events": []wfEvent{}}, nil
	})
	var out, errBuf bytes.Buffer
	if rc := runEvents(&eventsParams{Instance: "5"}, &out, &errBuf); rc != rcOK {
		t.Fatalf("runEvents rc=%d", rc)
	}
	if path != "GET /v1/workflows/5/events" {
		t.Errorf("events hit %q, want the no-query path", path)
	}
	if !strings.Contains(out.String(), "(no events)") {
		t.Errorf("empty events should print a placeholder\n%s", out.String())
	}
}

func whereResp() wfWhere {
	return wfWhere{
		Caller: "abcdef1234567890",
		Assignments: []wfAssignment{
			{ // live: running instance + running node
				Instance: wfInstanceMeta{ID: 1, Title: "live wf", TemplateRef: "user:foo", Status: "running"},
				Node:     wfNode{NodeID: "impl", Label: "Implement", Status: "running", AllowedOutcomes: []string{"pass", "fail"}},
			},
			{ // settled: done node in a completed instance
				Instance: wfInstanceMeta{ID: 2, Title: "old wf", TemplateRef: "user:bar", Status: "completed"},
				Node:     wfNode{NodeID: "plan", Label: "Plan", Status: "done", Outcome: "pass"},
			},
		},
	}
}

func TestRunWhere_DefaultLiveOnly(t *testing.T) {
	var path string
	daemonStub(t, &path, func(_, _ string, _ any) (any, error) { return whereResp(), nil })
	var out, errBuf bytes.Buffer
	if rc := runWhere(&whereParams{}, &out, &errBuf); rc != rcOK {
		t.Fatalf("runWhere rc=%d stderr=%s", rc, errBuf.String())
	}
	if path != "GET /v1/workflows/where" {
		t.Errorf("where hit %q", path)
	}
	s := out.String()
	if !strings.Contains(s, "live wf") || !strings.Contains(s, "Implement") {
		t.Errorf("where should show the live assignment\n%s", s)
	}
	if strings.Contains(s, "old wf") {
		t.Error("where default should hide settled/completed assignments")
	}
}

func TestRunWhere_AllAndInstanceFilter(t *testing.T) {
	var path string
	daemonStub(t, &path, func(_, _ string, _ any) (any, error) { return whereResp(), nil })
	var out, errBuf bytes.Buffer
	if rc := runWhere(&whereParams{All: true, Instance: "2"}, &out, &errBuf); rc != rcOK {
		t.Fatalf("runWhere --all rc=%d", rc)
	}
	if path != "GET /v1/workflows/where?instance=2" {
		t.Errorf("where --instance hit %q, want the instance-scoped path", path)
	}
	s := out.String()
	if !strings.Contains(s, "old wf") || !strings.Contains(s, "live wf") {
		t.Errorf("where --all should show every assignment\n%s", s)
	}
}

func TestRunWhere_HumanCaller(t *testing.T) {
	daemonStub(t, nil, func(_, _ string, _ any) (any, error) {
		return wfWhere{Caller: "", Assignments: []wfAssignment{}}, nil
	})
	var out, errBuf bytes.Buffer
	if rc := runWhere(&whereParams{}, &out, &errBuf); rc != rcOK {
		t.Fatalf("runWhere human rc=%d", rc)
	}
	if !strings.Contains(out.String(), "per-agent") {
		t.Errorf("human caller should get the per-agent hint\n%s", out.String())
	}
}

func TestRunWhere_JSONLiveFilter(t *testing.T) {
	daemonStub(t, nil, func(_, _ string, _ any) (any, error) { return whereResp(), nil })
	var out, errBuf bytes.Buffer
	if rc := runWhere(&whereParams{JSON: true}, &out, &errBuf); rc != rcOK {
		t.Fatalf("runWhere --json rc=%d", rc)
	}
	var got wfWhere
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("where --json invalid: %v", err)
	}
	if len(got.Assignments) != 1 || got.Assignments[0].Instance.ID != 1 {
		t.Errorf("where --json (default) should carry only the live assignment, got %+v", got.Assignments)
	}
}

// With --all and zero assignments the --json output must still be an empty array,
// never "assignments": null (the daemon emits [], but normalise the passthrough).
func TestRunWhere_AllEmptyJSONNotNull(t *testing.T) {
	daemonStub(t, nil, func(_, _ string, _ any) (any, error) {
		return wfWhere{Caller: "abc", Assignments: nil}, nil
	})
	var out, errBuf bytes.Buffer
	if rc := runWhere(&whereParams{All: true, JSON: true}, &out, &errBuf); rc != rcOK {
		t.Fatalf("runWhere --all --json rc=%d", rc)
	}
	if strings.Contains(out.String(), "null") {
		t.Errorf("where --all --json must emit [] not null for empty assignments\n%s", out.String())
	}
	var got wfWhere
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if got.Assignments == nil {
		t.Error("assignments must decode as a non-nil empty slice")
	}
}
