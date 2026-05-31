package workflowcli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/agent"
)

// TestRunNode_ForbiddenMapsRCAuth pins the workflow-local error refinement: the
// /v1 node-PATCH returns a "forbidden" code when the caller isn't the assignee
// (or group owner), and that must surface as rcAuth — not the generic
// rcIOFailure the shared agent.MapDaemonErrorToRC would give it.
func TestRunNode_ForbiddenMapsRCAuth(t *testing.T) {
	daemonStub(t, nil, func(_, _ string, _ any) (any, error) {
		return nil, &agent.DaemonError{Status: 403, Code: "forbidden", Msg: "not the assignee of node impl"}
	})
	var out, errBuf bytes.Buffer
	if rc := runNode(&nodeParams{Instance: "4", Node: "impl", Action: "done"}, &out, &errBuf); rc != rcAuth {
		t.Fatalf("forbidden node settle rc=%d, want rcAuth(%d)", rc, rcAuth)
	}
}

// captureBody installs a daemon stub that records the last request body and
// returns the given response value. The recorded *body is the exact Go value
// the wrapper passed to agent.DaemonRequest, so tests can assert the wire shape.
func captureBody(t *testing.T, lastPath *string, body *any, resp any) {
	t.Helper()
	daemonStub(t, lastPath, func(_, _ string, in any) (any, error) {
		if body != nil {
			*body = in
		}
		return resp, nil
	})
}

func asMap(t *testing.T, v any) map[string]any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	m := map[string]any{}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("body is not a JSON object: %v (%s)", err, string(b))
	}
	return m
}

func TestRunNode_StartShape(t *testing.T) {
	var path string
	var body any
	captureBody(t, &path, &body, nodePatchResp{NodeID: "impl", Status: "running", InstanceStatus: "running"})
	var out, errBuf bytes.Buffer
	if rc := runNode(&nodeParams{Instance: "4", Node: "impl", Action: "start"}, &out, &errBuf); rc != rcOK {
		t.Fatalf("runNode start rc=%d stderr=%s", rc, errBuf.String())
	}
	if path != "PATCH /v1/workflows/4/nodes/impl" {
		t.Errorf("start hit %q", path)
	}
	m := asMap(t, body)
	if m["status"] != "running" {
		t.Errorf("start status = %v, want running", m["status"])
	}
	if _, ok := m["outcome"]; ok {
		t.Error("start must not carry an outcome")
	}
}

func TestRunNode_DoneWithOutcomeAndAdvance(t *testing.T) {
	var body any
	captureBody(t, nil, &body, nodePatchResp{
		NodeID: "review", Status: "done", InstanceStatus: "running",
		Ready: []string{"deploy"}, Skipped: []string{"rework"},
	})
	var out, errBuf bytes.Buffer
	if rc := runNode(&nodeParams{Instance: "4", Node: "review", Action: "done", Outcome: "approved"}, &out, &errBuf); rc != rcOK {
		t.Fatalf("runNode done rc=%d stderr=%s", rc, errBuf.String())
	}
	m := asMap(t, body)
	if m["status"] != "done" || m["outcome"] != "approved" {
		t.Errorf("done body = %v, want status=done outcome=approved", m)
	}
	s := out.String()
	if !bytes.Contains([]byte(s), []byte("readied: deploy")) || !bytes.Contains([]byte(s), []byte("skipped: rework")) {
		t.Errorf("done output should report advance results\n%s", s)
	}
}

func TestRunNode_DoneWithoutOutcomeOmitsKey(t *testing.T) {
	var body any
	captureBody(t, nil, &body, nodePatchResp{NodeID: "plan", Status: "done", InstanceStatus: "running"})
	var out, errBuf bytes.Buffer
	if rc := runNode(&nodeParams{Instance: "4", Node: "plan", Action: "done"}, &out, &errBuf); rc != rcOK {
		t.Fatalf("rc=%d", rc)
	}
	m := asMap(t, body)
	if _, ok := m["outcome"]; ok {
		t.Error("done without --outcome must omit the outcome key so the server can default it to pass")
	}
}

func TestRunNode_FailShape(t *testing.T) {
	var body any
	captureBody(t, nil, &body, nodePatchResp{NodeID: "test", Status: "failed", InstanceStatus: "failed"})
	var out, errBuf bytes.Buffer
	if rc := runNode(&nodeParams{Instance: "4", Node: "test", Action: "fail", Output: "boom"}, &out, &errBuf); rc != rcOK {
		t.Fatalf("rc=%d", rc)
	}
	m := asMap(t, body)
	if m["status"] != "failed" || m["output"] != "boom" {
		t.Errorf("fail body = %v, want status=failed output=boom", m)
	}
}

func TestRunNode_OutcomeOnlyValidForDone(t *testing.T) {
	called := false
	daemonStub(t, nil, func(_, _ string, _ any) (any, error) { called = true; return nil, nil })
	var out, errBuf bytes.Buffer
	if rc := runNode(&nodeParams{Instance: "4", Node: "impl", Action: "start", Outcome: "pass"}, &out, &errBuf); rc != rcInvalidArg {
		t.Fatalf("start --outcome rc=%d, want %d", rc, rcInvalidArg)
	}
	if called {
		t.Error("--outcome with a non-done action must be rejected before any daemon call")
	}
}

func TestRunNode_UnknownActionAndBadID(t *testing.T) {
	daemonStub(t, nil, func(_, _ string, _ any) (any, error) {
		t.Error("daemon must not be called on a client-side validation failure")
		return nil, nil
	})
	var out, errBuf bytes.Buffer
	if rc := runNode(&nodeParams{Instance: "4", Node: "impl", Action: "frobnicate"}, &out, &errBuf); rc != rcInvalidArg {
		t.Errorf("unknown action rc=%d, want %d", rc, rcInvalidArg)
	}
	if rc := runNode(&nodeParams{Instance: "x", Node: "impl", Action: "start"}, &out, &errBuf); rc != rcInvalidArg {
		t.Errorf("bad id rc=%d, want %d", rc, rcInvalidArg)
	}
}

func TestRunSpawn_ContextShape(t *testing.T) {
	var path string
	var body any
	captureBody(t, &path, &body, spawnNodeResp{NodeID: "work", Status: "running", ConvID: "c-1234", AttachCmd: "tclaude session attach spwn-x"})
	var out, errBuf bytes.Buffer
	if rc := runSpawn(&spawnNodeParams{Instance: "4", Node: "work", Context: "upstream said: ship it"}, &out, &errBuf); rc != rcOK {
		t.Fatalf("runSpawn rc=%d stderr=%s", rc, errBuf.String())
	}
	if path != "POST /v1/workflows/4/nodes/work/start" {
		t.Errorf("spawn hit %q", path)
	}
	m := asMap(t, body)
	if m["context"] != "upstream said: ship it" {
		t.Errorf("spawn body = %v, want context seed", m)
	}
	if !bytes.Contains(out.Bytes(), []byte("spawned c-1234 into node work")) {
		t.Errorf("spawn output should report the spawned conv + node\n%s", out.String())
	}
	if !bytes.Contains(out.Bytes(), []byte("attach: tclaude session attach spwn-x")) {
		t.Errorf("spawn output should surface the attach cmd\n%s", out.String())
	}
}

func TestRunSpawn_NoContextOmitsBody(t *testing.T) {
	var body any = "sentinel"
	captureBody(t, nil, &body, spawnNodeResp{NodeID: "work", Status: "running", ConvID: "c-9"})
	var out, errBuf bytes.Buffer
	if rc := runSpawn(&spawnNodeParams{Instance: "4", Node: "work"}, &out, &errBuf); rc != rcOK {
		t.Fatalf("runSpawn rc=%d stderr=%s", rc, errBuf.String())
	}
	// No --context → no body at all (matches the dashboard start: seeds nothing).
	if body != nil {
		t.Errorf("spawn without --context must send a nil body, got %#v", body)
	}
}

func TestRunSpawn_ContextAndFileMutuallyExclusive(t *testing.T) {
	called := false
	daemonStub(t, nil, func(_, _ string, _ any) (any, error) { called = true; return nil, nil })
	var out, errBuf bytes.Buffer
	if rc := runSpawn(&spawnNodeParams{Instance: "4", Node: "work", Context: "a", ContextFile: "b.txt"}, &out, &errBuf); rc != rcInvalidArg {
		t.Fatalf("both context flags rc=%d, want %d", rc, rcInvalidArg)
	}
	if called {
		t.Error("--context + --context-file together must be rejected before any daemon call")
	}
}

func TestRunSpawn_ContextFile(t *testing.T) {
	dir := t.TempDir()
	file := dir + "/seed.txt"
	if err := os.WriteFile(file, []byte("  multi\nline\nseed  \n"), 0o644); err != nil {
		t.Fatalf("write seed file: %v", err)
	}
	var body any
	captureBody(t, nil, &body, spawnNodeResp{NodeID: "work", Status: "running", ConvID: "c-7"})
	var out, errBuf bytes.Buffer
	if rc := runSpawn(&spawnNodeParams{Instance: "4", Node: "work", ContextFile: file}, &out, &errBuf); rc != rcOK {
		t.Fatalf("runSpawn rc=%d stderr=%s", rc, errBuf.String())
	}
	m := asMap(t, body)
	if m["context"] != "multi\nline\nseed" {
		t.Errorf("context-file seed = %q, want trimmed file contents", m["context"])
	}
}

func TestRunDrive_Shape(t *testing.T) {
	var path string
	captureBody(t, &path, nil, driveResp{
		OK: true, Instance: 7, DriverConv: "drv-1234", Group: "squad",
		AttachCmd: "tclaude session attach spwn-y", Warning: "already has 1 live agent-owner(s)",
	})
	var out, errBuf bytes.Buffer
	if rc := runDrive(&driveParams{Instance: "7"}, &out, &errBuf); rc != rcOK {
		t.Fatalf("runDrive rc=%d stderr=%s", rc, errBuf.String())
	}
	if path != "POST /v1/workflows/7/drive" {
		t.Errorf("drive hit %q", path)
	}
	s := out.String()
	if !bytes.Contains([]byte(s), []byte("anchored driver drv-1234 for instance 7")) {
		t.Errorf("drive output should report the anchored driver\n%s", s)
	}
	if !bytes.Contains([]byte(s), []byte("warning: already has 1 live agent-owner(s)")) {
		t.Errorf("drive output should surface the existing-driver warning\n%s", s)
	}
}

func TestRunNew_Shape(t *testing.T) {
	var path string
	var body any
	captureBody(t, &path, &body, map[string]any{"id": 12, "group_id": 3})
	var out, errBuf bytes.Buffer
	rc := runNew(&newParams{
		Ref:   "user:ship",
		Param: []string{"service_name=billing", "env=prod"},
		Title: "ship billing",
		Group: "tclaude-dev",
	}, &out, &errBuf)
	if rc != rcOK {
		t.Fatalf("runNew rc=%d stderr=%s", rc, errBuf.String())
	}
	if path != "POST /v1/workflows" {
		t.Errorf("new hit %q", path)
	}
	m := asMap(t, body)
	if m["template_ref"] != "user:ship" || m["title"] != "ship billing" || m["group"] != "tclaude-dev" {
		t.Errorf("new body = %v", m)
	}
	params, _ := m["params"].(map[string]any)
	if params["service_name"] != "billing" || params["env"] != "prod" {
		t.Errorf("new params = %v", params)
	}
	if !bytes.Contains(out.Bytes(), []byte("created instance #12")) {
		t.Errorf("new should print the new instance id\n%s", out.String())
	}
}

func TestRunNew_BadParam(t *testing.T) {
	called := false
	daemonStub(t, nil, func(_, _ string, _ any) (any, error) { called = true; return nil, nil })
	var out, errBuf bytes.Buffer
	if rc := runNew(&newParams{Ref: "user:ship", Param: []string{"oops"}}, &out, &errBuf); rc != rcInvalidArg {
		t.Fatalf("bad param rc=%d, want %d", rc, rcInvalidArg)
	}
	if called {
		t.Error("a malformed --param must be rejected before the daemon call")
	}
}

func TestRunCancel(t *testing.T) {
	var path string
	captureBody(t, &path, nil, map[string]any{"ok": true, "instance_status": "cancelled"})
	var out, errBuf bytes.Buffer
	if rc := runCancel(&cancelParams{Instance: "8"}, &out, &errBuf); rc != rcOK {
		t.Fatalf("runCancel rc=%d", rc)
	}
	if path != "POST /v1/workflows/8/cancel" {
		t.Errorf("cancel hit %q", path)
	}
	if !bytes.Contains(out.Bytes(), []byte("cancelled instance #8")) {
		t.Errorf("cancel output:\n%s", out.String())
	}
}

func TestRunRm(t *testing.T) {
	var path string
	daemonStub(t, &path, func(method, _ string, _ any) (any, error) {
		if method != http.MethodDelete {
			t.Errorf("rm method = %s, want DELETE", method)
		}
		return nil, nil
	})
	var out, errBuf bytes.Buffer
	if rc := runRm(&rmParams{Instance: "8"}, &out, &errBuf); rc != rcOK {
		t.Fatalf("runRm rc=%d", rc)
	}
	if path != "DELETE /v1/workflows/8" {
		t.Errorf("rm hit %q", path)
	}
	if !bytes.Contains(out.Bytes(), []byte("removed instance #8")) {
		t.Errorf("rm output:\n%s", out.String())
	}
}
