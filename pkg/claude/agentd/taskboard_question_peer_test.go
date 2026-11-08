package agentd

import (
	"bytes"
	"mime/multipart"
	"net/textproto"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The asker's node records a create on a peer's board only when it asks a
// question, in a JSON body or in the multipart upload's "task" part.
func TestPeerCreateAsksQuestion(t *testing.T) {
	assert.True(t, peerCreateAsksQuestion("application/json", []byte(`{"title":"q?","question":{}}`)))
	assert.False(t, peerCreateAsksQuestion("application/json", []byte(`{"title":"plain"}`)))
	assert.False(t, peerCreateAsksQuestion("application/json", []byte(`{"title":"x","question":null}`)))
	assert.False(t, peerCreateAsksQuestion("application/json", []byte(`not json`)))

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", `form-data; name="task"`)
	part, err := mw.CreatePart(h)
	require.NoError(t, err)
	_, _ = part.Write([]byte(`{"title":"q?","question":{"options":[{"label":"a"},{"label":"b"}]}}`))
	require.NoError(t, mw.Close())
	assert.True(t, peerCreateAsksQuestion(mw.FormDataContentType(), buf.Bytes()))
}

func TestParsePeerAgentPrincipal(t *testing.T) {
	inst, agent, ok := parsePeerAgentPrincipal("peer:node1/agent:agt_bobremote0000000000000000")
	assert.True(t, ok)
	assert.Equal(t, "node1", inst)
	assert.Equal(t, "agt_bobremote0000000000000000", agent)
	for _, bad := range []string{"peer:node1", "agent:agt_x", "peer: agent bob (agt_x) of node1", "peer:/agent:agt_bobremote0000000000000000", "peer:a/b/agent:agt_bobremote0000000000000000"} {
		_, _, ok := parsePeerAgentPrincipal(bad)
		assert.False(t, ok, bad)
	}
}
