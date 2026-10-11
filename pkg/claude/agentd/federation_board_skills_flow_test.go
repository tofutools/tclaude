package agentd_test

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/skillroots"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func TestDashboardBoardSkillPublishPreviewImport(t *testing.T) {
	_ = newFedHarness(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://localhost:12345"))
	dash := agentd.BuildDashboardHandlerForTest()
	call := func(method, tail string, p any) *httptest.ResponseRecorder {
		t.Helper()
		return testharness.Serve(dash, testharness.JSONRequest(t, method, "/api/federation/boards"+tail, p))
	}
	must := func(method, tail string, p any) map[string]any {
		t.Helper()
		r := call(method, tail, p)
		require.Equal(t, 200, r.Code, r.Body.String())
		var out map[string]any
		testharness.DecodeJSON(t, r, &out)
		return out
	}
	root, e := skillroots.Claude()
	require.NoError(t, e)
	shared, e := skillroots.Codex()
	require.NoError(t, e)
	source := filepath.Join(root, "demo-recording")
	require.NoError(t, os.MkdirAll(filepath.Join(source, "scripts"), 0700))
	require.NoError(t, os.WriteFile(filepath.Join(source, "SKILL.md"), []byte("# Demo recording\nRun scripts/record.py.\n"), 0600))
	script := "#!/usr/bin/env python3\nprint('<img src=x onerror=alert(1)>')\n"
	require.NoError(t, os.WriteFile(filepath.Join(source, "scripts", "record.py"), []byte(script), 0700))
	board := must("POST", "", map[string]any{"name": "skills library"})["id"].(string)
	receipt := must("POST", "/"+board+"/items", map[string]any{"name": "Demo recording", "only": []string{"skills/demo-recording"}, "skill_root": "claude"})
	base := "/" + board + "/items/" + receipt["item"].(string) + "/versions/" + receipt["version"].(string)
	must("POST", base+"/fetch", map[string]any{})
	contents := must("GET", base+"/contents", nil)
	require.Len(t, contents["entries"], 2)
	entry := must("GET", base+"/contents?path=skills/demo-recording/scripts/record.py", nil)
	require.Equal(t, script, entry["text"])
	preview := must("POST", base+"/preview", map[string]any{})
	require.Equal(t, "skill", preview["kind"])
	require.NotEmpty(t, preview["skill_roots"])
	files := preview["files"].([]any)
	require.Len(t, files, 2)
	require.Equal(t, true, files[1].(map[string]any)["security"])
	require.Equal(t, script, files[1].(map[string]any)["text"])
	require.NoError(t, os.RemoveAll(source))
	targets := []string{"claude", "agents"}
	opts := map[string]any{"skill_targets": targets}
	preview = must("POST", base+"/preview", opts)
	require.NoDirExists(t, source, "preview must not write")
	require.Equal(t, 409, call("POST", base+"/import", opts).Code)
	changed := map[string]any{"skill_targets": targets, "preserve_executable": true, "preview_token": preview["preview_token"]}
	require.Equal(t, 409, call("POST", base+"/import", changed).Code, "executable confirmation is bound to preview")
	preview = must("POST", base+"/preview", opts)
	opts["preview_token"] = preview["preview_token"]
	imported := must("POST", base+"/import", opts)
	require.Len(t, imported["applied"], 2)
	for _, target := range []string{root, shared[0]} {
		data, e := os.ReadFile(filepath.Join(target, "demo-recording", "scripts", "record.py"))
		require.NoError(t, e)
		require.Equal(t, script, string(data))
		info, e := os.Stat(filepath.Join(target, "demo-recording", "scripts", "record.py"))
		require.NoError(t, e)
		require.Zero(t, info.Mode().Perm()&0111)
	}
	require.Equal(t, 409, call("POST", base+"/import", opts).Code, "preview tokens are single use")
	opts = map[string]any{"skill_targets": targets, "preserve_executable": true}
	preview = must("POST", base+"/preview", opts)
	opts["preview_token"] = preview["preview_token"]
	require.Equal(t, 409, call("POST", base+"/import", opts).Code, "existing skill needs overwrite confirmation")
	opts["replace"] = true
	preview = must("POST", base+"/preview", opts)
	require.NoError(t, os.WriteFile(filepath.Join(source, "SKILL.md"), []byte("# locally changed"), 0600))
	opts["preview_token"] = preview["preview_token"]
	stale := call("POST", base+"/import", opts)
	require.Equal(t, 409, stale.Code, stale.Body.String())
	require.Contains(t, stale.Body.String(), "preview_changed")
	preview = must("POST", base+"/preview", opts)
	opts["preview_token"] = preview["preview_token"]
	must("POST", base+"/import", opts)
	for _, target := range []string{root, shared[0]} {
		info, e := os.Stat(filepath.Join(target, "demo-recording", "scripts", "record.py"))
		require.NoError(t, e)
		require.Equal(t, os.FileMode(0700), info.Mode().Perm())
	}
	require.NoError(t, os.Symlink("SKILL.md", filepath.Join(source, "alias")))
	bad := call("POST", "/"+board+"/items", map[string]any{"name": "Unsafe", "only": []string{"skills/demo-recording"}, "skill_root": "claude"})
	require.Equal(t, 502, bad.Code, bad.Body.String())
	require.Contains(t, bad.Body.String(), "symlinks")
}
