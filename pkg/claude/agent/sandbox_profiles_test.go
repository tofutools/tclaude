package agent

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

func TestSandboxProfilesCommandHelpDocumentsNonSecretEnvironment(t *testing.T) {
	cmd := sandboxProfilesCmd()
	assert.Equal(t, "sandbox-profiles", cmd.Name())
	assert.Contains(t, cmd.Long, "non-secret environment values")
	for _, name := range []string{"ls", "show", "create", "edit", "rm", "default", "group", "export", "import", "draft", "plan"} {
		child, _, err := cmd.Find([]string{name})
		require.NoError(t, err)
		assert.Equal(t, name, child.Name())
	}
}

func TestRunSandboxProfilesPlanKeepsRecordedAndHypotheticalModesSeparate(t *testing.T) {
	var calls []capturedReq
	stubDaemon(t, &calls, func(method, path string) (int, string, string) {
		assert.Equal(t, http.MethodPost, method)
		assert.Equal(t, "/v1/sandbox-profile-plan", path)
		return 200, "", `{
			"source":"hypothetical","cwd":"/work",
			"target":{"implementation":"tclaude-layer","harness":"claude","platform":"linux","resolved_by":"harness default"},
			"profiles":[],"notices":[],
			"predicted_axes":{
				"network":{"tier":"closed","outcome":"enforced","detail":"isolated"},
				"unix_sockets":{"tier":"closed","outcome":"enforced","detail":"agentd only"}
			},
			"plan":{"applicable":true,"network_posture":"isolated-with-agentd","entries":[{
				"class":2,"class_name":"profile-plan","origin":"effective-filesystem",
				"mode":"rw","source":"/work","target":"/work","disposition":"present"
			}],"aliases":[]}
		}`
	})
	var stdout, stderr bytes.Buffer
	rc := runSandboxProfilesPlan(&sandboxProfilesPlanParams{
		Group: "crew", Cwd: "/work", For: "tclaude-layer/claude/linux",
	}, &stdout, &stderr)
	require.Equal(t, rcOK, rc, "stderr=%s", stderr.String())
	assert.Contains(t, stdout.String(), "Sandbox plan (hypothetical)")
	assert.Contains(t, stdout.String(), "isolated-with-agentd")
	require.Len(t, calls, 1)
	body, ok := calls[0].body.(sandboxProfilePlanRequestJSON)
	require.True(t, ok)
	assert.Equal(t, "crew", body.Group)
	assert.Empty(t, body.Agent)

	calls = nil
	stdout.Reset()
	stderr.Reset()
	rc = runSandboxProfilesPlan(&sandboxProfilesPlanParams{
		Agent: "worker", Group: "crew",
	}, &stdout, &stderr)
	assert.Equal(t, rcInvalidArg, rc)
	assert.Contains(t, stderr.String(), "--agent cannot be combined")
	assert.Empty(t, calls, "invalid mixed epistemic modes fail before contacting agentd")
}

func TestRunSandboxProfilesDraftUsesDraftOnlyHandoff(t *testing.T) {
	var calls []capturedReq
	stubDaemon(t, &calls, func(method, path string) (int, string, string) {
		assert.Equal(t, http.MethodPost, method)
		assert.Equal(t, "/v1/sandbox-profile-drafts/abcdefghijklmnop", path)
		return http.StatusAccepted, "", `{
			"accepted":true,"message":"draft validated",
			"notices":[{"class":"composition","axis":"network","reason":"empty_intersection",
				"effect":"nothing_allowed","detail":"draft network list is empty"}]
		}`
	})
	input := `{"name":"dev","filesystem":[{"path":"/work","access":"read"}],"environment":[]}`
	var stdout, stderr bytes.Buffer
	rc := runSandboxProfilesDraft(&sandboxProfilesDraftParams{
		Token: "abcdefghijklmnop", File: "-",
	}, strings.NewReader(input), &stdout, &stderr)
	require.Equal(t, rcOK, rc, "stderr=%s", stderr.String())
	require.Len(t, calls, 1)
	body, ok := calls[0].body.(struct {
		Profile json.RawMessage `json:"profile"`
	})
	require.True(t, ok)
	assert.JSONEq(t, input, string(body.Profile))
	assert.Contains(t, stdout.String(), "has not been saved")
	assert.Contains(t, stderr.String(), "Warning: draft network list is empty",
		"draft stdout is a handoff channel, so access notices are duplicated to stderr")
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
	assert.JSONEq(t, `{"name":"alpha","filesystem":[{"path":"/work","access":"write"}],"filesystem_spellings":null,"environment":[{"name":"CACHE","value":"/cache"}],"created_at":"2026-07-11T00:00:00Z"}`, stdout.String())
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
	created, ok := calls[0].body.(json.RawMessage)
	require.True(t, ok)
	assert.JSONEq(t, input, string(created),
		"show --json is accepted without lossy reshaping: the document reaches the daemon byte-for-byte, "+
			"created_at and all")

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

func TestSandboxProfileAccessAxesAndNoticesAreVisibleOnCLI(t *testing.T) {
	const notice = "profile tiers produce an empty network allow list"
	var calls []capturedReq
	stubDaemon(t, &calls, func(method, path string) (int, string, string) {
		switch method {
		case http.MethodGet:
			return 200, "", `{
				"name":"scoped","filesystem":[],"environment":[],
				"network":{"mode":"list","allow":[{"domain":"example.com","include_subdomains":true,"ports":[443]}]},
				"unix_sockets":{"mode":"list","allow":[{"path_glob":"/tmp/service-*.sock"}]}
			}`
		case http.MethodPost:
			return 201, "", `{"id":9,"name":"scoped","notices":[{
				"class":"composition","axis":"network","reason":"empty_intersection",
				"effect":"nothing_allowed","detail":"` + notice + `","tiers":["base","scoped"]
			}]}`
		default:
			return 404, "not_found", ""
		}
	})

	var stdout, stderr bytes.Buffer
	require.Equal(t, rcOK, runSandboxProfilesShow(
		&sandboxProfilesShowParams{Name: "scoped"}, &stdout, &stderr))
	assert.Contains(t, stdout.String(), "network: list")
	assert.Contains(t, stdout.String(), "*.example.com:443")
	assert.Contains(t, stdout.String(), "Unix sockets: list")
	assert.Contains(t, stdout.String(), "/tmp/service-*.sock")

	stdout.Reset()
	stderr.Reset()
	require.Equal(t, rcOK, runSandboxProfilesCreate(
		&sandboxProfilesFileParams{File: "-"},
		strings.NewReader(`{"name":"scoped","network":{"mode":"list","allow":[]}}`),
		&stdout, &stderr))
	assert.Contains(t, stdout.String(), "Warning: "+notice)
	assert.NotContains(t, stderr.String(), "Warning: "+notice,
		"human-readable commands use the existing stdout warning channel")
}

func TestSandboxProfileAxisLabelsDeriveLegacySocketPosture(t *testing.T) {
	network, sockets := sandboxProfileAxisLabels(sandboxProfileJSON{
		NetworkAccess: sandboxpolicy.NetworkAccessNone,
	})
	assert.Equal(t, "closed", network)
	assert.Equal(t, "closed", sockets)

	network, sockets = sandboxProfileAxisLabels(sandboxProfileJSON{
		NetworkAccess: sandboxpolicy.NetworkAccessInternet,
	})
	assert.Equal(t, "open", network)
	assert.Equal(t, "inherit", sockets)
}

func TestSandboxProfileShowForPrintsPredictionsAndJSON(t *testing.T) {
	var calls []capturedReq
	const prediction = `{"profile":"scoped","targets":[{
		"implementation":"tclaude-layer","harness":"claude","platform":"linux",
		"predicted":true,
		"axes":{
			"network":{"tier":"list","outcome":"not_enforced","detail":"bubblewrap has no filtered-egress applier"},
			"unix_sockets":{"tier":"closed","outcome":"refused","detail":"closed sockets cannot be enforced"}
		},
		"caveat":"(prediction for a non-host platform; host capability probes did not run)"
	}]}`
	stubDaemon(t, &calls, func(_ string, path string) (int, string, string) {
		if strings.Contains(path, "/enforcement?") {
			return 200, "", prediction
		}
		return 200, "", `{"name":"scoped","filesystem":[],"environment":[]}`
	})

	var stdout, stderr bytes.Buffer
	params := &sandboxProfilesShowParams{
		Name: "scoped", For: []string{"tclaude-layer/claude/linux"},
	}
	require.Equal(t, rcOK, runSandboxProfilesShow(params, &stdout, &stderr))
	assert.Contains(t, calls[0].path, "for=tclaude-layer%2Fclaude%2Flinux")
	assert.Contains(t, stdout.String(), "NOT ENFORCED")
	assert.Contains(t, stdout.String(), "REFUSED at launch")
	assert.Contains(t, stdout.String(), "prediction for a non-host platform")

	calls = nil
	stdout.Reset()
	params.JSON = true
	require.Equal(t, rcOK, runSandboxProfilesShow(params, &stdout, &stderr))
	assert.JSONEq(t, prediction, stdout.String())
	assert.Len(t, calls, 1, "--json returns the stable prediction shape directly")
}

func TestRunSandboxProfileDefaultAndGroupAssignments(t *testing.T) {
	var calls []capturedReq
	stubDaemon(t, &calls, func(method, path string) (int, string, string) {
		if strings.Contains(path, "/groups/") {
			if method == http.MethodDelete {
				return 200, "", `{"group":"crew","name":""}`
			}
			if method == http.MethodGet {
				return 200, "", `{"group":"crew","name":"dev"}`
			}
			return 200, "", `{"group":"crew","name":"dev","notices":[{"class":"composition","axis":"network","reason":"empty_intersection","effect":"nothing_allowed","detail":"group \"crew\": network access lists have an empty intersection","tiers":["global \"base\"","group \"dev\""]}]}`
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
	// TCL-791: the flag is a tombstone. It must fail with the real reason
	// rather than be accepted and ignored, and it must not reach the daemon.
	before := len(calls)
	assert.Equal(t, rcInvalidArg, runSandboxProfilesDefaultSet(
		&sandboxProfilesNameParams{Name: "dev", IUnderstandBreakGlassRisk: true}, &stdout, &stderr))
	assert.Contains(t, stderr.String(), "--i-understand-break-glass-risk no longer exists")
	assert.Contains(t, stderr.String(), "launch with the sandbox disabled")
	assert.Len(t, calls, before, "a tombstoned flag must not produce a daemon request")
	stderr.Reset()
	stdout.Reset()
	require.Equal(t, rcOK, runSandboxProfilesGroupSet(&sandboxProfilesGroupSetParams{Group: "crew", Name: "dev"}, &stdout, &stderr))
	assert.Equal(t, "/v1/groups/crew/sandbox-profile", calls[len(calls)-1].path)
	assert.Contains(t, stdout.String(), "crew: sandbox profile set to dev")
	assert.Contains(t, stdout.String(), "Warning: group \"crew\": network access lists have an empty intersection")
	assert.Empty(t, stderr.String(), "human assignment output keeps warnings on stdout")
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
