package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

const processTemplateYAML = `apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: demo
name: Demo
start: begin
nodes:
  begin:
    type: start
    next:
      pass: done
  done:
    type: end
    result: success
`

func TestRunProcessTemplatesLsUsesSharedRESTSurface(t *testing.T) {
	var calls []capturedReq
	stubDaemon(t, &calls, ok(`{"templates":[{"id":"demo","name":"Demo","description":"d","versionCount":2,"latestVersion":{"ref":"demo@sha256:abc","actor":"agent:agt_writer"}}]}`))
	var stdout, stderr bytes.Buffer

	rc := runProcessTemplatesLs(&stdout, &stderr)

	require.Equal(t, rcOK, rc, "stderr=%q", stderr.String())
	require.Len(t, calls, 1)
	assert.Equal(t, http.MethodGet, calls[0].method)
	assert.Equal(t, "/v1/process/templates", calls[0].path)
	assert.Contains(t, stdout.String(), "demo")
	assert.Contains(t, stdout.String(), "versions=2")
	assert.Contains(t, stdout.String(), "actor=agent:agt_writer")
}

func TestRunProcessTemplatesShowEmitsEditableYAMLAndCASMetadata(t *testing.T) {
	var calls []capturedReq
	response, err := json.Marshal(processTemplateShowJSON{
		Source: processTemplateYAML, SourceHash: "source-1", SemanticHash: "semantic-1", CurrentRef: "demo@sha256:semantic-1",
	})
	require.NoError(t, err)
	stubDaemon(t, &calls, ok(string(response)))
	var stdout, stderr bytes.Buffer

	rc := runProcessTemplatesShow(&processTemplatesShowParams{ID: "demo"}, &stdout, &stderr)

	require.Equal(t, rcOK, rc, "stderr=%q", stderr.String())
	assert.Contains(t, stdout.String(), "# tclaude currentRef: demo@sha256:semantic-1")
	assert.Contains(t, stdout.String(), "# tclaude sourceHash: source-1")
	assert.Contains(t, stdout.String(), "# tclaude semanticHash: semantic-1")
	parsed, err := model.Parse(stdout.Bytes())
	require.NoError(t, err, "show output must remain directly editable YAML")
	assert.Equal(t, "demo", parsed.Template.ID)
	assert.Equal(t, "/v1/process/templates/demo", calls[0].path)
}

func TestRunProcessTemplatesValidatePostsRawSourceAndFailsOnErrors(t *testing.T) {
	var calls []capturedReq
	stubDaemon(t, &calls, ok(`{"sourceHash":"src","semanticHash":"sem","diagnostics":[{"scope":"node","targetId":"begin","severity":"error","code":"broken","message":"fix it"}]}`))
	var stdout, stderr bytes.Buffer

	rc := runProcessTemplatesValidate(&processTemplatesValidateParams{File: "-"}, strings.NewReader(processTemplateYAML), &stdout, &stderr)

	assert.Equal(t, rcInvalidArg, rc)
	require.Len(t, calls, 1)
	assert.Equal(t, http.MethodPost, calls[0].method)
	assert.Equal(t, "/v1/process/validate", calls[0].path)
	body, ok := calls[0].body.(processTemplateSourceRequest)
	require.True(t, ok, "body type=%T", calls[0].body)
	assert.Equal(t, processTemplateYAML, body.Source)
	assert.Contains(t, stdout.String(), "ERROR broken [node:begin] fix it")
	assert.Contains(t, stdout.String(), "sourceHash=src")
}

func TestRunProcessTemplatesValidateRendersStableCardinalityCode(t *testing.T) {
	var calls []capturedReq
	stubDaemon(t, &calls, ok(`{"sourceHash":"src","semanticHash":"","diagnostics":[{"scope":"template","severity":"error","code":"normalized_edge_limit","message":"normalized edge count exceeds 4096 (counted at least 4097, including the synthetic start edge when present)"}]}`))
	var stdout, stderr bytes.Buffer

	rc := runProcessTemplatesValidate(&processTemplatesValidateParams{File: "-"}, strings.NewReader(processTemplateYAML), &stdout, &stderr)

	assert.Equal(t, rcInvalidArg, rc)
	assert.Contains(t, stdout.String(), "ERROR normalized_edge_limit [template]")
	assert.NotContains(t, stdout.String(), "node-")
}

func TestRunProcessTemplatesSaveRejectsCompactAliasCardinalityLocally(t *testing.T) {
	var calls []capturedReq
	stubDaemon(t, &calls, ok(`{}`))
	var stdout, stderr bytes.Buffer
	source := compactAliasProcessTemplate(64, 64)
	require.Less(t, len(source), 4<<20)

	rc := runProcessTemplatesSave(&processTemplatesSaveParams{File: "-"}, strings.NewReader(source), &stdout, &stderr)

	assert.Equal(t, rcInvalidArg, rc)
	assert.Empty(t, calls, "local raw guard rejects before contacting agentd")
	assert.Contains(t, stderr.String(), "ERROR normalized_edge_limit [template]")
}

func TestRunProcessTemplatesSaveRejectsMalformedGraphKeyLocally(t *testing.T) {
	var calls []capturedReq
	stubDaemon(t, &calls, ok(`{}`))
	var stdout, stderr bytes.Buffer
	source := `apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: malformed-key
nodes:
  ? [malformed]
  : {type: end}
`

	rc := runProcessTemplatesSave(&processTemplatesSaveParams{File: "-"}, strings.NewReader(source), &stdout, &stderr)

	assert.Equal(t, rcInvalidArg, rc)
	assert.Empty(t, calls, "local raw guard rejects before contacting agentd")
	assert.Contains(t, stderr.String(), "ERROR invalid_graph_key [template]")
}

func TestRunProcessTemplatesSaveRejectsSemanticDiagnosticFloodLocally(t *testing.T) {
	var calls []capturedReq
	stubDaemon(t, &calls, ok(`{}`))
	var stdout, stderr bytes.Buffer
	tmpl := &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "semantic-budget", Start: "source",
		Nodes: map[string]model.Node{
			"source": {
				Type: model.NodeTypeTask,
				Performer: &model.Performer{Kind: model.PerformerAgent,
					Prompt: strings.Repeat("{{ params.missing }}", 100_000)},
				Next: model.Next{"pass": "target"},
			},
			"target": {Type: model.NodeTypeEnd},
		},
	}
	source, err := model.CanonicalYAML(tmpl)
	require.NoError(t, err)
	require.Less(t, len(source), model.MaxProcessTemplateSourceBytes)

	rc := runProcessTemplatesSave(&processTemplatesSaveParams{File: "-"}, bytes.NewReader(source), &stdout, &stderr)

	assert.Equal(t, rcInvalidArg, rc)
	assert.Empty(t, calls, "local diagnostic budget rejects before contacting agentd")
	assert.Contains(t, stderr.String(), "ERROR template_diagnostic_budget [template]")
}

func TestRenderLocalGraphCardinalityDiagnosticsIncludesPredecodeRejections(t *testing.T) {
	for _, code := range []string{
		model.DiagnosticCodeDiagnosticBudget,
		model.DiagnosticCodeInvalidGraphKey,
		model.DiagnosticCodeInvalidGraphShape,
	} {
		t.Run(code, func(t *testing.T) {
			var stderr bytes.Buffer
			found := renderLocalGraphCardinalityDiagnostics(model.Diagnostics{{
				Severity: model.SeverityError,
				Code:     code,
				Message:  "bounded predecode rejection",
			}}, &stderr)
			assert.True(t, found)
			assert.Contains(t, stderr.String(), "ERROR "+code+" [template]")
		})
	}
}

func compactAliasProcessTemplate(nodeCount, outcomes int) string {
	var source strings.Builder
	source.WriteString("apiVersion: tclaude.dev/v1alpha1\nkind: ProcessTemplate\nid: aliases\nstart: n000\nnodes:\n")
	for nodeIndex := 0; nodeIndex < nodeCount; nodeIndex++ {
		fmt.Fprintf(&source, "  n%03d:\n    type: end\n    next: ", nodeIndex)
		if nodeIndex == 0 {
			source.WriteString("&shared\n")
			for outcome := 0; outcome < outcomes; outcome++ {
				fmt.Fprintf(&source, "      outcome-%03d: n000\n", outcome)
			}
		} else {
			source.WriteString("*shared\n")
		}
	}
	return source.String()
}

func TestRunProcessTemplatesSaveSendsCASAndAskHuman(t *testing.T) {
	var calls []capturedReq
	stubDaemon(t, &calls, ok(`{"ref":"demo@sha256:new","semanticHash":"new","sourceHash":"new-source","actor":"agent:agt_writer","diagnostics":[]}`))
	var stdout, stderr bytes.Buffer

	rc := runProcessTemplatesSave(&processTemplatesSaveParams{
		File: "-", ExpectSourceHash: "old-source", AskHuman: "30s",
	}, strings.NewReader(processTemplateYAML), &stdout, &stderr)

	require.Equal(t, rcOK, rc, "stderr=%q", stderr.String())
	require.Len(t, calls, 1)
	assert.Equal(t, "/v1/process/templates/demo", calls[0].path)
	body, ok := calls[0].body.(processTemplateSourceRequest)
	require.True(t, ok, "body type=%T", calls[0].body)
	assert.Equal(t, "old-source", body.SourceHash)
	assert.Equal(t, 30*time.Second, calls[0].opts.AskHuman)
	assert.Contains(t, stdout.String(), "Saved process template \"demo\"")
	assert.Contains(t, stdout.String(), "actor=agent:agt_writer")
}

func TestRunProcessTemplatesSavePreservesStructuredConflictGuidance(t *testing.T) {
	prevAvail, prevReq := DaemonAvailableImpl, DaemonRequestImpl
	t.Cleanup(func() { DaemonAvailableImpl, DaemonRequestImpl = prevAvail, prevReq })
	DaemonAvailableImpl = func() bool { return true }
	DaemonRequestImpl = func(string, string, any, any, DaemonOpts) error {
		raw := []byte(`{"error":"template head changed since it was opened","code":"process_template_conflict","currentSourceHash":"current-src","currentRef":"demo@sha256:current"}`)
		return &DaemonError{
			Status: http.StatusConflict, Code: "process_template_conflict",
			Msg: "template head changed since it was opened", Raw: raw,
		}
	}
	var stdout, stderr bytes.Buffer

	rc := runProcessTemplatesSave(&processTemplatesSaveParams{
		File: "-", ExpectSourceHash: "stale-src",
	}, strings.NewReader(processTemplateYAML), &stdout, &stderr)

	assert.Equal(t, rcIOFailure, rc)
	assert.Contains(t, stderr.String(), "code=process_template_conflict")
	assert.Contains(t, stderr.String(), "currentRef=demo@sha256:current")
	assert.Contains(t, stderr.String(), "currentSourceHash=current-src")
	assert.Contains(t, stderr.String(), "show demo")
	assert.Contains(t, stderr.String(), "--expect-source-hash current-src")
	assert.Contains(t, stderr.String(), "Never blind-overwrite")
}
