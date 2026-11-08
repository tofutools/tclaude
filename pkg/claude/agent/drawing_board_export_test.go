package agent

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDrawingBoardExportWritesBoardNamedFileByDefault(t *testing.T) {
	prev := DaemonPostRawBytesImpl
	t.Cleanup(func() { DaemonPostRawBytesImpl = prev })
	DaemonPostRawBytesImpl = func(string, string, []byte, http.Header, DaemonOpts) ([]byte, http.Header, error) {
		return []byte("img"), http.Header{}, nil
	}
	t.Chdir(t.TempDir())
	var stdout, stderr bytes.Buffer
	require.Equal(t, rcOK, runDrawingBoardExport(&drawingBoardExportParams{Format: "svg"}, "plan", &stdout, &stderr), stderr.String())
	data, err := os.ReadFile("plan.svg")
	require.NoError(t, err)
	assert.Equal(t, "img", string(data))
}

func TestDrawingBoardExportFormatFollowsTheFile(t *testing.T) {
	for _, c := range []struct{ format, output, want string }{
		{"", "plan.png", "png"}, {"", "plan.SVG", "svg"}, {"", "-", "png"}, {"svg", "-", "svg"}, {"", "plan.out", "png"},
	} {
		got, err := exportFormat(c.format, c.output)
		require.NoError(t, err, c)
		assert.Equal(t, c.want, got, c)
	}
	_, err := exportFormat("svg", "plan.png")
	assert.Error(t, err, "a format that contradicts the file is refused")
	_, err = exportFormat("gif", "-")
	assert.Error(t, err)
}

func TestDrawingBoardExportSendsThePartAndWritesTheImage(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	prev := DaemonPostRawBytesImpl
	t.Cleanup(func() { DaemonPostRawBytesImpl = prev })
	DaemonPostRawBytesImpl = func(path, _ string, body []byte, _ http.Header, _ DaemonOpts) ([]byte, http.Header, error) {
		gotPath = path
		gotBody = nil
		require.NoError(t, json.Unmarshal(body, &gotBody))
		return []byte("<svg/>"), http.Header{"X-Drawing-Board-Version": {"7"}, "X-Drawing-Board-Export-Elements": {"3"}}, nil
	}
	out := filepath.Join(t.TempDir(), "part.svg")
	var stdout, stderr bytes.Buffer
	rc := runDrawingBoardExport(&drawingBoardExportParams{Output: out, Elements: []string{"api,db", "cache"}}, "plan", &stdout, &stderr)
	require.Equal(t, rcOK, rc, stderr.String())
	assert.Equal(t, "/v1/drawing-boards/plan/export", gotPath)
	assert.Equal(t, map[string]any{"format": "svg", "elements": []any{"api", "db", "cache"}}, gotBody)
	data, err := os.ReadFile(out)
	require.NoError(t, err)
	assert.Equal(t, "<svg/>", string(data))
	assert.Contains(t, stdout.String(), "3 element(s) at version 7")

	stdout.Reset()
	rc = runDrawingBoardExport(&drawingBoardExportParams{Output: "-", Region: "0, 10, 400, 300", Scale: 1}, "plan", &stdout, &stderr)
	require.Equal(t, rcOK, rc, stderr.String())
	assert.Equal(t, map[string]any{"format": "png", "scale": 1.0, "region": map[string]any{"x": 0.0, "y": 10.0, "width": 400.0, "height": 300.0}}, gotBody)
	assert.Equal(t, "<svg/>", stdout.String(), "-o - writes the image itself to stdout")

	for _, bad := range []drawingBoardExportParams{
		{Output: "a.png", Region: "1,2,3"},
		{Output: "a.png", Region: "0,0,0,5"},
		{Output: "a.png", Region: "0,0,5,5", Elements: []string{"api"}},
		{Output: "a.svg", Scale: 2},
	} {
		assert.Equal(t, rcInvalidArg, runDrawingBoardExport(&bad, "plan", &stdout, &stderr), "%+v", bad)
	}
}

func TestDrawingBoardExportAttachesToATask(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	prev := DaemonRequestImpl
	t.Cleanup(func() { DaemonRequestImpl = prev })
	DaemonRequestImpl = func(method, path string, in, out any, _ DaemonOpts) error {
		gotPath = method + " " + path
		raw, _ := json.Marshal(in)
		gotBody = nil
		require.NoError(t, json.Unmarshal(raw, &gotBody))
		return json.Unmarshal([]byte(`{"id":"tk_1","replaced":true,"attachment":{"id":4,"name":"plan.png","size":99,"drawing":{"version":7}}}`), out)
	}
	var stdout, stderr bytes.Buffer
	rc := runDrawingBoardExport(&drawingBoardExportParams{AttachTo: "tk_1", Elements: []string{"api"}, Name: "api"}, "plan", &stdout, &stderr)
	require.Equal(t, rcOK, rc, stderr.String())
	assert.Equal(t, "POST /v1/taskboard/tasks/tk_1/drawings", gotPath)
	assert.Equal(t, map[string]any{"board": "plan", "elements": []any{"api"}, "name": "api"}, gotBody)
	assert.Contains(t, stdout.String(), "Updated plan.png (file 4) on tk_1: drawing board plan at version 7")

	for _, bad := range []drawingBoardExportParams{
		{AttachTo: "tk_1", Output: "x.png"},
		{AttachTo: "tk_1", Format: "svg"},
		{Output: "x.png", Comment: "see"},
		{Output: "x.png", Replace: 3},
	} {
		assert.Equal(t, rcInvalidArg, runDrawingBoardExport(&bad, "plan", &stdout, &stderr), "%+v", bad)
	}
}
