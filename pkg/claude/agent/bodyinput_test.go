package agent

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// With no --file, the inline value is passed straight through — the
// behaviour every command had before --file existed.
func TestResolveBodyInput_NoFileReturnsInline(t *testing.T) {
	stderr := new(bytes.Buffer)
	got, rc := resolveBodyInput("inline text", "", "--initial-message", new(bytes.Buffer), stderr)
	require.Equal(t, rcOK, rc, "stderr=%s", stderr.String())
	assert.Equal(t, "inline text", got)
	assert.Empty(t, stderr.String())
}

// Neither inline nor --file given → empty string, no error. Callers
// that require a body do their own emptiness check afterwards.
func TestResolveBodyInput_NeitherGivenIsEmpty(t *testing.T) {
	stderr := new(bytes.Buffer)
	got, rc := resolveBodyInput("", "", "--body", new(bytes.Buffer), stderr)
	require.Equal(t, rcOK, rc, "stderr=%s", stderr.String())
	assert.Empty(t, got)
	assert.Empty(t, stderr.String())
}

// --file reads the file content verbatim, backticks and newlines and
// all — that is the whole point of the feature.
func TestResolveBodyInput_ReadsFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "brief.txt")
	const content = "line one\nline two with `backticks`\n\tindented\n"
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	stderr := new(bytes.Buffer)
	got, rc := resolveBodyInput("", path, "--initial-message", new(bytes.Buffer), stderr)
	require.Equal(t, rcOK, rc, "stderr=%s", stderr.String())
	assert.Equal(t, content, got, "file content must be returned verbatim")
}

// A --file path of "-" reads from stdin, so a brief can be piped in.
func TestResolveBodyInput_DashReadsStdin(t *testing.T) {
	stderr := new(bytes.Buffer)
	got, rc := resolveBodyInput("", "-", "--body", bytes.NewBufferString("piped body"), stderr)
	require.Equal(t, rcOK, rc, "stderr=%s", stderr.String())
	assert.Equal(t, "piped body", got)
}

// Passing both the inline value and --file is a usage error — the two
// sources are mutually exclusive.
func TestResolveBodyInput_MutualExclusion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "brief.txt")
	require.NoError(t, os.WriteFile(path, []byte("from file"), 0o600))

	stderr := new(bytes.Buffer)
	got, rc := resolveBodyInput("inline text", path, "--initial-message", new(bytes.Buffer), stderr)
	assert.Equal(t, rcInvalidArg, rc)
	assert.Empty(t, got)
	assert.Contains(t, stderr.String(), "--initial-message")
	assert.Contains(t, stderr.String(), "not both")
}

// A whitespace-only inline value still counts as "given" for the
// mutual-exclusion check, so `--initial-message "  " --file x` fails
// rather than silently picking the file.
func TestResolveBodyInput_MutualExclusionWhitespaceInline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "brief.txt")
	require.NoError(t, os.WriteFile(path, []byte("from file"), 0o600))

	stderr := new(bytes.Buffer)
	_, rc := resolveBodyInput("   ", path, "--body", new(bytes.Buffer), stderr)
	assert.Equal(t, rcInvalidArg, rc)
	assert.Contains(t, stderr.String(), "not both")
}

// A missing / unreadable --file is a clear error, named, before the
// caller does any work.
func TestResolveBodyInput_MissingFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist.txt")
	stderr := new(bytes.Buffer)
	got, rc := resolveBodyInput("", missing, "--body", new(bytes.Buffer), stderr)
	assert.Equal(t, rcIOFailure, rc)
	assert.Empty(t, got)
	assert.Contains(t, stderr.String(), missing, "error must name the unreadable file")
}
