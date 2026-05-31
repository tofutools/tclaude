package workflows

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setupHome builds a temp $HOME populated from the ccworkflows package's vetted
// fixtures: the run tree under ~/.claude/projects and the saved templates under
// ~/.claude/workflows/saved. It returns nothing; callers read via the default
// home-resolving ccworkflows wrappers.
func setupHome(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)

	projectsDst := filepath.Join(home, ".claude", "projects")
	if err := os.CopyFS(projectsDst, os.DirFS("../ccworkflows/testdata/projects")); err != nil {
		t.Fatalf("copy projects fixtures: %v", err)
	}
	savedDst := filepath.Join(home, ".claude", "workflows", "saved")
	if err := os.CopyFS(savedDst, os.DirFS("../ccworkflows/testdata/saved")); err != nil {
		t.Fatalf("copy saved fixtures: %v", err)
	}
}

func captureLs(t *testing.T, p *LsParams) (string, int) {
	t.Helper()
	var out, errb bytes.Buffer
	code := RunLs(p, &out, &errb)
	if errb.Len() > 0 {
		t.Logf("stderr: %s", errb.String())
	}
	return out.String(), code
}

func TestRunLs_Both(t *testing.T) {
	setupHome(t)
	out, code := captureLs(t, &LsParams{})
	if code != 0 {
		t.Fatalf("code = %d", code)
	}
	// Saved section: the three saved fixtures.
	if !strings.Contains(out, "SAVED TEMPLATES (3)") {
		t.Errorf("missing saved header:\n%s", out)
	}
	// The table lists by filename (Name), not meta.name.
	if !strings.Contains(out, "ccwf-fixture-probe") || !strings.Contains(out, "double-quoted") {
		t.Errorf("saved names missing:\n%s", out)
	}
	// Runs section: the four run fixtures (3 completed + 1 live).
	if !strings.Contains(out, "RUNS (4)") {
		t.Errorf("missing runs header:\n%s", out)
	}
	if !strings.Contains(out, "wf_213c457c-3ac") || !strings.Contains(out, "completed") {
		t.Errorf("expected completed run row:\n%s", out)
	}
	if !strings.Contains(out, "wf_11ab22cd-e01") || !strings.Contains(out, "running") {
		t.Errorf("expected live run row:\n%s", out)
	}
}

func TestRunLs_SavedOnly(t *testing.T) {
	setupHome(t)
	out, code := captureLs(t, &LsParams{Saved: true})
	if code != 0 {
		t.Fatalf("code = %d", code)
	}
	if !strings.Contains(out, "SAVED TEMPLATES") {
		t.Errorf("missing saved:\n%s", out)
	}
	if strings.Contains(out, "RUNS (") {
		t.Errorf("--saved should omit runs:\n%s", out)
	}
}

func TestRunLs_RunsOnly(t *testing.T) {
	setupHome(t)
	out, code := captureLs(t, &LsParams{Runs: true})
	if code != 0 {
		t.Fatalf("code = %d", code)
	}
	if !strings.Contains(out, "RUNS (") {
		t.Errorf("missing runs:\n%s", out)
	}
	if strings.Contains(out, "SAVED TEMPLATES") {
		t.Errorf("--runs should omit saved:\n%s", out)
	}
}

func TestRunLs_JSON(t *testing.T) {
	setupHome(t)
	var out, errb bytes.Buffer
	if code := RunLs(&LsParams{JSON: true}, &out, &errb); code != 0 {
		t.Fatalf("code = %d, stderr=%s", code, errb.String())
	}
	var got lsJSON
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out.String())
	}
	if got.Saved == nil || len(*got.Saved) != 3 {
		t.Errorf("saved = %v, want 3", got.Saved)
	}
	if got.Runs == nil || len(*got.Runs) != 4 {
		t.Errorf("runs = %v, want 4", got.Runs)
	}
}

func TestRunLs_JSONFilteredSectionAbsent(t *testing.T) {
	setupHome(t)
	var out, errb bytes.Buffer
	if code := RunLs(&LsParams{Runs: true, JSON: true}, &out, &errb); code != 0 {
		t.Fatalf("code = %d", code)
	}
	// --runs: runs present, saved key absent (not null, not []).
	if !strings.Contains(out.String(), `"runs"`) {
		t.Errorf("runs key missing:\n%s", out.String())
	}
	if strings.Contains(out.String(), `"saved"`) {
		t.Errorf("saved key should be absent under --runs:\n%s", out.String())
	}
}

func TestRunShow_Tree(t *testing.T) {
	setupHome(t)
	var out, errb bytes.Buffer
	if code := RunShow(&ShowParams{RunID: "wf_213c457c-3ac"}, &out, &errb); code != 0 {
		t.Fatalf("code = %d, stderr=%s", code, errb.String())
	}
	s := out.String()
	for _, want := range []string{
		"Run wf_213c457c-3ac  [completed]", "workflow: ccwf-fixture-probe",
		"Phase 1: Scout", "scout:alpha", "Phase 2: Fan", "fan:bravo", "fan:charlie",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("show output missing %q:\n%s", want, s)
		}
	}
}

func TestRunShow_LiveBestEffortMarker(t *testing.T) {
	setupHome(t)
	var out, errb bytes.Buffer
	if code := RunShow(&ShowParams{RunID: "wf_11ab22cd-e01"}, &out, &errb); code != 0 {
		t.Fatalf("code = %d, stderr=%s", code, errb.String())
	}
	s := out.String()
	if !strings.Contains(s, "[running]") {
		t.Errorf("expected a running status in live run:\n%s", s)
	}
	// The live run's script is static with a 1:1 journal match, so labels are
	// confident (• marker, not ~).
	if !strings.Contains(s, "build:a") {
		t.Errorf("expected recovered label build:a:\n%s", s)
	}
	// Live token accrual surfaces in the tree (read from the agent transcript).
	if !strings.Contains(s, "tokens=5600") {
		t.Errorf("expected live token accrual tokens=5600:\n%s", s)
	}
}

func TestRunShow_Script(t *testing.T) {
	setupHome(t)
	var out, errb bytes.Buffer
	if code := RunShow(&ShowParams{RunID: "wf_213c457c-3ac", Script: true}, &out, &errb); code != 0 {
		t.Fatalf("code = %d, stderr=%s", code, errb.String())
	}
	s := out.String()
	if !strings.Contains(s, "export const meta") || !strings.Contains(s, "phase('Scout')") {
		t.Errorf("--script did not print the script:\n%s", s)
	}
}

func TestRunShow_JSON(t *testing.T) {
	setupHome(t)
	var out, errb bytes.Buffer
	if code := RunShow(&ShowParams{RunID: "wf_213c457c-3ac", JSON: true}, &out, &errb); code != 0 {
		t.Fatalf("code = %d", code)
	}
	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if got["runId"] != "wf_213c457c-3ac" || got["status"] != "completed" {
		t.Errorf("json = %v", got)
	}
}

func TestRunShow_Unknown(t *testing.T) {
	setupHome(t)
	var out, errb bytes.Buffer
	code := RunShow(&ShowParams{RunID: "wf_nope-xxx"}, &out, &errb)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if !strings.Contains(errb.String(), "not found") {
		t.Errorf("stderr = %q", errb.String())
	}
}

func TestRunShow_MissingArg(t *testing.T) {
	var out, errb bytes.Buffer
	if code := RunShow(&ShowParams{}, &out, &errb); code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
}

func TestRunCat(t *testing.T) {
	setupHome(t)
	var out, errb bytes.Buffer
	if code := RunCat(&CatParams{Name: "ccwf-fixture-probe"}, &out, &errb); code != 0 {
		t.Fatalf("code = %d, stderr=%s", code, errb.String())
	}
	if !strings.Contains(out.String(), "export const meta") {
		t.Errorf("cat output:\n%s", out.String())
	}
}

func TestRunCat_Missing(t *testing.T) {
	setupHome(t)
	var out, errb bytes.Buffer
	code := RunCat(&CatParams{Name: "no-such-workflow"}, &out, &errb)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if !strings.Contains(errb.String(), "no saved workflow") {
		t.Errorf("stderr = %q", errb.String())
	}
}

func TestRunCat_MissingArg(t *testing.T) {
	var out, errb bytes.Buffer
	if code := RunCat(&CatParams{}, &out, &errb); code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
}

// --- formatting helpers ----------------------------------------------------

func TestFormattingHelpers(t *testing.T) {
	if got := fmtTimeMs(0); got != "-" {
		t.Errorf("fmtTimeMs(0) = %q", got)
	}
	if got := fmtDurationMs(0); got != "-" {
		t.Errorf("fmtDurationMs(0) = %q", got)
	}
	if got := fmtDurationMs(3028); got != "3.0s" {
		t.Errorf("fmtDurationMs(3028) = %q, want 3.0s", got)
	}
	if got := fmtDurationMs(64000); got != "1m04s" {
		t.Errorf("fmtDurationMs(64000) = %q, want 1m04s", got)
	}
	if got := fmtCount(0); got != "-" {
		t.Errorf("fmtCount(0) = %q", got)
	}
	if got := shortID("11111111-2222-3333"); got != "11111111" {
		t.Errorf("shortID = %q", got)
	}
	if got := firstLine("hello\nworld", 100); got != "hello" {
		t.Errorf("firstLine newline = %q", got)
	}
	if got := firstLine("abcdefghij", 5); got != "abcd…" {
		t.Errorf("firstLine truncate = %q", got)
	}
}
