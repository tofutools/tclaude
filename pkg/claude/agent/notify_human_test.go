package agent

import (
	"archive/zip"
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

// A published artifact under a subject is a complete message on its own, so the
// body may be dropped there — and only there.
func TestNotifyHumanHasContent(t *testing.T) {
	assert.True(t, notifyHumanHasContent("status update", &notifyHumanParams{}),
		"a plain body needs nothing else")
	assert.True(t, notifyHumanHasContent("  ", &notifyHumanParams{
		Subject: "screenshot", Attach: []string{"shot.png"}}),
		"a subject plus an attachment stands in for the body")
	assert.False(t, notifyHumanHasContent("  ", &notifyHumanParams{Subject: "screenshot"}),
		"a subject with nothing published is a headline over an empty page")
	assert.False(t, notifyHumanHasContent("", &notifyHumanParams{Attach: []string{"shot.png"}}),
		"an unlabelled attachment says nothing about what arrived")
	assert.False(t, notifyHumanHasContent("", &notifyHumanParams{
		Subject: "   ", Attach: []string{"shot.png"}}),
		"a blank subject is no subject")
}

// The bodiless form is refused when it is missing the subject or the attachment
// that justify it, before the CLI ever reaches the daemon.
func TestRunNotifyHuman_RejectsBodilessWithoutSubjectAndAttachment(t *testing.T) {
	var stdout, stderr bytes.Buffer
	rc := runNotifyHuman(&notifyHumanParams{Subject: "screenshot"},
		new(bytes.Buffer), &stdout, &stderr)
	assert.Equal(t, rcInvalidArg, rc)
	assert.Contains(t, stderr.String(), "a notification body is required")
	assert.Empty(t, stdout.String())
}

// Two files sharing a base name must both survive the upload.
func TestBuildMultipartAttachments_DisambiguatesNameCollisions(t *testing.T) {
	dirA, dirB := t.TempDir(), t.TempDir()
	a := filepath.Join(dirA, "shot.png")
	b := filepath.Join(dirB, "shot.png")
	require.NoError(t, os.WriteFile(a, []byte("first"), 0o644))
	require.NoError(t, os.WriteFile(b, []byte("second"), 0o644))

	var stderr bytes.Buffer
	data, contentType, tooLarge, rc := buildMultipartAttachments([]string{a, b}, &stderr)
	require.Equal(t, rcOK, rc, stderr.String())
	require.False(t, tooLarge)
	parts := readParts(t, data, contentType)
	assert.Equal(t, map[string]string{"shot.png": "first", "shot-1.png": "second"}, parts)
}

// Separate attachments travel uncompressed, so a set that fits the limit as an
// archive can overflow it. The body must stop growing at the cap instead of
// buffering every file without bound, and auto mode must package the set
// rather than refusing it.
func TestBuildNotifyHumanPayload_OversizeSetFallsBackToZip(t *testing.T) {
	files := writeAttachFiles(t, 3)
	restore := maxSeparateAttachmentBytes
	maxSeparateAttachmentBytes = 64 // smaller than three parts plus their headers
	t.Cleanup(func() { maxSeparateAttachmentBytes = restore })

	var stderr bytes.Buffer
	data, contentType, tooLarge, rc := buildMultipartAttachments(files, &stderr)
	require.Equal(t, rcOK, rc, stderr.String())
	assert.True(t, tooLarge, "the assembled body is refused at the cap, not grown past it")
	assert.Nil(t, data)
	assert.Empty(t, contentType)

	stderr.Reset()
	_, name, contentType, _, rc := buildNotifyHumanPayload(
		&notifyHumanParams{Attach: files}, notifyAttachAuto, &stderr)
	require.Equal(t, rcOK, rc, stderr.String())
	assert.Equal(t, "export.zip", name, "auto mode packages what will not fit separately")
	assert.Equal(t, "application/zip", contentType)

	// With --separate there is no fallback, so the agent is told to use --zip.
	stderr.Reset()
	_, _, _, _, rc = buildNotifyHumanPayload(
		&notifyHumanParams{Attach: files}, notifyAttachSeparate, &stderr)
	assert.Equal(t, rcInvalidArg, rc)
	assert.Contains(t, stderr.String(), "use --zip")
}

// --zip promises an archive, so it must produce one even for a lone file.
func TestBuildNotifyHumanPayload_ZipHonoursSingleFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "report.md")
	require.NoError(t, os.WriteFile(p, []byte("# hi\n"), 0o644))

	var stderr bytes.Buffer
	data, name, contentType, _, rc := buildNotifyHumanPayload(
		&notifyHumanParams{Attach: []string{p}, Zip: true}, notifyAttachZip, &stderr)
	require.Equal(t, rcOK, rc, stderr.String())
	assert.Equal(t, "report.zip", name)
	assert.Equal(t, "application/zip", contentType)
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	require.NoError(t, err)
	require.Len(t, zr.File, 1)
	assert.Equal(t, "report.md", zr.File[0].Name)
}
