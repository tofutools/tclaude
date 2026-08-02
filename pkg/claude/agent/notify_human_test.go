package agent

import (
	"bytes"
	"io"
	"mime"
	"mime/multipart"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeAttachFiles(t *testing.T, n int) []string {
	t.Helper()
	dir := t.TempDir()
	paths := make([]string, 0, n)
	for i := range n {
		p := filepath.Join(dir, string(rune('a'+i))+".png")
		require.NoError(t, os.WriteFile(p, []byte("pixels"), 0o644))
		paths = append(paths, p)
	}
	return paths
}

// readParts decodes a built multipart body into filename → content.
func readParts(t *testing.T, data []byte, contentType string) map[string]string {
	t.Helper()
	_, params, err := mime.ParseMediaType(contentType)
	require.NoError(t, err)
	reader := multipart.NewReader(bytes.NewReader(data), params["boundary"])
	out := map[string]string{}
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		body, err := io.ReadAll(part)
		require.NoError(t, err)
		out[part.FileName()] = string(body)
	}
	return out
}

// A handful of attached files must reach the human as separate downloads — the
// whole point of not auto-zipping: an attached image stays viewable.
func TestBuildNotifyHumanPayload_FewFilesStaySeparate(t *testing.T) {
	files := writeAttachFiles(t, 3)
	var stderr bytes.Buffer
	data, name, contentType, summary, rc := buildNotifyHumanPayload(
		&notifyHumanParams{Attach: files}, notifyAttachAuto, &stderr)
	require.Equal(t, rcOK, rc, stderr.String())
	assert.Empty(t, name, "separate attachments keep their own names")
	assert.True(t, strings.HasPrefix(contentType, "multipart/form-data"), contentType)
	assert.Contains(t, summary, "3 files")
	assert.Len(t, readParts(t, data, contentType), 3)
}

// One file keeps the original raw-body shape.
func TestBuildNotifyHumanPayload_SingleFileUnchanged(t *testing.T) {
	files := writeAttachFiles(t, 1)
	var stderr bytes.Buffer
	data, name, contentType, _, rc := buildNotifyHumanPayload(
		&notifyHumanParams{Attach: files}, notifyAttachAuto, &stderr)
	require.Equal(t, rcOK, rc, stderr.String())
	assert.Equal(t, "a.png", name)
	assert.Equal(t, "image/png", contentType)
	assert.Equal(t, "pixels", string(data))
}

// Above the threshold a message would become a file listing, so the set is
// packaged automatically.
func TestBuildNotifyHumanPayload_LargeSetAutoZips(t *testing.T) {
	files := writeAttachFiles(t, notifyHumanAutoZipFileCount+1)
	var stderr bytes.Buffer
	_, name, contentType, _, rc := buildNotifyHumanPayload(
		&notifyHumanParams{Attach: files}, notifyAttachAuto, &stderr)
	require.Equal(t, rcOK, rc, stderr.String())
	assert.Equal(t, "export.zip", name)
	assert.Equal(t, "application/zip", contentType)
}

// A directory is always zipped: separate attachments would flatten its layout.
func TestBuildNotifyHumanPayload_DirectoryZips(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "shots")
	require.NoError(t, os.MkdirAll(nested, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(nested, "one.png"), []byte("x"), 0o644))
	var stderr bytes.Buffer
	_, name, contentType, _, rc := buildNotifyHumanPayload(
		&notifyHumanParams{Attach: []string{nested}}, notifyAttachAuto, &stderr)
	require.Equal(t, rcOK, rc, stderr.String())
	assert.Equal(t, "shots.zip", name)
	assert.Equal(t, "application/zip", contentType)
}

func TestBuildNotifyHumanPayload_ZipAndSeparateOverrides(t *testing.T) {
	files := writeAttachFiles(t, 2)
	var stderr bytes.Buffer
	_, name, contentType, _, rc := buildNotifyHumanPayload(
		&notifyHumanParams{Attach: files}, notifyAttachZip, &stderr)
	require.Equal(t, rcOK, rc, stderr.String())
	assert.Equal(t, "export.zip", name)
	assert.Equal(t, "application/zip", contentType)

	stderr.Reset()
	single := writeAttachFiles(t, 1)
	_, _, contentType, _, rc = buildNotifyHumanPayload(
		&notifyHumanParams{Attach: single}, notifyAttachSeparate, &stderr)
	require.Equal(t, rcOK, rc, stderr.String())
	assert.True(t, strings.HasPrefix(contentType, "multipart/form-data"),
		"--separate publishes even one file as its own attachment")

	stderr.Reset()
	dir := t.TempDir()
	_, _, _, _, rc = buildNotifyHumanPayload(
		&notifyHumanParams{Attach: []string{dir}}, notifyAttachSeparate, &stderr)
	assert.Equal(t, rcInvalidArg, rc)
	assert.Contains(t, stderr.String(), "cannot publish a directory")
}

func TestResolveNotifyHumanAttachMode(t *testing.T) {
	var stderr bytes.Buffer
	_, rc := resolveNotifyHumanAttachMode(&notifyHumanParams{Zip: true, Separate: true}, &stderr)
	assert.Equal(t, rcInvalidArg, rc)
	assert.Contains(t, stderr.String(), "mutually exclusive")

	stderr.Reset()
	mode, rc := resolveNotifyHumanAttachMode(
		&notifyHumanParams{Name: "bundle.zip", Attach: []string{"a", "b"}}, &stderr)
	require.Equal(t, rcOK, rc, stderr.String())
	assert.Equal(t, notifyAttachZip, mode, "naming the download selects one artifact")

	stderr.Reset()
	_, rc = resolveNotifyHumanAttachMode(
		&notifyHumanParams{Name: "one.png", Separate: true, Attach: []string{"a"}}, &stderr)
	assert.Equal(t, rcInvalidArg, rc)
}

// Two files sharing a base name must both survive the upload.
func TestBuildMultipartAttachments_DisambiguatesNameCollisions(t *testing.T) {
	dirA, dirB := t.TempDir(), t.TempDir()
	a := filepath.Join(dirA, "shot.png")
	b := filepath.Join(dirB, "shot.png")
	require.NoError(t, os.WriteFile(a, []byte("first"), 0o644))
	require.NoError(t, os.WriteFile(b, []byte("second"), 0o644))

	var stderr bytes.Buffer
	data, contentType, rc := buildMultipartAttachments([]string{a, b}, &stderr)
	require.Equal(t, rcOK, rc, stderr.String())
	parts := readParts(t, data, contentType)
	assert.Equal(t, map[string]string{"shot.png": "first", "shot-1.png": "second"}, parts)
}
