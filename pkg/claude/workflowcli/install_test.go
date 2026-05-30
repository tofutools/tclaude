package workflowcli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/workflow"
)

// writeTemplateFixture lays down a minimal valid template dir (workflow.yaml +
// flow.mmd + two node files) so a dir: install has something real to resolve +
// copy. No network — install is exercised purely against local dir: sources.
func writeTemplateFixture(t *testing.T, dir, name string) {
	t.Helper()
	write := func(rel, content string) {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("workflow.yaml", "name: "+name+"\nentry: work\n")
	write("flow.mmd", "flowchart TD\n  work --> done\n")
	write("nodes/work.yaml", "label: Work\nexecutor:\n  kind: ai\n  agent: worker\n  prompt: do the thing\n")
	write("nodes/done.yaml", "label: Done\nexecutor:\n  kind: human\n")
}

// installHarness sets HOME to a temp dir (so UserDir() → <tmp>/.tclaude/workflows)
// and writes a source fixture, returning (srcDir, userDir).
func installHarness(t *testing.T, tmplName string) (string, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	src := filepath.Join(t.TempDir(), "src")
	writeTemplateFixture(t, src, tmplName)
	return src, filepath.Join(home, ".tclaude", "workflows")
}

func TestRunInstall_DirSourceRoundTrips(t *testing.T) {
	src, userDir := installHarness(t, "myflow")
	var out, errBuf bytes.Buffer
	if rc := runInstall(&installParams{Src: "dir:" + src}, &out, &errBuf); rc != rcOK {
		t.Fatalf("runInstall rc=%d stderr=%s", rc, errBuf.String())
	}
	// Files landed under user:myflow.
	for _, rel := range []string{"workflow.yaml", "flow.mmd", "nodes/work.yaml", "nodes/done.yaml"} {
		if _, err := os.Stat(filepath.Join(userDir, "myflow", rel)); err != nil {
			t.Errorf("expected installed file %s: %v", rel, err)
		}
	}
	if !strings.Contains(out.String(), "user:myflow") {
		t.Errorf("output should name the installed ref\n%s", out.String())
	}
	// The whole point: it's now resolvable as a user template.
	tmpl, err := workflow.Resolve("user:myflow")
	if err != nil {
		t.Fatalf("installed template should resolve as user:myflow: %v", err)
	}
	if len(tmpl.Nodes) != 2 {
		t.Errorf("installed template has %d nodes, want 2", len(tmpl.Nodes))
	}
}

func TestRunInstall_NameOverride(t *testing.T) {
	src, userDir := installHarness(t, "myflow")
	var out, errBuf bytes.Buffer
	if rc := runInstall(&installParams{Src: "dir:" + src, Name: "renamed"}, &out, &errBuf); rc != rcOK {
		t.Fatalf("rc=%d stderr=%s", rc, errBuf.String())
	}
	if _, err := os.Stat(filepath.Join(userDir, "renamed", "workflow.yaml")); err != nil {
		t.Errorf("--name should install under the override: %v", err)
	}
	if _, err := os.Stat(filepath.Join(userDir, "myflow")); err == nil {
		t.Error("--name should NOT also install under the template's own name")
	}
}

func TestRunInstall_BarePathNormalizedToDir(t *testing.T) {
	src, userDir := installHarness(t, "bareflow")
	var out, errBuf bytes.Buffer
	// No dir: prefix — a bare existing path must be treated as a dir: source.
	if rc := runInstall(&installParams{Src: src}, &out, &errBuf); rc != rcOK {
		t.Fatalf("bare-path install rc=%d stderr=%s", rc, errBuf.String())
	}
	if _, err := os.Stat(filepath.Join(userDir, "bareflow", "workflow.yaml")); err != nil {
		t.Errorf("bare path should install: %v", err)
	}
}

func TestRunInstall_ExistsForce(t *testing.T) {
	src, _ := installHarness(t, "dupe")
	var out, errBuf bytes.Buffer
	if rc := runInstall(&installParams{Src: "dir:" + src}, &out, &errBuf); rc != rcOK {
		t.Fatalf("first install rc=%d", rc)
	}
	// Second install without --force is refused.
	out.Reset()
	errBuf.Reset()
	if rc := runInstall(&installParams{Src: "dir:" + src}, &out, &errBuf); rc != rcInvalidArg {
		t.Fatalf("re-install without --force rc=%d, want rcInvalidArg(%d)", rc, rcInvalidArg)
	}
	// With --force it succeeds.
	out.Reset()
	errBuf.Reset()
	if rc := runInstall(&installParams{Src: "dir:" + src, Force: true}, &out, &errBuf); rc != rcOK {
		t.Fatalf("re-install --force rc=%d stderr=%s", rc, errBuf.String())
	}
}

func TestRunInstall_SymlinkRejected(t *testing.T) {
	src, userDir := installHarness(t, "evil")
	// Plant a symlink as a NON-template file (the loader ignores README.md, so
	// Resolve still succeeds) — install must refuse to bake a link that a later
	// load could follow into the user dir, even for a file the resolver skipped.
	if err := os.Symlink("/etc/hosts", filepath.Join(src, "README.md")); err != nil {
		t.Skipf("symlink unsupported here: %v", err)
	}
	var out, errBuf bytes.Buffer
	rc := runInstall(&installParams{Src: "dir:" + src}, &out, &errBuf)
	if rc != rcIOFailure {
		t.Fatalf("symlinked template install rc=%d, want rcIOFailure(%d)", rc, rcIOFailure)
	}
	if !strings.Contains(errBuf.String(), "symlink") {
		t.Errorf("error should mention the symlink\n%s", errBuf.String())
	}
	// Atomic staging: a refused install leaves nothing behind.
	if _, err := os.Stat(filepath.Join(userDir, "evil")); err == nil {
		t.Error("a refused install must not leave a half-installed template")
	}
}

// A --name that tries to escape the user workflows dir must be rejected by
// validInstallName before any copy, so it can't write outside ~/.tclaude/workflows.
func TestRunInstall_NameTraversalRejected(t *testing.T) {
	src, userDir := installHarness(t, "ok")
	for _, bad := range []string{"../escape", "..", "a/b", `a\b`, "."} {
		var out, errBuf bytes.Buffer
		if rc := runInstall(&installParams{Src: "dir:" + src, Name: bad}, &out, &errBuf); rc != rcInvalidArg {
			t.Errorf("install --name %q rc=%d, want rcInvalidArg(%d)", bad, rc, rcInvalidArg)
		}
	}
	// And nothing leaked outside the user dir (parent of userDir stays clean of
	// an "escape" entry).
	if _, err := os.Stat(filepath.Join(filepath.Dir(userDir), "escape")); err == nil {
		t.Error("a traversing --name wrote outside the user workflows dir")
	}
}

// Force-overwrite replaces the template atomically, so a file present in the
// first install but absent from the new source is gone afterwards (the
// RemoveAll+rename publish, not an in-place merge).
func TestRunInstall_ForceRemovesStale(t *testing.T) {
	src, userDir := installHarness(t, "staleflow")
	// First install carries an extra (non-node) file the loader ignores but the
	// copy still picks up.
	if err := os.WriteFile(filepath.Join(src, "README.md"), []byte("old docs\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errBuf bytes.Buffer
	if rc := runInstall(&installParams{Src: "dir:" + src}, &out, &errBuf); rc != rcOK {
		t.Fatalf("first install rc=%d stderr=%s", rc, errBuf.String())
	}
	stale := filepath.Join(userDir, "staleflow", "README.md")
	if _, err := os.Stat(stale); err != nil {
		t.Fatalf("first install should have copied README.md: %v", err)
	}
	// Remove the extra file from the source, re-install with --force.
	if err := os.Remove(filepath.Join(src, "README.md")); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errBuf.Reset()
	if rc := runInstall(&installParams{Src: "dir:" + src, Force: true}, &out, &errBuf); rc != rcOK {
		t.Fatalf("force re-install rc=%d stderr=%s", rc, errBuf.String())
	}
	if _, err := os.Stat(stale); err == nil {
		t.Error("force-overwrite must replace (not merge) — the stale extra.yaml should be gone")
	}
}

func TestRunInstall_EmbeddedExampleRejected(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	var out, errBuf bytes.Buffer
	// example: is embedded — no source dir to copy from.
	rc := runInstall(&installParams{Src: "example:implement-microservice"}, &out, &errBuf)
	if rc != rcInvalidArg {
		t.Fatalf("installing an embedded example rc=%d, want rcInvalidArg(%d)", rc, rcInvalidArg)
	}
}

func TestRunInstall_UnknownDirRef(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	var out, errBuf bytes.Buffer
	rc := runInstall(&installParams{Src: "dir:/no/such/template/dir"}, &out, &errBuf)
	if rc != rcNotFound {
		t.Fatalf("install of a missing dir rc=%d, want rcNotFound(%d)", rc, rcNotFound)
	}
}

func TestRunInstall_JSON(t *testing.T) {
	src, _ := installHarness(t, "jflow")
	var out, errBuf bytes.Buffer
	if rc := runInstall(&installParams{Src: "dir:" + src, JSON: true}, &out, &errBuf); rc != rcOK {
		t.Fatalf("rc=%d stderr=%s", rc, errBuf.String())
	}
	var got struct {
		Ref       string `json:"ref"`
		Name      string `json:"name"`
		NodeCount int    `json:"node_count"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("install --json invalid: %v\n%s", err, out.String())
	}
	if got.Ref != "user:jflow" || got.Name != "jflow" || got.NodeCount != 2 {
		t.Errorf("install --json = %+v, want user:jflow / jflow / 2 nodes", got)
	}
}
