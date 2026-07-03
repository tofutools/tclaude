package agent

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// capturedReq records one DaemonRequest the CLI would have sent.
type capturedReq struct {
	method string
	path   string
	body   any
	opts   DaemonOpts
}

// stubDaemon makes the daemon look "available" and routes every
// DaemonRequest through respond, which returns (httpStatus,
// responseJSON). Each call is appended to *calls. A status >= 400
// surfaces as a *DaemonError carrying code; otherwise responseJSON is
// unmarshalled into the caller's out value. t.Cleanup restores the
// production indirection vars.
func stubDaemon(t *testing.T, calls *[]capturedReq, respond func(method, path string) (int, string, string)) {
	t.Helper()
	prevAvail, prevReq := DaemonAvailableImpl, DaemonRequestImpl
	t.Cleanup(func() { DaemonAvailableImpl, DaemonRequestImpl = prevAvail, prevReq })
	DaemonAvailableImpl = func() bool { return true }
	DaemonRequestImpl = func(method, path string, in, out any, opts DaemonOpts) error {
		*calls = append(*calls, capturedReq{method: method, path: path, body: in, opts: opts})
		status, code, respJSON := respond(method, path)
		if status >= 400 {
			return &DaemonError{Status: status, Code: code, Msg: "stub error"}
		}
		if out != nil && respJSON != "" {
			return json.Unmarshal([]byte(respJSON), out)
		}
		return nil
	}
}

// ok is the common respond closure for a 200 with a fixed body.
func ok(body string) func(string, string) (int, string, string) {
	return func(string, string) (int, string, string) { return 200, "", body }
}

func TestRunTemplatesLs_FormatsTable(t *testing.T) {
	var calls []capturedReq
	stubDaemon(t, &calls, ok(`[
		{"name":"feature-team","descr":"a PO and two devs","agents":[
			{"name":"PO","is_owner":true,"permissions":["groups.spawn"]},
			{"name":"dev1","permissions":[]},
			{"name":"dev2","permissions":[]}]},
		{"name":"solo","agents":[{"name":"worker","permissions":[]}]}]`))

	var stdout, stderr bytes.Buffer
	rc := runTemplatesLs(&stdout, &stderr)
	require.Equal(t, rcOK, rc, "stderr=%q", stderr.String())

	out := stdout.String()
	assert.Contains(t, out, "feature-team")
	assert.Contains(t, out, "a PO and two devs")
	assert.Contains(t, out, "PO", "owner column lists the owner agent")
	assert.Contains(t, out, "solo")
	require.Len(t, calls, 1)
	assert.Equal(t, "GET", calls[0].method)
	assert.Equal(t, "/v1/templates", calls[0].path)
}

func TestRunTemplatesLs_Empty(t *testing.T) {
	var calls []capturedReq
	stubDaemon(t, &calls, ok(`[]`))
	var stdout, stderr bytes.Buffer
	rc := runTemplatesLs(&stdout, &stderr)
	require.Equal(t, rcOK, rc)
	assert.Contains(t, stdout.String(), "no group templates")
}

func TestRunTemplatesShow_HumanView(t *testing.T) {
	var calls []capturedReq
	stubDaemon(t, &calls, ok(`{"name":"feature-team","descr":"a team",
		"default_context":"shared boilerplate",
		"agents":[{"name":"PO","role":"product-owner","descr":"leads",
			"initial_message":"Coordinate the team.","is_owner":true,
			"permissions":["groups.spawn"]}]}`))

	var stdout, stderr bytes.Buffer
	rc := runTemplatesShow(&templatesShowParams{Name: "feature-team"}, &stdout, &stderr)
	require.Equal(t, rcOK, rc, "stderr=%q", stderr.String())

	out := stdout.String()
	assert.Contains(t, out, "Template: feature-team")
	assert.Contains(t, out, "shared boilerplate", "default context is shown")
	assert.Contains(t, out, "PO")
	assert.Contains(t, out, "owner", "owner tag is shown")
	assert.Contains(t, out, "role=product-owner")
	assert.Contains(t, out, "groups.spawn", "permission slugs are shown")
	assert.Contains(t, out, "Coordinate the team.", "the per-agent brief is shown")
	require.Len(t, calls, 1)
	assert.Equal(t, "/v1/templates/feature-team", calls[0].path)
}

func TestRunTemplatesShow_JSONRoundTrips(t *testing.T) {
	var calls []capturedReq
	stubDaemon(t, &calls, ok(`{"name":"t","agents":[{"name":"a","permissions":[]}]}`))

	var stdout, stderr bytes.Buffer
	rc := runTemplatesShow(&templatesShowParams{Name: "t", JSON: true}, &stdout, &stderr)
	require.Equal(t, rcOK, rc, "stderr=%q", stderr.String())

	// --json output must parse straight back into the wire shape.
	var got templateJSON
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &got), "stdout=%q", stdout.String())
	assert.Equal(t, "t", got.Name)
	require.Len(t, got.Agents, 1)
	assert.Equal(t, "a", got.Agents[0].Name)
}

func TestRunTemplatesShow_EmptyNameRejected(t *testing.T) {
	var stdout, stderr bytes.Buffer
	rc := runTemplatesShow(&templatesShowParams{Name: "  "}, &stdout, &stderr)
	assert.Equal(t, rcInvalidArg, rc)
	assert.Contains(t, stderr.String(), "name is required")
}

func TestRunTemplatesShow_NotFoundMapsToRC(t *testing.T) {
	var calls []capturedReq
	stubDaemon(t, &calls, func(string, string) (int, string, string) {
		return 404, "not_found", ""
	})
	var stdout, stderr bytes.Buffer
	rc := runTemplatesShow(&templatesShowParams{Name: "ghost"}, &stdout, &stderr)
	assert.Equal(t, rcNotFound, rc, "a 404 not_found maps to rcNotFound")
}

// validateGroupName (which template names reuse) permits spaces and
// other URL-significant characters, so a name must be percent-escaped
// before it goes into the request path — otherwise a space breaks the
// HTTP request-line and a '?' silently truncates the target.
func TestRunTemplatesShow_EscapesNameInPath(t *testing.T) {
	var calls []capturedReq
	stubDaemon(t, &calls, ok(`{"name":"my template","agents":[]}`))
	var stdout, stderr bytes.Buffer
	rc := runTemplatesShow(&templatesShowParams{Name: "my template"}, &stdout, &stderr)
	require.Equal(t, rcOK, rc, "stderr=%q", stderr.String())
	require.Len(t, calls, 1)
	assert.Equal(t, "/v1/templates/my%20template", calls[0].path,
		"a name with a space must be percent-escaped in the URL path")
}

func TestRunTemplatesInstantiate_EscapesNameInPath(t *testing.T) {
	var calls []capturedReq
	stubDaemon(t, &calls, ok(`{"group":"g","template":"odd name","spawned":0,"failed":0,"agents":[]}`))
	var stdout, stderr bytes.Buffer
	rc := runTemplatesInstantiate(&templatesInstantiateParams{
		Name: "odd name", Group: "g",
	}, nil, &stdout, &stderr)
	require.Equal(t, rcOK, rc, "stderr=%q", stderr.String())
	require.Len(t, calls, 1)
	assert.Equal(t, "/v1/templates/odd%20name/instantiate", calls[0].path,
		"the name segment is escaped, the literal /instantiate suffix is not")
}

func TestRunTemplatesCreate_SendsParsedTemplate(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "tmpl.json")
	require.NoError(t, os.WriteFile(file, []byte(
		`{"name":"feature-team","descr":"d","agents":[
			{"name":"PO","is_owner":true,"permissions":["groups.spawn"]},
			{"name":"dev1","permissions":[]}]}`), 0o644))

	var calls []capturedReq
	stubDaemon(t, &calls, ok(`{"id":7,"name":"feature-team"}`))

	var stdout, stderr bytes.Buffer
	rc := runTemplatesCreate(&templatesCreateParams{File: file}, nil, &stdout, &stderr)
	require.Equal(t, rcOK, rc, "stderr=%q", stderr.String())

	require.Len(t, calls, 1)
	assert.Equal(t, "POST", calls[0].method)
	assert.Equal(t, "/v1/templates", calls[0].path)
	tmpl, ok := calls[0].body.(*templateJSON)
	require.True(t, ok, "body should be a *templateJSON, got %T", calls[0].body)
	assert.Equal(t, "feature-team", tmpl.Name)
	require.Len(t, tmpl.Agents, 2)
	assert.True(t, tmpl.Agents[0].IsOwner, "PO owner flag survives the round-trip")
	assert.Contains(t, stdout.String(), "Created template")
	assert.Contains(t, stdout.String(), "2 agents")
}

func TestRunTemplatesCreate_StdinFile(t *testing.T) {
	var calls []capturedReq
	stubDaemon(t, &calls, ok(`{"id":1,"name":"t"}`))
	stdin := strings.NewReader(`{"name":"t","agents":[]}`)

	var stdout, stderr bytes.Buffer
	rc := runTemplatesCreate(&templatesCreateParams{File: "-"}, stdin, &stdout, &stderr)
	require.Equal(t, rcOK, rc, "stderr=%q", stderr.String())
	require.Len(t, calls, 1)
}

func TestRunTemplatesCreate_MissingFileRejected(t *testing.T) {
	var stdout, stderr bytes.Buffer
	rc := runTemplatesCreate(&templatesCreateParams{File: ""}, nil, &stdout, &stderr)
	assert.Equal(t, rcInvalidArg, rc)
	assert.Contains(t, stderr.String(), "--file is required")
}

func TestRunTemplatesCreate_MalformedJSONRejected(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "bad.json")
	require.NoError(t, os.WriteFile(file, []byte(`{not valid json`), 0o644))

	var stdout, stderr bytes.Buffer
	rc := runTemplatesCreate(&templatesCreateParams{File: file}, nil, &stdout, &stderr)
	assert.Equal(t, rcInvalidArg, rc)
	assert.Contains(t, stderr.String(), "not valid template JSON")
}

func TestRunTemplatesEdit_PatchAndRenameNotice(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "t.json")
	require.NoError(t, os.WriteFile(file, []byte(`{"name":"renamed","agents":[]}`), 0o644))

	var calls []capturedReq
	stubDaemon(t, &calls, ok(`{"id":3,"name":"renamed"}`))

	var stdout, stderr bytes.Buffer
	rc := runTemplatesEdit(&templatesEditParams{Name: "original", File: file}, nil, &stdout, &stderr)
	require.Equal(t, rcOK, rc, "stderr=%q", stderr.String())

	require.Len(t, calls, 1)
	assert.Equal(t, "PATCH", calls[0].method)
	assert.Equal(t, "/v1/templates/original", calls[0].path)
	assert.Contains(t, stdout.String(), "renamed", "rename is surfaced when the body name differs")
}

func TestRunTemplatesRm_DeletePath(t *testing.T) {
	var calls []capturedReq
	stubDaemon(t, &calls, func(string, string) (int, string, string) { return 204, "", "" })

	var stdout, stderr bytes.Buffer
	rc := runTemplatesRm(&templatesRmParams{Name: "feature-team"}, &stdout, &stderr)
	require.Equal(t, rcOK, rc, "stderr=%q", stderr.String())
	require.Len(t, calls, 1)
	assert.Equal(t, "DELETE", calls[0].method)
	assert.Equal(t, "/v1/templates/feature-team", calls[0].path)
	assert.Contains(t, stdout.String(), "Deleted template")
}

func TestRunTemplatesInstantiate_HappyPath(t *testing.T) {
	var calls []capturedReq
	stubDaemon(t, &calls, ok(`{"group":"phoenix","template":"feature-team","spawned":2,"failed":0,
		"agents":[
			{"name":"PO","final_name":"phoenix-PO","conv_id":"abcd1234-0000-0000-0000-000000000000","owner":true,"granted":["groups.spawn"]},
			{"name":"dev1","final_name":"phoenix-dev1","conv_id":"efef5678-0000-0000-0000-000000000000"}]}`))

	var stdout, stderr bytes.Buffer
	rc := runTemplatesInstantiate(&templatesInstantiateParams{
		Name: "feature-team", Group: "phoenix", Task: "Build the login flow.",
	}, nil, &stdout, &stderr)
	require.Equal(t, rcOK, rc, "stderr=%q", stderr.String())

	out := stdout.String()
	assert.Contains(t, out, "2 spawned, 0 failed")
	assert.Contains(t, out, "phoenix-PO")
	assert.Contains(t, out, "owner")
	assert.Contains(t, out, "groups.spawn")

	require.Len(t, calls, 1)
	assert.Equal(t, "/v1/templates/feature-team/instantiate", calls[0].path)
	body, ok := calls[0].body.(map[string]any)
	require.True(t, ok, "body should be a map, got %T", calls[0].body)
	assert.Equal(t, "phoenix", body["group_name"])
	assert.Equal(t, "Build the login flow.", body["task"])
	assert.Positive(t, calls[0].opts.Timeout,
		"instantiate must extend the request timeout past the 10s default")
}

func TestRunTemplatesInstantiate_PartialFailureIsNonZeroExit(t *testing.T) {
	var calls []capturedReq
	stubDaemon(t, &calls, ok(`{"group":"phoenix","template":"t","spawned":1,"failed":1,
		"agents":[
			{"name":"PO","final_name":"phoenix-PO","conv_id":"abcd1234-0000-0000-0000-000000000000"},
			{"name":"dev1","final_name":"phoenix-dev1","error":"spawn timed out"}]}`))

	var stdout, stderr bytes.Buffer
	rc := runTemplatesInstantiate(&templatesInstantiateParams{
		Name: "t", Group: "phoenix",
	}, nil, &stdout, &stderr)
	assert.Equal(t, rcIOFailure, rc, "a partial spawn failure must be a non-zero exit")
	assert.Contains(t, stdout.String(), "spawn timed out", "the failed agent's error is shown")
	assert.Contains(t, stderr.String(), "failed to spawn")
}

func TestRunTemplatesInstantiate_MissingGroupRejected(t *testing.T) {
	var stdout, stderr bytes.Buffer
	rc := runTemplatesInstantiate(&templatesInstantiateParams{Name: "t", Group: ""}, nil, &stdout, &stderr)
	assert.Equal(t, rcInvalidArg, rc)
	assert.Contains(t, stderr.String(), "--group is required")
}

func TestRunTemplatesInstantiate_TaskAndTaskFileMutuallyExclusive(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "task.txt")
	require.NoError(t, os.WriteFile(file, []byte("a task"), 0o644))

	var stdout, stderr bytes.Buffer
	rc := runTemplatesInstantiate(&templatesInstantiateParams{
		Name: "t", Group: "g", Task: "inline", TaskFile: file,
	}, nil, &stdout, &stderr)
	assert.Equal(t, rcInvalidArg, rc)
	assert.Contains(t, stderr.String(), "not both")
}

func TestRunTemplatesFromGroup_SendsBody(t *testing.T) {
	var calls []capturedReq
	stubDaemon(t, &calls, ok(`{"name":"src-tmpl","agents":[
		{"name":"lead","permissions":[]},{"name":"helper","permissions":[]}]}`))

	var stdout, stderr bytes.Buffer
	rc := runTemplatesFromGroup(&templatesFromGroupParams{
		Group: "src", TemplateName: "src-tmpl",
	}, &stdout, &stderr)
	require.Equal(t, rcOK, rc, "stderr=%q", stderr.String())

	require.Len(t, calls, 1)
	assert.Equal(t, "POST", calls[0].method)
	assert.Equal(t, "/v1/templates/from-group", calls[0].path)
	body, ok := calls[0].body.(map[string]any)
	require.True(t, ok, "body should be a map, got %T", calls[0].body)
	assert.Equal(t, "src", body["group"])
	assert.Equal(t, "src-tmpl", body["template_name"])
	assert.Equal(t, false, body["update"], "plain from-group sends update:false")
	assert.Contains(t, stdout.String(), "2 agents")
}

func TestRunTemplatesFromGroup_UpdateSendsFlagAndReportsDiff(t *testing.T) {
	var calls []capturedReq
	stubDaemon(t, &calls, ok(`{"name":"src-tmpl","agents":[
		{"name":"lead","permissions":[]},{"name":"navigator","permissions":[]}],
		"updated":true,"briefs_kept":["lead"],"added":["navigator"],"removed":["dev1"]}`))

	var stdout, stderr bytes.Buffer
	rc := runTemplatesFromGroup(&templatesFromGroupParams{
		Group: "src", TemplateName: "src-tmpl", Update: true,
	}, &stdout, &stderr)
	require.Equal(t, rcOK, rc, "stderr=%q", stderr.String())

	require.Len(t, calls, 1)
	body, ok := calls[0].body.(map[string]any)
	require.True(t, ok, "body should be a map, got %T", calls[0].body)
	assert.Equal(t, true, body["update"], "--update lands in the request body")
	out := stdout.String()
	assert.Contains(t, out, `Updated template "src-tmpl" from group "src" — 2 agents`)
	assert.Contains(t, out, "briefs kept: lead; added: navigator; removed: dev1")
	assert.NotContains(t, out, "Created template", "update output replaces the create line")
}

func TestRunTemplatesFromGroup_MissingArgsRejected(t *testing.T) {
	var stdout, stderr bytes.Buffer
	rc := runTemplatesFromGroup(&templatesFromGroupParams{Group: "src", TemplateName: ""}, &stdout, &stderr)
	assert.Equal(t, rcInvalidArg, rc)
}
