package agent

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSandboxProfilesCommandHelpDocumentsNonSecretEnvironment(t *testing.T) {
	cmd := sandboxProfilesCmd()
	assert.Equal(t, "sandbox-profiles", cmd.Name())
	assert.Contains(t, cmd.Long, "non-secret environment values")
	for _, name := range []string{"ls", "show", "create", "edit", "rm", "default", "group", "export", "import", "draft"} {
		child, _, err := cmd.Find([]string{name})
		require.NoError(t, err)
		assert.Equal(t, name, child.Name())
	}
}

func TestRunSandboxProfilesDraftUsesDraftOnlyHandoff(t *testing.T) {
	var calls []capturedReq
	stubDaemon(t, &calls, func(method, path string) (int, string, string) {
		assert.Equal(t, http.MethodPost, method)
		assert.Equal(t, "/v1/sandbox-profile-drafts/abcdefghijklmnop", path)
		return http.StatusAccepted, "", `{"accepted":true,"message":"draft validated"}`
	})
	input := `{"name":"dev","filesystem":[{"path":"/work","access":"read"}],"environment":[]}`
	var stdout, stderr bytes.Buffer
	rc := runSandboxProfilesDraft(&sandboxProfilesDraftParams{
		Token: "abcdefghijklmnop", File: "-",
	}, strings.NewReader(input), &stdout, &stderr)
	require.Equal(t, rcOK, rc, "stderr=%s", stderr.String())
	require.Len(t, calls, 1)
	body, ok := calls[0].body.(struct {
		Profile sandboxProfileJSON `json:"profile"`
	})
	require.True(t, ok)
	assert.Equal(t, "dev", body.Profile.Name)
	assert.Contains(t, stdout.String(), "has not been saved")
}

func TestRunSandboxProfilesListAndShowStableOutputs(t *testing.T) {
	var calls []capturedReq
	stubDaemon(t, &calls, func(method, path string) (int, string, string) {
		switch path {
		case "/v1/sandbox-profiles":
			return 200, "", `[
				{"name":"zeta","filesystem":[],"environment":[]},
				{"name":"alpha","filesystem":[{"path":"/work","access":"write"}],"environment":[{"name":"CACHE","value":"/cache"}]}
			]`
		case "/v1/sandbox-profiles/alpha":
			return 200, "", `{"name":"alpha","filesystem":[{"path":"/work","access":"write"}],"environment":[{"name":"CACHE","value":"/cache"}],"created_at":"2026-07-11T00:00:00Z"}`
		default:
			return 404, "not_found", ""
		}
	})

	var stdout, stderr bytes.Buffer
	require.Equal(t, rcOK, runSandboxProfilesLs(&sandboxProfilesLsParams{}, &stdout, &stderr), "stderr=%s", stderr.String())
	assert.Less(t, strings.Index(stdout.String(), "alpha"), strings.Index(stdout.String(), "zeta"))
	assert.Contains(t, stdout.String(), "FILESYSTEM")

	stdout.Reset()
	require.Equal(t, rcOK, runSandboxProfilesLs(&sandboxProfilesLsParams{JSON: true}, &stdout, &stderr))
	var listed []sandboxProfileJSON
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &listed))
	require.Len(t, listed, 2)
	assert.Equal(t, "alpha", listed[0].Name, "JSON list ordering is stable")

	stdout.Reset()
	require.Equal(t, rcOK, runSandboxProfilesShow(&sandboxProfilesShowParams{Name: "alpha"}, &stdout, &stderr))
	assert.Contains(t, stdout.String(), "write /work")
	assert.Contains(t, stdout.String(), "CACHE=/cache")
	assert.Contains(t, stdout.String(), "created: 2026-07-11T00:00:00Z")

	stdout.Reset()
	require.Equal(t, rcOK, runSandboxProfilesShow(&sandboxProfilesShowParams{Name: "alpha", JSON: true}, &stdout, &stderr))
	assert.JSONEq(t, `{"name":"alpha","filesystem":[{"path":"/work","access":"write"}],"environment":[{"name":"CACHE","value":"/cache"}],"created_at":"2026-07-11T00:00:00Z"}`, stdout.String())
	assert.Equal(t, http.MethodGet, calls[0].method)
}

func TestRunSandboxProfilesCRUDRoundTripsShowJSONShape(t *testing.T) {
	var calls []capturedReq
	stubDaemon(t, &calls, func(method, _ string) (int, string, string) {
		if method == http.MethodPost {
			return 201, "", `{"id":7,"name":"dev"}`
		}
		if method == http.MethodPatch {
			return 200, "", `{"id":7,"name":"renamed"}`
		}
		return 204, "", ""
	})
	input := `{"name":"dev","filesystem":[{"path":"/work","access":"read"}],"environment":[{"name":"CACHE","value":"/cache"}],"created_at":"ignored"}`
	var stdout, stderr bytes.Buffer
	require.Equal(t, rcOK, runSandboxProfilesCreate(&sandboxProfilesFileParams{File: "-"}, strings.NewReader(input), &stdout, &stderr))
	created, ok := calls[0].body.(*sandboxProfileJSON)
	require.True(t, ok)
	assert.Equal(t, "dev", created.Name)
	assert.Equal(t, "ignored", created.CreatedAt, "show --json is accepted without lossy reshaping")

	stdout.Reset()
	input = strings.Replace(input, `"name":"dev"`, `"name":"renamed"`, 1)
	require.Equal(t, rcOK, runSandboxProfilesEdit(&sandboxProfilesEditParams{Name: "dev/name", File: "-"}, strings.NewReader(input), &stdout, &stderr))
	assert.Equal(t, "/v1/sandbox-profiles/dev%2Fname", calls[1].path)
	assert.Contains(t, stdout.String(), `renamed to "renamed"`)

	stdout.Reset()
	require.Equal(t, rcOK, runSandboxProfilesRm(&sandboxProfilesRmParams{Name: "renamed"}, &stdout, &stderr))
	assert.Equal(t, http.MethodDelete, calls[2].method)
	assert.Equal(t, "/v1/sandbox-profiles/renamed", calls[2].path)
}

func TestRunSandboxProfileDefaultAndGroupAssignments(t *testing.T) {
	var calls []capturedReq
	stubDaemon(t, &calls, func(method, path string) (int, string, string) {
		if strings.Contains(path, "/groups/") {
			if method == http.MethodDelete {
				return 200, "", `{"group":"crew","name":""}`
			}
			return 200, "", `{"group":"crew","name":"dev"}`
		}
		if method == http.MethodDelete {
			return 200, "", `{"name":""}`
		}
		return 200, "", `{"name":"dev"}`
	})
	var stdout, stderr bytes.Buffer
	require.Equal(t, rcOK, runSandboxProfilesDefaultShow(&sandboxProfilesJSONParams{JSON: true}, &stdout, &stderr))
	assert.JSONEq(t, `{"name":"dev"}`, stdout.String())
	stdout.Reset()
	require.Equal(t, rcOK, runSandboxProfilesGroupShow(&sandboxProfilesGroupShowParams{Group: "crew"}, &stdout, &stderr))
	assert.Equal(t, "crew: dev\n", stdout.String())
	assert.Equal(t, "/v1/groups/crew/sandbox-profile", calls[len(calls)-1].path)
	stdout.Reset()
	require.Equal(t, rcOK, runSandboxProfilesGroupShow(&sandboxProfilesGroupShowParams{Group: "crew", JSON: true}, &stdout, &stderr))
	assert.JSONEq(t, `{"group":"crew","name":"dev"}`, stdout.String())
	stdout.Reset()
	assert.Equal(t, rcInvalidArg, runSandboxProfilesGroupShow(&sandboxProfilesGroupShowParams{}, &stdout, &stderr))
	assert.Contains(t, stderr.String(), "group name is required")
	stderr.Reset()
	stdout.Reset()
	require.Equal(t, rcOK, runSandboxProfilesDefaultSet(&sandboxProfilesNameParams{Name: " dev "}, &stdout, &stderr))
	// An ordinary assignment sends exactly the name: the break-glass
	// acknowledgement key is omitted unless the operator actually gave it.
	assert.Equal(t, map[string]any{"name": "dev"}, calls[len(calls)-1].body)
	stdout.Reset()
	require.Equal(t, rcOK, runSandboxProfilesDefaultSet(
		&sandboxProfilesNameParams{Name: "dev", IUnderstandBreakGlassRisk: true}, &stdout, &stderr))
	assert.Equal(t, map[string]any{"name": "dev", "break_glass_acknowledged": true}, calls[len(calls)-1].body,
		"--i-understand-break-glass-risk must reach the daemon's assignment gate")
	stdout.Reset()
	require.Equal(t, rcOK, runSandboxProfilesGroupSet(&sandboxProfilesGroupSetParams{Group: "crew", Name: "dev"}, &stdout, &stderr))
	assert.Equal(t, "/v1/groups/crew/sandbox-profile", calls[len(calls)-1].path)
	assert.Contains(t, stdout.String(), "crew: sandbox profile set to dev")
	stdout.Reset()
	require.Equal(t, rcOK, runSandboxProfilesGroupClear(&sandboxProfilesGroupClearParams{Group: "crew"}, &stdout, &stderr))
	assert.Equal(t, http.MethodDelete, calls[len(calls)-1].method)
}

func TestRunSandboxProfilesExportImportPreservesFutureEnvelopeFields(t *testing.T) {
	const wire = `{"format":"tclaude-sandbox-profiles","format_version":1,"profiles":[{"name":"dev","filesystem":[],"environment":[]}],"on_conflict":"overwrite","apply_assignments":true,"future_field":{"keep":true}}`
	var gotExportPath string
	stubDaemonGetRaw(t, &gotExportPath, wire)
	var calls []capturedReq
	stubDaemon(t, &calls, ok(`{"imported":["dev"],"skipped":[],"warnings":["group missing"]}`))

	var stdout, stderr bytes.Buffer
	require.Equal(t, rcOK, runSandboxProfilesExport(&sandboxProfilesExportParams{Names: []string{"dev kit"}, IncludeAssignments: true}, &stdout, &stderr))
	assert.Equal(t, "/v1/sandbox-profiles/export?include_assignments=true&name=dev+kit", gotExportPath)
	var exported map[string]any
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &exported))
	assert.Contains(t, exported, "future_field")

	stdout.Reset()
	require.Equal(t, rcOK, runSandboxProfilesImport(&sandboxProfilesImportParams{File: "-", OnConflict: "overwrite", ApplyAssignments: true, JSON: true}, strings.NewReader(wire), &stdout, &stderr))
	require.Len(t, calls, 1)
	body, ok := calls[0].body.(map[string]any)
	require.True(t, ok)
	assert.Contains(t, body, "future_field")
	assert.Equal(t, "overwrite", body["on_conflict"])
	assert.Equal(t, true, body["apply_assignments"])
	assert.JSONEq(t, `{"imported":["dev"],"skipped":[],"warnings":["group missing"]}`, stdout.String())

	// A bundle cannot opt itself into overwrite/assignment mutation; omitting
	// the flags forces the safe defaults even when the file says otherwise.
	calls = nil
	stdout.Reset()
	require.Equal(t, rcOK, runSandboxProfilesImport(&sandboxProfilesImportParams{File: "-"}, strings.NewReader(wire), &stdout, &stderr))
	body, ok = calls[0].body.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "error", body["on_conflict"])
	assert.Equal(t, false, body["apply_assignments"])
}
