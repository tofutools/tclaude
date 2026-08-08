package probehelper

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestDispatchPrivateModesRequireExactShape(t *testing.T) {
	for _, args := range [][]string{
		{"tclaude", AFUnixMode, "extra"},
		{"tclaude", StubMode},
		{"tclaude", StubMode, "root", "secret", "command"},
		{"tclaude", internalPrefix + "unknown"},
	} {
		handled, code := Dispatch(args)
		assert.True(t, handled)
		assert.Equal(t, invalidInvocationExit, code)
	}
	handled, code := Dispatch([]string{"tclaude", "session", "new"})
	assert.False(t, handled)
	assert.Zero(t, code)
}

func TestAFUnixModeExitContract(t *testing.T) {
	previousSocket := socketAFUnix
	previousClose := closeFD
	t.Cleanup(func() {
		socketAFUnix = previousSocket
		closeFD = previousClose
	})
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{name: "denied eperm", err: unix.EPERM, want: AFUnixDeniedExit},
		{name: "denied eacces", err: unix.EACCES, want: AFUnixDeniedExit},
		{name: "untestable", err: unix.EAFNOSUPPORT, want: AFUnixUntestableExit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			socketAFUnix = func() (int, error) { return -1, tc.err }
			assert.Equal(t, tc.want, afUnixExit())
		})
	}
	closed := -1
	socketAFUnix = func() (int, error) { return 42, nil }
	closeFD = func(fd int) error {
		closed = fd
		return nil
	}
	assert.Zero(t, afUnixExit())
	assert.Equal(t, 42, closed)
}

func TestServeStubDeterministicMessagesRoundTrip(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.Chmod(root, 0o700))
	rootFD, err := openProbeRoot(root)
	require.NoError(t, err)
	require.NoError(t, unix.Close(rootFD))
	secret := strings.Repeat("a", 48)
	marker := "TCLAUDE_STACKED_INNER_OK_" + secret
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- ServeStub(ctx, root, secret, "touch allowed", marker)
	}()
	endpointPath := filepath.Join(root, EndpointFileName)
	require.Eventually(t, func() bool {
		data, err := os.ReadFile(endpointPath)
		return err == nil && strings.HasPrefix(string(data), "http://127.0.0.1:")
	}, 3*time.Second, 10*time.Millisecond)
	endpointBytes, err := os.ReadFile(endpointPath)
	require.NoError(t, err)
	endpoint := string(endpointBytes)
	info, err := os.Stat(endpointPath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	response, err := http.Head(endpoint + "/wrong/v1/messages")
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	assert.Equal(t, http.StatusNotFound, response.StatusCode)
	response, err = http.Head(endpoint + "/" + secret + "/v1/messages")
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	assert.Equal(t, http.StatusOK, response.StatusCode)

	first := postMessages(t, endpoint, secret, `{"messages":[]}`)
	content := first["content"].([]any)
	tool := content[0].(map[string]any)
	assert.Equal(t, "tool_use", tool["type"])
	assert.Equal(t, "Bash", tool["name"])
	assert.Equal(t, "touch allowed", tool["input"].(map[string]any)["command"])
	assert.Equal(t, "tool_use", first["stop_reason"])

	successBody := `{"messages":[{"content":[{"type":"tool_result","content":"` +
		marker + `","is_error":false}]}]}`
	success := postMessages(t, endpoint, secret, successBody)
	assert.Equal(t, "end_turn", success["stop_reason"])
	successContent := success["content"].([]any)
	assert.Equal(
		t,
		"TCLAUDE_STACKED_STUB_OK_"+secret,
		successContent[0].(map[string]any)["text"],
	)

	refusedBody := `{"messages":[{"content":[{"type":"tool_result","content":"no marker","is_error":false}]}]}`
	refused := postMessages(t, endpoint, secret, refusedBody)
	assert.Equal(t, "probe refused", refused["content"].([]any)[0].(map[string]any)["text"])
	evidence, err := os.ReadFile(filepath.Join(root, InnerPolicyFileName))
	require.NoError(t, err)
	assert.Equal(t, InnerPolicyFailureValue, string(evidence))

	cancel()
	require.NoError(t, <-done)
}

func TestServeStubBoundsRequestsAndRefusesEndpointSymlink(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.Chmod(root, 0o700))
	secret := strings.Repeat("b", 48)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- ServeStub(ctx, root, secret, "true", "marker")
	}()
	endpointPath := filepath.Join(root, EndpointFileName)
	require.Eventually(t, func() bool {
		endpoint, err := os.ReadFile(endpointPath)
		return err == nil && strings.HasPrefix(string(endpoint), "http://127.0.0.1:")
	}, 3*time.Second, 10*time.Millisecond)
	endpoint, err := os.ReadFile(endpointPath)
	require.NoError(t, err)
	oversized := bytes.Repeat([]byte("x"), maxMessagesRequestBytes+1)
	response, err := http.Post(
		string(endpoint)+"/"+secret+"/v1/messages",
		"application/json",
		bytes.NewReader(oversized),
	)
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, response.Body)
	require.NoError(t, response.Body.Close())
	assert.Equal(t, http.StatusBadRequest, response.StatusCode)
	cancel()
	require.NoError(t, <-done)

	otherRoot := t.TempDir()
	require.NoError(t, os.Chmod(otherRoot, 0o700))
	target := filepath.Join(t.TempDir(), "outside")
	require.NoError(t, os.Symlink(target, filepath.Join(otherRoot, EndpointFileName)))
	err = ServeStub(
		context.Background(),
		otherRoot,
		strings.Repeat("c", 48),
		"true",
		"marker",
	)
	require.Error(t, err)
	_, statErr := os.Stat(target)
	require.True(t, errors.Is(statErr, os.ErrNotExist))
}

func TestPublishAtDoesNotClobberExistingFile(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.Chmod(root, 0o700))
	rootFD, err := openProbeRoot(root)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, unix.Close(rootFD)) })

	require.NoError(t, publishAt(rootFD, "evidence", "first", 0o600))
	require.Error(t, publishAt(rootFD, "evidence", "second", 0o600))
	value, err := os.ReadFile(filepath.Join(root, "evidence"))
	require.NoError(t, err)
	assert.Equal(t, "first", string(value))
}

func TestPublishAtRemovesTempFile(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.Chmod(root, 0o700))
	rootFD, err := openProbeRoot(root)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, unix.Close(rootFD)) })

	require.NoError(t, publishAt(rootFD, "success", "value", 0o600))
	requireNoPublishTemps(t, root, "success")
	require.NoError(t, publishAt(rootFD, "failure", "first", 0o600))
	require.Error(t, publishAt(rootFD, "failure", "second", 0o600))
	requireNoPublishTemps(t, root, "failure")
}

func requireNoPublishTemps(t *testing.T, root, name string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	prefix := "." + name + ".tmp-"
	for _, entry := range entries {
		assert.False(t, strings.HasPrefix(entry.Name(), prefix), "temporary publish file remains")
	}
}

func postMessages(
	t *testing.T,
	endpoint, secret, body string,
) map[string]any {
	t.Helper()
	response, err := http.Post(
		endpoint+"/"+secret+"/v1/messages",
		"application/json",
		strings.NewReader(body),
	)
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode)
	var decoded map[string]any
	require.NoError(t, json.NewDecoder(response.Body).Decode(&decoded))
	return decoded
}
