package federationcmd

import (
	"bytes"
	"encoding/json"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
)

func TestOfferContentsCLIPathAndPagination(t *testing.T) {
	oldAvail, oldReq := agent.DaemonAvailableImpl, agent.DaemonRequestImpl
	t.Cleanup(func() { agent.DaemonAvailableImpl, agent.DaemonRequestImpl = oldAvail, oldReq })
	agent.DaemonAvailableImpl = func() bool { return true }
	var paths []string
	agent.DaemonRequestImpl = func(method, path string, in, out any, opts agent.DaemonOpts) error {
		require.Equal(t, "GET", method)
		paths = append(paths, path)
		return json.Unmarshal([]byte(`{"type":"agent","entries":[]}`), out)
	}
	var stdout, stderr bytes.Buffer
	require.Zero(t, runOfferContents(&offerContentsParams{ID: "offer", Peer: "peer name"}, &stdout, &stderr), stderr.String())
	require.Zero(t, runOfferContents(&offerContentsParams{ID: "offer", Peer: "peer name", Path: "history/transcript.jsonl", Offset: 1024, MaxBytes: 8192}, &stdout, &stderr), stderr.String())
	require.Len(t, paths, 2)
	q, err := url.Parse(paths[1])
	require.NoError(t, err)
	require.Equal(t, "/v1/federation/bundle-offers/offer/contents", q.Path)
	require.Equal(t, "peer name", q.Query().Get("peer"))
	require.Equal(t, "history/transcript.jsonl", q.Query().Get("path"))
	require.Equal(t, "1024", q.Query().Get("offset"))
	require.Equal(t, "8192", q.Query().Get("max_bytes"))
	require.Contains(t, stdout.String(), `"entries"`)
}
