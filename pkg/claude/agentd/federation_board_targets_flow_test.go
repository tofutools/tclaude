package agentd_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/skillroots"
	"github.com/tofutools/tclaude/pkg/testharness"
	"github.com/tofutools/tclaude/pkg/testutil"
)

func TestDashboardBoardTargetsArchiveAndCatalog(t *testing.T) {
	fh := newFedHarness(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://localhost:12345"))
	dash := agentd.BuildDashboardHandlerForTest()
	call := func(method, tail string, body any) *httptest.ResponseRecorder {
		t.Helper()
		return testharness.Serve(dash, testharness.JSONRequest(t, method, "/api/federation/boards"+tail, body))
	}
	must := func(method, tail string, body any) map[string]any {
		t.Helper()
		r := call(method, tail, body)
		require.Equal(t, 200, r.Code, r.Body.String())
		var out map[string]any
		testharness.DecodeJSON(t, r, &out)
		return out
	}
	root, e := skillroots.Claude()
	require.NoError(t, e)
	source := filepath.Join(root, "demo")
	require.NoError(t, os.MkdirAll(source, 0700))
	markdown := "---\nname: demo\ndescription: Record a demo safely.\n---\n# Demo\n"
	require.NoError(t, os.WriteFile(filepath.Join(source, "SKILL.md"), []byte(markdown), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(source, "run.sh"), []byte("#!/bin/sh\necho demo\n"), 0700))
	board := must("POST", "", map[string]any{"name": "library"})["id"].(string)
	publication := must("POST", "/"+board+"/items", map[string]any{"name": "Demo", "only": []string{"skills/demo"}, "skill_root": "claude"})
	item, version := publication["item"].(string), publication["version"].(string)
	base := "/" + board + "/items/" + item + "/versions/" + version
	require.Equal(t, 409, call("GET", base+"/archive", nil).Code, "no unverified archive bytes")
	must("POST", base+"/fetch", map[string]any{})
	rows := must("GET", "/"+board+"/items", nil)["items"].([]any)
	row := rows[0].(map[string]any)
	require.Equal(t, "Record a demo safely.", row["description"])
	require.Equal(t, float64(1), row["seq"])
	posted, e := time.Parse(time.RFC3339Nano, row["updated_at"].(string))
	require.NoError(t, e)
	require.Equal(t, time.UTC, posted.Location())
	groupDir := testutil.CanonicalTempDir(t)
	_, e = db.CreateAgentGroup("skill-team", "")
	require.NoError(t, e)
	_, e = db.SetAgentGroupDefaultCwd("skill-team", groupDir)
	require.NoError(t, e)
	custom := testutil.CanonicalTempDir(t)
	opts := map[string]any{"skill_targets": []string{"group:skill-team", "custom"}, "custom_dir": custom}
	preview := must("POST", base+"/preview", opts)
	changes := preview["changes"].([]any)
	require.Len(t, changes, 2)
	require.Equal(t, filepath.Join(groupDir, ".claude", "skills", "demo"), changes[0].(map[string]any)["path"])
	require.Equal(t, filepath.Join(custom, "demo"), changes[1].(map[string]any)["path"])
	require.NoDirExists(t, filepath.Join(groupDir, ".claude"), "preview doesn't create project roots")
	opts["preview_token"] = preview["preview_token"]
	must("POST", base+"/import", opts)
	for _, dest := range []string{filepath.Join(groupDir, ".claude", "skills"), custom} {
		content, e := os.ReadFile(filepath.Join(dest, "demo", "SKILL.md"))
		require.NoError(t, e)
		require.Equal(t, markdown, string(content))
		info, e := os.Stat(filepath.Join(dest, "demo", "run.sh"))
		require.NoError(t, e)
		require.Zero(t, info.Mode().Perm()&0111)
	}
	// Group directory changes invalidate the preview, even before file content changes.
	opts = map[string]any{"skill_targets": []string{"group:skill-team"}}
	preview = must("POST", base+"/preview", opts)
	_, e = db.SetAgentGroupDefaultCwd("skill-team", testutil.CanonicalTempDir(t))
	require.NoError(t, e)
	opts["preview_token"] = preview["preview_token"]
	require.Equal(t, 409, call("POST", base+"/import", opts).Code)
	for _, bad := range []string{"relative", filepath.Join(custom, "missing"), custom + "/../" + filepath.Base(custom)} {
		r := call("POST", base+"/preview", map[string]any{"skill_targets": []string{"custom"}, "custom_dir": bad})
		require.Equal(t, 400, r.Code, r.Body.String())
		require.Contains(t, r.Body.String(), "custom_dir")
	}
	alias := filepath.Join(testutil.CanonicalTempDir(t), "alias")
	require.NoError(t, os.Symlink(custom, alias))
	require.Equal(t, 400, call("POST", base+"/preview", map[string]any{"skill_targets": []string{"custom"}, "custom_dir": alias}).Code)
	require.Equal(t, 400, call("POST", base+"/preview", map[string]any{"skill_targets": []string{"custom"}, "custom_dir": filepath.Join(alias, "demo")}).Code, "intermediate symlinks refused too")
	// Empty replacement at the same path must not reuse the old root's permit.
	fresh := filepath.Join(testutil.CanonicalTempDir(t), "root")
	require.NoError(t, os.Mkdir(fresh, 0700))
	opts = map[string]any{"skill_targets": []string{"custom"}, "custom_dir": fresh}
	preview = must("POST", base+"/preview", opts)
	require.NoError(t, os.Rename(fresh, fresh+"-old"))
	require.NoError(t, os.Mkdir(fresh, 0700))
	opts["preview_token"] = preview["preview_token"]
	require.Equal(t, 409, call("POST", base+"/import", opts).Code)
	require.NoDirExists(t, filepath.Join(fresh, "demo"))
	agentRequest := agentd.AsAgentPeer(testharness.JSONRequest(t, "POST", "/v1/federation/boards"+base+"/preview", map[string]any{"custom_dir": custom, "skill_targets": []string{"custom"}}), "agent")
	require.Equal(t, 403, testharness.Serve(fh.f.Mux, agentRequest).Code, "agents cannot select custom directories")
	for _, exec := range []bool{false, true} {
		path := base + "/archive"
		if exec {
			path += "?preserve_executable=1"
		}
		response := call("GET", path, nil)
		require.Equal(t, 200, response.Code, response.Body.String())
		require.Equal(t, "application/gzip", response.Header().Get("Content-Type"))
		require.Contains(t, response.Header().Get("Content-Disposition"), "demo.tar.gz")
		gz, e := gzip.NewReader(bytes.NewReader(response.Body.Bytes()))
		require.NoError(t, e)
		tr := tar.NewReader(gz)
		seen := map[string]bool{}
		for {
			header, e := tr.Next()
			if e == io.EOF {
				break
			}
			require.NoError(t, e)
			seen[header.Name] = true
			require.Equal(t, byte(tar.TypeReg), header.Typeflag)
			data, e := io.ReadAll(tr)
			require.NoError(t, e)
			if header.Name == "demo/SKILL.md" {
				require.Equal(t, markdown, string(data))
			}
			if header.Name == "demo/run.sh" {
				if exec {
					require.Equal(t, int64(0700), header.Mode)
				} else {
					require.Equal(t, int64(0600), header.Mode)
				}
			}
		}
		require.NoError(t, gz.Close())
		require.Len(t, seen, 2)
	}
	// Updating increments the hub sequence; description follows signed source metadata.
	updated := must("POST", "/"+board+"/items", map[string]any{"name": "Demo", "item": item, "parent": version, "only": []string{"skills/demo"}, "skill_root": "claude"})
	row = must("GET", "/"+board+"/items", nil)["items"].([]any)[0].(map[string]any)
	require.Equal(t, float64(2), row["seq"])
	require.Equal(t, updated["version"], row["version"])
	// Re-publication preserves the optional publisher description.
	copiedBoard := must("POST", "", map[string]any{"name": "copy"})["id"].(string)
	must("POST", "/"+copiedBoard+"/items", map[string]any{"source": map[string]any{"board": board, "item": item, "version": version}})
	copy := must("GET", "/"+copiedBoard+"/items", nil)["items"].([]any)[0].(map[string]any)
	require.Equal(t, "Record a demo safely.", copy["description"])
	_, e = db.CreateRole(&db.Role{Name: "archive-role", Brief: "Review carefully."})
	require.NoError(t, e)
	config := must("POST", "/"+board+"/items", map[string]any{"name": "Role", "only": []string{"roles/archive-role"}})
	configBase := "/" + board + "/items/" + config["item"].(string) + "/versions/" + config["version"].(string)
	must("POST", configBase+"/fetch", map[string]any{})
	response := call("GET", configBase+"/archive", nil)
	require.Equal(t, 200, response.Code)
	gz, e := gzip.NewReader(bytes.NewReader(response.Body.Bytes()))
	require.NoError(t, e)
	reader := tar.NewReader(gz)
	header, e := reader.Next()
	require.NoError(t, e)
	require.Equal(t, "bundle.json", header.Name)
	data, e := io.ReadAll(reader)
	require.NoError(t, e)
	require.Contains(t, string(data), "archive-role")
	_, e = reader.Next()
	require.Equal(t, io.EOF, e)
	require.NoError(t, gz.Close())
}
