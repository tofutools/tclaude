package agentd

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// isolateSpawnAttachmentsBase repoints the per-batch upload base at a fresh
// t.TempDir() for the duration of one test and restores it on cleanup. This is
// cross-platform (unlike a $TMPDIR override, which Windows os.TempDir() ignores)
// so each test gets its own empty base dir — sibling tests can't leave batches
// that would trip the "no litter left behind" assertions.
func isolateSpawnAttachmentsBase(t *testing.T) {
	t.Helper()
	prev := spawnAttachmentsBase
	spawnAttachmentsBase = t.TempDir()
	t.Cleanup(func() { spawnAttachmentsBase = prev })
}

// uploadPart is one file in a synthetic multipart spawn-attachment upload.
type uploadPart struct {
	field    string // form field name; "" defaults to "file"
	filename string
	data     []byte
}

// newSpawnAttachUpload builds an authed multipart POST to the upload endpoint
// from the given parts. It mirrors dashboardRequest's cookie+Origin priming so
// checkDashboardAuth passes.
func newSpawnAttachUpload(t *testing.T, parts []uploadPart) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, p := range parts {
		field := p.field
		if field == "" {
			field = "file"
		}
		fw, err := mw.CreateFormFile(field, p.filename)
		require.NoError(t, err)
		_, err = fw.Write(p.data)
		require.NoError(t, err)
	}
	require.NoError(t, mw.Close())
	r := httptest.NewRequest(http.MethodPost, "/api/spawn-attachments", &buf)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	r.Header.Set("Origin", popupBaseURL)
	r.AddCookie(&http.Cookie{Name: dashboardCookieName, Value: dashboardSessionToken})
	return r
}

func TestSpawnAttachments_UploadHappyPath(t *testing.T) {
	withDashboardAuth(t)
	isolateSpawnAttachmentsBase(t)

	w := httptest.NewRecorder()
	r := newSpawnAttachUpload(t, []uploadPart{
		{filename: "notes.txt", data: []byte("hello brief")},
		{filename: "shot.png", data: []byte("\x89PNGfakebytes")},
	})
	handleDashboardSpawnAttachments(w, r)
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	var resp spawnAttachmentsResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Files, 2)
	assert.NotEmpty(t, resp.Token)
	assert.True(t, strings.HasPrefix(resp.Dir, spawnAttachmentsBaseDir()), "dir under the base")

	for _, f := range resp.Files {
		// Each returned path is inside the batch dir and holds the bytes we sent.
		assert.Equal(t, resp.Dir, filepath.Dir(f.Path), "file lives in the batch dir")
		got, err := os.ReadFile(f.Path)
		require.NoError(t, err, "stored file readable")
		assert.EqualValues(t, len(got), f.Size, "reported size matches bytes on disk")
	}
}

func TestSpawnAttachments_PathTraversalNameIsContained(t *testing.T) {
	withDashboardAuth(t)
	isolateSpawnAttachmentsBase(t)

	w := httptest.NewRecorder()
	r := newSpawnAttachUpload(t, []uploadPart{
		{filename: "../../etc/evil.txt", data: []byte("x")},
	})
	handleDashboardSpawnAttachments(w, r)
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	var resp spawnAttachmentsResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Files, 1)
	// The stored path must stay under the batch dir — never escape via "..".
	assert.Equal(t, resp.Dir, filepath.Dir(resp.Files[0].Path))
	assert.NotContains(t, resp.Files[0].Name, "..")
	assert.NotContains(t, resp.Files[0].Name, "/")
}

func TestSpawnAttachments_DedupesCollidingNames(t *testing.T) {
	withDashboardAuth(t)
	isolateSpawnAttachmentsBase(t)

	w := httptest.NewRecorder()
	r := newSpawnAttachUpload(t, []uploadPart{
		{filename: "shot.png", data: []byte("a")},
		{filename: "shot.png", data: []byte("bb")},
	})
	handleDashboardSpawnAttachments(w, r)
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	var resp spawnAttachmentsResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Files, 2)
	assert.NotEqual(t, resp.Files[0].Name, resp.Files[1].Name, "colliding names disambiguated")
	assert.Equal(t, "shot.png", resp.Files[0].Name)
	assert.Equal(t, "shot-1.png", resp.Files[1].Name)
}

func TestSpawnAttachments_RejectsEmptyUpload(t *testing.T) {
	withDashboardAuth(t)
	isolateSpawnAttachmentsBase(t)

	w := httptest.NewRecorder()
	r := newSpawnAttachUpload(t, nil) // no parts
	handleDashboardSpawnAttachments(w, r)
	assert.Equal(t, http.StatusBadRequest, w.Code, "an upload with no files is rejected")
}

func TestSpawnAttachments_RejectsTooManyFiles(t *testing.T) {
	withDashboardAuth(t)
	isolateSpawnAttachmentsBase(t)

	parts := make([]uploadPart, spawnAttachmentMaxFiles+1)
	for i := range parts {
		parts[i] = uploadPart{filename: "f.txt", data: []byte("x")}
	}
	w := httptest.NewRecorder()
	handleDashboardSpawnAttachments(w, newSpawnAttachUpload(t, parts))
	assert.Equal(t, http.StatusBadRequest, w.Code, "over the per-upload file cap is rejected")

	// And the batch dir is rolled back — no partial litter left behind.
	entries, err := os.ReadDir(spawnAttachmentsBaseDir())
	if err == nil {
		assert.Empty(t, entries, "rejected batch left no dir behind")
	}
}

func TestSpawnAttachments_RejectsNonPost(t *testing.T) {
	withDashboardAuth(t)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/spawn-attachments", nil)
	r.Header.Set("Origin", popupBaseURL)
	r.AddCookie(&http.Cookie{Name: dashboardCookieName, Value: dashboardSessionToken})
	handleDashboardSpawnAttachments(w, r)
	assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
}

func TestWriteSpawnAttachmentPart_PerFileCap(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "f.bin")
	// limit is min(remaining, perFileMax)=3; 4 bytes exceeds it.
	_, werr := writeSpawnAttachmentPart(dest, strings.NewReader("abcd"), 3, 100)
	require.NotNil(t, werr)
	assert.Equal(t, http.StatusBadRequest, werr.status)
	_, statErr := os.Stat(dest)
	assert.True(t, os.IsNotExist(statErr), "the over-cap partial file is removed")
}

func TestWriteSpawnAttachmentPart_TotalCap(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "f.bin")
	// remaining(2) is tighter than perFileMax(100); 3 bytes exceeds the batch.
	_, werr := writeSpawnAttachmentPart(dest, strings.NewReader("abc"), 100, 2)
	require.NotNil(t, werr)
	assert.Equal(t, http.StatusBadRequest, werr.status)
	assert.Contains(t, werr.msg, "total cap")
}

func TestWriteSpawnAttachmentPart_ExactlyAtCap(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "f.bin")
	n, werr := writeSpawnAttachmentPart(dest, strings.NewReader("abc"), 3, 100)
	require.Nil(t, werr, "exactly at the cap is accepted")
	assert.EqualValues(t, 3, n)
}

func TestSanitizeAttachmentFilename(t *testing.T) {
	cases := map[string]string{
		"clean.png":            "clean.png",
		"../../etc/passwd":     "passwd",
		`C:\Users\me\shot.png`: "shot.png",
		"with\x00ctrl.txt":     "withctrl.txt",
		"":                     "attachment",
		".":                    "attachment",
		"..":                   "attachment",
		"  spaced.txt  ":       "spaced.txt",
	}
	for in, want := range cases {
		assert.Equal(t, want, sanitizeAttachmentFilename(in), "sanitize %q", in)
	}
	// Long names are bounded but keep the extension.
	long := strings.Repeat("a", 300) + ".png"
	got := sanitizeAttachmentFilename(long)
	assert.LessOrEqual(t, len(got), 128)
	assert.True(t, strings.HasSuffix(got, ".png"), "extension preserved: %q", got)
}

func TestUniqueAttachmentName(t *testing.T) {
	used := map[string]bool{"shot.png": true, "shot-1.png": true}
	assert.Equal(t, "shot-2.png", uniqueAttachmentName("shot.png", used))
	assert.Equal(t, "fresh.txt", uniqueAttachmentName("fresh.txt", used))
}

func TestSanitizeSpawnAttachments(t *testing.T) {
	got, errMsg := sanitizeSpawnAttachments([]string{" /tmp/a.png ", "", "/tmp/b.txt"})
	require.Empty(t, errMsg)
	assert.Equal(t, []string{"/tmp/a.png", "/tmp/b.txt"}, got, "trimmed + blanks dropped")

	_, errMsg = sanitizeSpawnAttachments([]string{"/tmp/with\nnewline"})
	assert.NotEmpty(t, errMsg, "a control char (newline) in a path is rejected")

	too := make([]string, maxSpawnAttachments+1)
	for i := range too {
		too[i] = "/tmp/x"
	}
	_, errMsg = sanitizeSpawnAttachments(too)
	assert.NotEmpty(t, errMsg, "over the count cap is rejected")
}

func TestBuildSpawnAttachmentsSection(t *testing.T) {
	assert.Empty(t, buildSpawnAttachmentsSection(nil), "no attachments → no section")
	assert.Empty(t, buildSpawnAttachmentsSection([]string{"", "  "}), "only-blank → no section")

	s := buildSpawnAttachmentsSection([]string{"/tmp/a.png", "/tmp/b.txt"})
	assert.Contains(t, s, "Attached files")
	assert.Contains(t, s, "- /tmp/a.png")
	assert.Contains(t, s, "- /tmp/b.txt")
}

func TestBuildSpawnContextBody_IncludesAttachments(t *testing.T) {
	body := buildSpawnContextBody("team", "", "do the thing", []string{"/tmp/shot.png"})
	assert.Contains(t, body, "Your task brief:")
	assert.Contains(t, body, "do the thing")
	assert.Contains(t, body, "Attached files")
	assert.Contains(t, body, "/tmp/shot.png")

	// No attachments → no attachment section, brief unchanged.
	plain := buildSpawnContextBody("team", "", "do the thing", nil)
	assert.NotContains(t, plain, "Attached files")
}
