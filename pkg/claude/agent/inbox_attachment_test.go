package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/testutil"
)

func stubAttachments(t *testing.T) {
	t.Helper()
	prevAvail, prevReq, prevStream := DaemonAvailableImpl, DaemonRequestImpl, daemonAttachmentStream
	t.Cleanup(func() {
		DaemonAvailableImpl, DaemonRequestImpl, daemonAttachmentStream = prevAvail, prevReq, prevStream
	})
	DaemonAvailableImpl = func() bool { return true }
	DaemonRequestImpl = func(_, path string, _, out any, _ DaemonOpts) error {
		assert.Equal(t, "/v1/messages/7/attachments", path)
		return json.Unmarshal([]byte(`{"attachments":[{"id":1,"name":"a.png"},{"id":2,"name":"b.md"}]}`), out)
	}
	daemonAttachmentStream = func(_ context.Context, path string) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(path)), nil
	}
}

func TestInboxAttachmentSavesTheNamedFile(t *testing.T) {
	stubAttachments(t)
	dest := filepath.Join(testutil.CanonicalTempDir(t), "shot.png")
	var stdout, stderr bytes.Buffer
	rc := runInboxAttachment(&inboxAttachmentParams{ID: "7", Name: "b.md", Output: dest}, &stdout, &stderr)
	require.Equal(t, rcOK, rc, "stderr=%s", stderr.String())
	assert.Equal(t, dest+"\n", stdout.String())
	got, err := os.ReadFile(dest)
	require.NoError(t, err)
	assert.Equal(t, "/v1/messages/7/attachments/2", string(got))
}

func TestInboxAttachmentSavesAllToANewDirectory(t *testing.T) {
	stubAttachments(t)
	t.Setenv("TMPDIR", testutil.CanonicalTempDir(t))
	var stdout, stderr bytes.Buffer
	rc := runInboxAttachment(&inboxAttachmentParams{ID: "7"}, &stdout, &stderr)
	require.Equal(t, rcOK, rc, "stderr=%s", stderr.String())
	paths := strings.Fields(stdout.String())
	require.Len(t, paths, 2)
	info, err := os.Stat(filepath.Dir(paths[0]))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())
}

func TestInboxAttachmentRefusesAmbiguousOrUnknownRequests(t *testing.T) {
	stubAttachments(t)
	var stdout, stderr bytes.Buffer
	assert.Equal(t, rcInvalidArg, runInboxAttachment(&inboxAttachmentParams{ID: "7", Output: "x"}, &stdout, &stderr))
	assert.Equal(t, rcInvalidArg, runInboxAttachment(&inboxAttachmentParams{ID: "7", Name: "nope"}, &stdout, &stderr))
}
