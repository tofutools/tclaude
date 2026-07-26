package agentipc

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSpoolRequestRoundTrip(t *testing.T) {
	in := SpoolRequest{
		Method:     http.MethodPost,
		RequestURI: "/v1/messages?limit=3",
		Header:     http.Header{"Idempotency-Key": []string{"abc"}, "Content-Type": []string{"application/json"}},
		Body:       []byte(`{"to":"peer","body":"hi"}`),
	}
	data, err := EncodeSpoolRequest(in)
	require.NoError(t, err)
	out, err := DecodeSpoolRequest(data)
	require.NoError(t, err)
	assert.Equal(t, in, out)
}

func TestSpoolResponseRoundTrip(t *testing.T) {
	in := SpoolResponse{
		Status: http.StatusConflict,
		Header: http.Header{"Content-Type": []string{"application/json"}},
		Body:   []byte(`{"error":"nope"}`),
	}
	data, err := EncodeSpoolResponse(in)
	require.NoError(t, err)
	out, err := DecodeSpoolResponse(data)
	require.NoError(t, err)
	assert.Equal(t, in, out)
}

func TestWriteSpoolFilePublishesCompleteAndLeavesNoTmp(t *testing.T) {
	dir := t.TempDir()
	path := SpoolEnvelopePath(dir, "req-1")
	require.NoError(t, WriteSpoolFile(path, []byte("payload")))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "payload", string(data))

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "tmp files must not survive a successful publish")
	assert.Equal(t, "req-1.json", entries[0].Name())
}

func TestSpoolEnvelopeFileIgnoresTmpAndClaimFiles(t *testing.T) {
	assert.True(t, SpoolEnvelopeFile("0123abcd.json"))
	assert.False(t, SpoolEnvelopeFile(".0123abcd.json.tmp-42"), "in-flight tmp writes are hidden")
	assert.False(t, SpoolEnvelopeFile(".0123abcd.json.work"), "daemon claim files are hidden")
	assert.False(t, SpoolEnvelopeFile("notes.txt"))
}

func TestSpoolDirFromEnvRejectsRelativePaths(t *testing.T) {
	t.Setenv(SpoolEnv, "relative/spool")
	assert.Empty(t, SpoolDirFromEnv(), "a relative spool dir must not resolve against ambient CWD")

	abs := filepath.Join(t.TempDir(), "spool-x")
	t.Setenv(SpoolEnv, abs)
	assert.Equal(t, abs, SpoolDirFromEnv())
}

func TestNewSpoolIDIsUnique(t *testing.T) {
	a, err := NewSpoolID()
	require.NoError(t, err)
	b, err := NewSpoolID()
	require.NoError(t, err)
	assert.Len(t, a, 32)
	assert.NotEqual(t, a, b)
}
