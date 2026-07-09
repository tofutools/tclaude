package agent

import (
	"bytes"
	"encoding/json"
	"net/http"
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

// JOH-344 #4: a from-group create warns that the snapshot's agents come through
// with blank briefs, using the daemon's blank_briefs count.
func TestRunTemplatesFromGroup_WarnsOnBlankBriefs(t *testing.T) {
	var calls []capturedReq
	stubDaemon(t, &calls, ok(`{"name":"src-tmpl","agents":[
		{"name":"lead","permissions":[]},{"name":"helper","permissions":[]}],
		"blank_briefs":2}`))

	var stdout, stderr bytes.Buffer
	rc := runTemplatesFromGroup(&templatesFromGroupParams{
		Group: "src", TemplateName: "src-tmpl",
	}, &stdout, &stderr)
	require.Equal(t, rcOK, rc, "stderr=%q", stderr.String())
	out := stdout.String()
	assert.Contains(t, out, "⚠ 2 agent brief(s) are blank")
	assert.Contains(t, out, "edit the template (initial_message) before deploying")
	assert.Contains(t, out, "templates edit src-tmpl")
}

// A from-group that produced no blank briefs (an update that kept every brief)
// prints no warning — zero-noise.
func TestRunTemplatesFromGroup_NoWarnWhenFullyBriefed(t *testing.T) {
	var calls []capturedReq
	stubDaemon(t, &calls, ok(`{"name":"src-tmpl","agents":[
		{"name":"lead","permissions":[]}],
		"updated":true,"briefs_kept":["lead"],"added":[],"removed":[],"blank_briefs":0}`))

	var stdout, stderr bytes.Buffer
	rc := runTemplatesFromGroup(&templatesFromGroupParams{
		Group: "src", TemplateName: "src-tmpl", Update: true,
	}, &stdout, &stderr)
	require.Equal(t, rcOK, rc, "stderr=%q", stderr.String())
	assert.NotContains(t, stdout.String(), "brief(s) are blank")
}

// JOH-344 #1: printStagedSpawnAndRhythms surfaces the staged-spawn + rhythm
// summary the "N spawned" headline hides, and stays silent for a simple
// single-wave template with no rhythms.
func TestPrintStagedSpawnAndRhythms(t *testing.T) {
	t.Run("prints daemon note verbatim + rhythms", func(t *testing.T) {
		var out bytes.Buffer
		printStagedSpawnAndRhythms(&out, instantiateResponse{
			PendingWaves:     1,
			WavesTotal:       2,
			PendingAgents:    4,
			ChoreographyNote: "wave 1/2 spawned; 4 more agent(s) in 1 more wave(s) will spawn",
			RhythmsCreated:   1,
		})
		s := out.String()
		assert.Contains(t, s, "staged spawn: wave 1/2 spawned; 4 more agent(s) in 1 more wave(s) will spawn")
		assert.Contains(t, s, "rhythms: 1 recurring nudge armed")
	})

	t.Run("falls back to a composed wave line when the note is absent", func(t *testing.T) {
		var out bytes.Buffer
		printStagedSpawnAndRhythms(&out, instantiateResponse{
			PendingWaves:  2,
			WavesTotal:    3,
			PendingAgents: 5,
		})
		assert.Contains(t, out.String(), "staged spawn: wave 1/3 up — 5 more agent(s) will spawn as this wave settles")
	})

	t.Run("pluralizes rhythms", func(t *testing.T) {
		var out bytes.Buffer
		printStagedSpawnAndRhythms(&out, instantiateResponse{RhythmsCreated: 2})
		assert.Contains(t, out.String(), "rhythms: 2 recurring nudges armed")
	})

	t.Run("silent for a simple single-wave template", func(t *testing.T) {
		var out bytes.Buffer
		printStagedSpawnAndRhythms(&out, instantiateResponse{Spawned: 2})
		assert.Empty(t, out.String())
	})
}

// JOH-344 #1: the deploy CLI decodes the daemon's staged-spawn fields (same
// JSON tags) and prints them, so deploying a wave-using starter no longer reads
// as "1 spawned" only.
func TestRunTaskForceDeploy_PrintsStagedSpawn(t *testing.T) {
	var calls []capturedReq
	stubDaemon(t, &calls, ok(`{"group":"squad","template":"dev-squad",
		"agents":[{"name":"lead","final_name":"squad-lead","conv_id":"c1"}],
		"spawned":1,"failed":0,"pattern_delivered":0,"pattern_errors":[],
		"deployed":true,"mission":"ship it",
		"waves_total":2,"pending_waves":1,"pending_agents":4,"rhythms_created":1,
		"choreography_note":"wave 1/2 spawned; 4 more agent(s) in 1 more wave(s) will spawn as each wave settles"}`))

	var stdout, stderr bytes.Buffer
	rc := runTaskForceDeploy(&taskForceDeployParams{
		Name: "dev-squad", Mission: "ship it",
	}, strings.NewReader(""), &stdout, &stderr)
	require.Equal(t, rcOK, rc, "stderr=%q", stderr.String())
	out := stdout.String()
	assert.Contains(t, out, "1 spawned, 0 failed")
	assert.Contains(t, out, "staged spawn: wave 1/2 spawned; 4 more agent(s)")
	assert.Contains(t, out, "rhythms: 1 recurring nudge armed")
}

// stubDaemonGetRaw makes the daemon look "available" and serves body for
// every DaemonGetRaw call, recording the requested path. t.Cleanup restores
// the production indirection vars.
func stubDaemonGetRaw(t *testing.T, gotPath *string, body string) {
	t.Helper()
	prevAvail, prevRaw := DaemonAvailableImpl, DaemonGetRawImpl
	t.Cleanup(func() { DaemonAvailableImpl, DaemonGetRawImpl = prevAvail, prevRaw })
	DaemonAvailableImpl = func() bool { return true }
	DaemonGetRawImpl = func(path string) ([]byte, http.Header, error) {
		*gotPath = path
		return []byte(body), nil, nil
	}
}

// exportEnvelopeWire is a daemon-side export envelope carrying everything the
// CLI must not strip: the embedded `profiles` array (referenced registry
// profiles travel with the export) and an unknown future envelope field.
const exportEnvelopeWire = `{"format":"tclaude-task-force","format_version":3,` +
	`"template":{"name":"crew","agents":[{"name":"lead","permissions":[]}]},` +
	`"roles":[{"name":"dev"}],` +
	`"profiles":[{"name":"lead-kit","is_owner":true,"permission_overrides":{"groups.spawn":"grant"}}],` +
	`"some_future_envelope_field":"must-survive"}`

// runTemplatesExport must pass the daemon's export bytes through VERBATIM
// (re-indented only): decoding into a CLI-side mirror struct silently strips
// every envelope field the mirror doesn't carry — the embedded `profiles`
// array was lost exactly that way, breaking profile portability of a
// CLI-produced export.
func TestRunTemplatesExport_PassesDaemonEnvelopeThroughVerbatim(t *testing.T) {
	var gotPath string
	stubDaemonGetRaw(t, &gotPath, exportEnvelopeWire)

	var stdout, stderr bytes.Buffer
	rc := runTemplatesExport(&templatesExportParams{Name: "crew"}, &stdout, &stderr)
	require.Equal(t, rcOK, rc, "stderr=%q", stderr.String())
	assert.Equal(t, "/v1/templates/crew/export", gotPath)

	var back map[string]any
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &back))
	profiles, isArr := back["profiles"].([]any)
	require.True(t, isArr, "embedded profiles survive the CLI export")
	require.Len(t, profiles, 1)
	assert.Equal(t, "lead-kit", profiles[0].(map[string]any)["name"])
	assert.Equal(t, "must-survive", back["some_future_envelope_field"],
		"unknown daemon-side envelope fields pass through untouched")
	assert.Len(t, back["roles"], 1, "roles still travel too")
}

// The --file branch writes the same verbatim bytes and reports the agent
// count from a minimal, throwaway decode of the envelope.
func TestRunTemplatesExport_FileBranchKeepsEnvelopeAndCountsAgents(t *testing.T) {
	var gotPath string
	stubDaemonGetRaw(t, &gotPath, exportEnvelopeWire)

	file := filepath.Join(t.TempDir(), "crew.task-force.json")
	var stdout, stderr bytes.Buffer
	rc := runTemplatesExport(&templatesExportParams{Name: "crew", File: file}, &stdout, &stderr)
	require.Equal(t, rcOK, rc, "stderr=%q", stderr.String())

	raw, err := os.ReadFile(file)
	require.NoError(t, err)
	var back map[string]any
	require.NoError(t, json.Unmarshal(raw, &back))
	_, isArr := back["profiles"].([]any)
	assert.True(t, isArr, "embedded profiles land in the file")
	assert.Contains(t, stderr.String(), "(1 agent)", "agent count decoded for the confirmation line")
}

// The owner column and show tags must reflect EFFECTIVE ownership (the
// daemon's derived effective_is_owner, which folds in profile-granted owner
// bits), not just the legacy per-agent flag — a roster whose ownership comes
// from a spawn profile is not "ownerless". Absent field (older daemon) falls
// back to the legacy flag.
func TestTemplates_OwnerDisplayUsesEffectiveOwnership(t *testing.T) {
	var calls []capturedReq
	stubDaemon(t, &calls, ok(`[
		{"name":"board","agents":[
			{"name":"chair","permissions":[],"spawn_profile":"boss-kit","effective_is_owner":true},
			{"name":"guest","permissions":[]}]}]`))

	var stdout, stderr bytes.Buffer
	rc := runTemplatesLs(&stdout, &stderr)
	require.Equal(t, rcOK, rc, "stderr=%q", stderr.String())
	assert.Contains(t, stdout.String(), "chair", "profile-granted owner listed in the owner column")

	// show: the tag distinguishes a profile-granted owner from the legacy flag
	// (flipping is_owner would not clear it).
	stubDaemon(t, &calls, ok(`
		{"name":"board","agents":[
			{"name":"chair","permissions":[],"spawn_profile":"boss-kit","effective_is_owner":true},
			{"name":"boss","permissions":[],"is_owner":true,"effective_is_owner":true},
			{"name":"guest","permissions":[]}]}`))
	stdout.Reset()
	rc = runTemplatesShow(&templatesShowParams{Name: "board"}, &stdout, &stderr)
	require.Equal(t, rcOK, rc, "stderr=%q", stderr.String())
	out := stdout.String()
	assert.Contains(t, out, "owner(via profile)", "profile-granted owner tagged with its source")
	assert.Contains(t, out, "[owner]", "legacy-flag owner keeps the plain tag")
}
