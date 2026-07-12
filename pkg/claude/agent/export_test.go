package agent

import (
	"archive/zip"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildExportArtifact_SingleFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "summary.md")
	require.NoError(t, os.WriteFile(p, []byte("# hi\n"), 0o644))

	var stderr bytes.Buffer
	data, name, ct, rc := buildExportArtifact([]string{p}, "", &stderr)
	require.Equal(t, rcOK, rc, stderr.String())
	assert.Equal(t, []byte("# hi\n"), data, "single file passes through verbatim")
	assert.Equal(t, "summary.md", name)
	assert.Contains(t, ct, "text/markdown")
}

func TestBuildExportArtifact_SingleFileNameOverride(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "raw.txt")
	require.NoError(t, os.WriteFile(p, []byte("x"), 0o644))

	var stderr bytes.Buffer
	_, name, _, rc := buildExportArtifact([]string{p}, "renamed.txt", &stderr)
	require.Equal(t, rcOK, rc, stderr.String())
	assert.Equal(t, "renamed.txt", name)
}

func TestBuildExportArtifact_MultipleFilesZipped(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.md")
	b := filepath.Join(dir, "b.csv")
	require.NoError(t, os.WriteFile(a, []byte("aaa"), 0o644))
	require.NoError(t, os.WriteFile(b, []byte("b,b"), 0o644))

	var stderr bytes.Buffer
	data, name, ct, rc := buildExportArtifact([]string{a, b}, "", &stderr)
	require.Equal(t, rcOK, rc, stderr.String())
	assert.Equal(t, "export.zip", name)
	assert.Equal(t, "application/zip", ct)

	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	require.NoError(t, err)
	got := map[string]string{}
	for _, f := range zr.File {
		rc, err := f.Open()
		require.NoError(t, err)
		buf, err := io.ReadAll(rc)
		require.NoError(t, err)
		_ = rc.Close()
		got[f.Name] = string(buf)
	}
	assert.Equal(t, map[string]string{"a.md": "aaa", "b.csv": "b,b"}, got)
}

func TestBuildExportArtifact_MissingFile(t *testing.T) {
	var stderr bytes.Buffer
	_, _, _, rc := buildExportArtifact([]string{"/no/such/file"}, "", &stderr)
	assert.Equal(t, rcInvalidArg, rc)
	assert.NotEmpty(t, stderr.String())
}

func TestBuildExportArtifact_DirectoryPackaged(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "result.txt"), []byte("done"), 0o644))
	var stderr bytes.Buffer
	data, name, contentType, rc := buildExportArtifact([]string{dir}, "", &stderr)
	require.Equal(t, rcOK, rc, stderr.String())
	assert.Equal(t, filepath.Base(dir)+".zip", name)
	assert.Equal(t, "application/zip", contentType)
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	require.NoError(t, err)
	require.Len(t, zr.File, 1)
	assert.Equal(t, filepath.Base(dir)+"/result.txt", zr.File[0].Name)
}

func TestZipFiles_DuplicateBaseNamesDisambiguated(t *testing.T) {
	d1 := t.TempDir()
	d2 := t.TempDir()
	p1 := filepath.Join(d1, "report.md")
	p2 := filepath.Join(d2, "report.md")
	require.NoError(t, os.WriteFile(p1, []byte("one"), 0o644))
	require.NoError(t, os.WriteFile(p2, []byte("two"), 0o644))

	data, err := zipFiles([]string{p1, p2})
	require.NoError(t, err)
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	require.NoError(t, err)
	require.Len(t, zr.File, 2, "both files present despite shared base name")
	names := []string{zr.File[0].Name, zr.File[1].Name}
	assert.Contains(t, names, "report.md")
	assert.Contains(t, names, "report-1.md", "collision gets a numeric suffix")
}

func TestContentTypeForName(t *testing.T) {
	assert.Contains(t, contentTypeForName("x.md"), "text/markdown")
	assert.Contains(t, contentTypeForName("x.txt"), "text/plain")
	assert.Equal(t, "application/zip", contentTypeForName("x.zip"))
	assert.Equal(t, "application/octet-stream", contentTypeForName("x.unknownext"))
}

func TestParseExportJobID(t *testing.T) {
	var stderr bytes.Buffer
	id, rc := parseExportJobID("42", &stderr)
	assert.Equal(t, rcOK, rc)
	assert.Equal(t, int64(42), id)

	_, rc = parseExportJobID("nope", &stderr)
	assert.Equal(t, rcInvalidArg, rc)
	_, rc = parseExportJobID("0", &stderr)
	assert.Equal(t, rcInvalidArg, rc)
}
