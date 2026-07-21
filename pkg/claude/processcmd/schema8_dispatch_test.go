package processcmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
)

func TestSchema8DefaultRootCLIUsesDaemonOnly(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := store.DefaultRoot()
	runID, source := seedSchema8DispatchRun(t, root, "schema8-cli")
	candidatePath := filepath.Join(t.TempDir(), "candidate.yaml")
	if err := os.WriteFile(candidatePath, source, 0o600); err != nil {
		t.Fatal(err)
	}

	previousRequest, previousRaw := agent.DaemonRequestImpl, agent.DaemonGetRawImpl
	t.Cleanup(func() { agent.DaemonRequestImpl, agent.DaemonGetRawImpl = previousRequest, previousRaw })
	var calls []string
	var unblockWire []byte
	agent.DaemonRequestImpl = func(method, path string, in, out any, opts agent.DaemonOpts) error {
		if opts.Timeout != schema8DaemonTimeout {
			t.Fatalf("schema-8 daemon timeout = %s", opts.Timeout)
		}
		calls = append(calls, method+" "+path)
		var response string
		switch method + " " + path {
		case "GET /v1/process/runs/" + runID:
			response = `{"run":{"id":"schema8-cli","templateRef":"schema8-cli@sha256:` + strings.Repeat("a", 64) + `","effectiveStatus":"running"},"lineage":{"currentTemplateRef":"schema8-cli@sha256:` + strings.Repeat("a", 64) + `","epochs":[{}]},"authorityCounts":{"total":1},"currentBinding":{"revision":7,"digest":"` + strings.Repeat("b", 64) + `"}}`
		case "GET /v1/process/runs/" + runID + "/verify":
			response = `{"verified":true,"view":{"run":{"id":"schema8-cli","effectiveStatus":"failed"}}}`
		case "POST /v1/process/runs/" + runID + "/unlock/preview":
			response = `{"status":"valid","classification":"cannot_affect_without_later_intervention"}`
		case "POST /v1/process/runs/" + runID + "/unblock":
			unblockWire, _ = json.Marshal(in)
			response = `{"settled":true,"decision":"retry","repreviewRequired":true}`
		default:
			t.Fatalf("unexpected daemon request %s %s", method, path)
		}
		return json.Unmarshal([]byte(response), out)
	}
	agent.DaemonGetRawImpl = func(path string) ([]byte, http.Header, error) {
		calls = append(calls, "RAW "+path)
		return []byte("exact-diff"), http.Header{}, nil
	}

	cmd := &cobra.Command{}
	cmd.SetContext(t.Context())
	var out bytes.Buffer
	if err := runShowDispatch(cmd, &showParams{RunID: runID, StoreRoot: root}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Base revision: 7") {
		t.Fatalf("unexpected show output: %s", out.String())
	}
	out.Reset()
	if err := runVerifyDispatch(t.Context(), &verifyParams{RunID: runID, StoreRoot: root}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Effective status: failed") {
		t.Fatalf("unexpected verify output: %s", out.String())
	}
	out.Reset()
	if err := runPreview(cmd, &previewParams{RunID: runID, StoreRoot: root, CandidateFile: candidatePath, BaseRevision: 7, BaseDigest: strings.Repeat("b", 64)}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "cannot_affect_without_later_intervention") {
		t.Fatalf("unexpected preview output: %s", out.String())
	}
	out.Reset()
	if err := runUnblockDispatch(cmd, &unblockParams{RunID: runID, NodeID: strings.Repeat("c", 64), StoreRoot: root, Decision: "retry", Reason: "operator decision", EvidenceRef: "artifact:1", BaseRevision: 7, BaseDigest: strings.Repeat("b", 64)}, &out); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(unblockWire, []byte(`"actor"`)) || bytes.Contains(unblockWire, []byte(`"timestamp"`)) {
		t.Fatalf("client authority leaked into schema-8 settlement: %s", unblockWire)
	}
	out.Reset()
	if err := runShowDispatch(cmd, &showParams{RunID: runID, StoreRoot: root, Epoch: strings.Repeat("d", 64), Diff: true}, &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != "exact-diff" {
		t.Fatalf("exact artifact bytes changed: %q", out.String())
	}
	if len(calls) != 5 {
		t.Fatalf("daemon calls = %v", calls)
	}
}

func TestSchema8CustomRootCLIIsRejectedBeforeDaemon(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	customRoot := filepath.Join(t.TempDir(), "portable")
	runID, _ := seedSchema8DispatchRun(t, customRoot, "schema8-custom")
	previous := agent.DaemonRequestImpl
	t.Cleanup(func() { agent.DaemonRequestImpl = previous })
	agent.DaemonRequestImpl = func(string, string, any, any, agent.DaemonOpts) error {
		t.Fatal("custom-root schema-8 run reached daemon")
		return nil
	}
	cmd := &cobra.Command{}
	cmd.SetContext(t.Context())
	err := runShowDispatch(cmd, &showParams{RunID: runID, StoreRoot: customRoot}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "canonical process store") {
		t.Fatalf("unexpected custom-root result: %v", err)
	}
}

func TestSchema8ArtifactFlagsRejectCustomRootBeforeSchemaProbe(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	customRoot := filepath.Join(t.TempDir(), "portable")
	seedLegacyDispatchRun(t, customRoot, "legacy-custom")
	previous := agent.DaemonRequestImpl
	t.Cleanup(func() { agent.DaemonRequestImpl = previous })
	agent.DaemonRequestImpl = func(string, string, any, any, agent.DaemonOpts) error {
		t.Fatal("custom-root legacy run reached daemon")
		return nil
	}
	cmd := &cobra.Command{}
	cmd.SetContext(t.Context())
	var out bytes.Buffer
	if err := runShowDispatch(cmd, &showParams{RunID: "legacy-custom", StoreRoot: customRoot}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Run: legacy-custom") {
		t.Fatalf("unexpected custom-root legacy output: %s", out.String())
	}
	err := runShowDispatch(cmd, &showParams{
		RunID: "legacy-custom", StoreRoot: customRoot,
		Epoch: strings.Repeat("d", 64), Diff: true,
	}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "canonical process store") {
		t.Fatalf("unexpected custom-root artifact result: %v", err)
	}
}

func TestLegacyDefaultRootCLIStaysDirect(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := store.DefaultRoot()
	seedLegacyDispatchRun(t, root, "legacy-cli")
	previous := agent.DaemonRequestImpl
	t.Cleanup(func() { agent.DaemonRequestImpl = previous })
	agent.DaemonRequestImpl = func(string, string, any, any, agent.DaemonOpts) error {
		t.Fatal("legacy run reached daemon")
		return nil
	}
	cmd := &cobra.Command{}
	cmd.SetContext(t.Context())
	var out bytes.Buffer
	if err := runShowDispatch(cmd, &showParams{RunID: "legacy-cli", StoreRoot: root}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Run: legacy-cli") {
		t.Fatalf("unexpected legacy output: %s", out.String())
	}
	err := runShowDispatch(cmd, &showParams{
		RunID: "legacy-cli", StoreRoot: root,
		Epoch: strings.Repeat("d", 64), Reason: true,
	}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "only available for schema-8") {
		t.Fatalf("unexpected legacy artifact result: %v", err)
	}
}

func seedLegacyDispatchRun(t *testing.T, root, runID string) {
	t.Helper()
	fs, err := store.NewFS(root)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := schema8DispatchTemplate(runID)
	record, err := fs.PutTemplate(t.Context(), tmpl)
	if err != nil {
		t.Fatal(err)
	}
	initial := state.New(runID, record.Ref, record.Ref, []state.NodeInit{{ID: "work", Type: model.NodeTypeTask, Status: state.NodeStatusReady}, {ID: "done", Type: model.NodeTypeEnd, Status: state.NodeStatusPending}})
	if _, err := fs.CreateRun(t.Context(), store.RunRecord{ID: runID, TemplateRef: record.Ref}, initial); err != nil {
		t.Fatal(err)
	}
}

func TestPreviewDomainErrorsRenderStructuredBodyAndRemainNonzero(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := store.DefaultRoot()
	runID, source := seedSchema8DispatchRun(t, root, "preview-domain")
	candidatePath := filepath.Join(t.TempDir(), "candidate.yaml")
	if err := os.WriteFile(candidatePath, source, 0o600); err != nil {
		t.Fatal(err)
	}
	base := strings.Repeat("b", 64)
	token := strings.Repeat("c", 64)
	blocked := `{"status":"blocked","baseBinding":{"revision":7,"digest":"` + base + `"},"currentBinding":{"revision":7,"digest":"` + base + `"},"graphSummary":{"current":{"nodes":2,"edges":1},"candidate":{"nodes":2,"edges":1},"changed":true},"lineage":{"originalTemplateRef":"preview-domain@sha256:` + strings.Repeat("a", 64) + `","currentTemplateRef":"preview-domain@sha256:` + strings.Repeat("a", 64) + `","epochs":[{"ordinal":0,"templateRef":"preview-domain@sha256:` + strings.Repeat("a", 64) + `"}]},"authorityCounts":{"total":1,"active":1,"terminal":0},"blockers":[{"code":"handoff_missing","token":"` + token + `"}]}`
	stale := `{"status":"stale","currentBinding":{"revision":8,"digest":"` + strings.Repeat("d", 64) + `"}}`

	previous := agent.DaemonRequestImpl
	t.Cleanup(func() { agent.DaemonRequestImpl = previous })
	cmd := &cobra.Command{}
	cmd.SetContext(t.Context())
	for _, tc := range []struct {
		name   string
		status int
		raw    string
		want   error
		field  string
	}{
		{name: "blocked", status: http.StatusUnprocessableEntity, raw: blocked, want: errPreviewBlocked, field: token},
		{name: "stale", status: http.StatusConflict, raw: stale, want: errPreviewStale, field: strings.Repeat("d", 64)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent.DaemonRequestImpl = func(string, string, any, any, agent.DaemonOpts) error {
				return &agent.DaemonError{Status: tc.status, Raw: []byte(tc.raw)}
			}
			var out bytes.Buffer
			err := runPreview(cmd, &previewParams{RunID: runID, StoreRoot: root, CandidateFile: candidatePath, BaseRevision: 7, BaseDigest: base}, &out)
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			if !strings.Contains(out.String(), tc.field) {
				t.Fatalf("structured domain response not rendered: %s", out.String())
			}
			var rendered map[string]any
			if err := json.Unmarshal(out.Bytes(), &rendered); err != nil {
				t.Fatalf("rendered response is not JSON: %v", err)
			}
			if rendered["status"] != tc.name {
				t.Fatalf("rendered status = %v", rendered["status"])
			}
		})
	}
}

func TestPreviewDomainErrorsFailClosedOnMalformedOrUnexpectedRaw(t *testing.T) {
	for _, daemonErr := range []*agent.DaemonError{
		{Status: http.StatusConflict, Raw: []byte(`{"status":"stale","currentBinding":{"revision":1,"digest":"` + strings.Repeat("a", 64) + `"},"extra":"secret"}`)},
		{Status: http.StatusUnprocessableEntity, Raw: []byte(`{"status":"blocked"}`)},
		{Status: http.StatusUnprocessableEntity, Raw: []byte(`{"status":"blocked","baseBinding":{"revision":1,"digest":"` + strings.Repeat("a", 64) + `"},"currentBinding":{"revision":1,"digest":"` + strings.Repeat("a", 64) + `"},"blockers":[{"code":"handoff_missing","token":"` + strings.Repeat("b", 64) + `"}]}`)},
		{Status: http.StatusUnprocessableEntity, Raw: []byte(`{"code":"process_preview_invalid","error":"invalid"}`)},
	} {
		var out bytes.Buffer
		if err, handled := renderPreviewDomainError(&out, daemonErr); handled || err != nil || out.Len() != 0 {
			t.Fatalf("unexpected malformed response handling: handled=%v err=%v out=%q", handled, err, out.String())
		}
	}
}

func seedSchema8DispatchRun(t *testing.T, root, runID string) (string, []byte) {
	t.Helper()
	fs, err := store.NewFS(root)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := schema8DispatchTemplate(runID)
	source, err := model.CanonicalYAML(tmpl)
	if err != nil {
		t.Fatal(err)
	}
	record, err := fs.PutTemplate(t.Context(), tmpl)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fs.InitializeEpochV8Run(t.Context(), store.RunRecord{ID: runID, TemplateRef: record.Ref}, source); err != nil {
		t.Fatal(err)
	}
	return runID, source
}

func schema8DispatchTemplate(id string) *model.Template {
	return &model.Template{APIVersion: model.APIVersion, Kind: model.Kind, ID: id, Start: "work", Nodes: map[string]model.Node{
		"work": {Type: model.NodeTypeTask, Performer: &model.Performer{Kind: model.PerformerAgent, Prompt: "private"}, Next: model.Next{"pass": "done"}},
		"done": {Type: model.NodeTypeEnd, Result: "completed"},
	}}
}
