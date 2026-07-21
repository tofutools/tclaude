package agentd_test

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc/agentipctest"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
)

func TestEpochV8PreviewRealClientPreservesStaleAndBlockedBodies(t *testing.T) {
	f, root := processEngineFlow(t)
	fs, err := store.NewFS(root)
	require.NoError(t, err)
	source := func(prompt string) []byte {
		tmpl := &model.Template{APIVersion: model.APIVersion, Kind: model.Kind, ID: "preview-client", Start: "work", Nodes: map[string]model.Node{
			"work": {Type: model.NodeTypeTask, Performer: &model.Performer{Kind: model.PerformerAgent, Prompt: prompt}, Next: model.Next{"pass": "done"}},
			"done": {Type: model.NodeTypeEnd, Result: "completed"},
		}}
		encoded, encodeErr := model.CanonicalYAML(tmpl)
		require.NoError(t, encodeErr)
		return encoded
	}
	initialSource, candidateSource := source("initial"), source("changed")
	parsed, err := model.ParseExactSource(initialSource)
	require.NoError(t, err)
	record, err := fs.PutTemplate(t.Context(), parsed.Template)
	require.NoError(t, err)
	initialized, err := fs.InitializeEpochV8Run(t.Context(), store.RunRecord{ID: "preview-client-run", TemplateRef: record.Ref}, initialSource)
	require.NoError(t, err)

	socketDir := agentipctest.ShortSocketDir(t)
	socket := socketDir + "/preview.sock"
	listener, err := net.Listen("unix", socket)
	require.NoError(t, err)
	server := &http.Server{Handler: f.Mux, ReadHeaderTimeout: time.Second}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()
	t.Cleanup(func() {
		_ = server.Close()
		serveErr := <-serveDone
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			t.Errorf("preview test server: %v", serveErr)
		}
	})
	t.Setenv(agentipc.SocketEnv, socket)

	request := func(revision uint64, digest string) *agent.DaemonError {
		t.Helper()
		body := map[string]any{
			"baseBinding":     map[string]any{"revision": revision, "digest": digest},
			"candidateSource": string(candidateSource),
			"handoffs":        []any{},
		}
		err := agent.DaemonRequest(http.MethodPost, "/v1/process/runs/preview-client-run/unlock/preview", body, nil, agent.DaemonOpts{Timeout: 5 * time.Second})
		var daemonErr *agent.DaemonError
		require.ErrorAs(t, err, &daemonErr)
		return daemonErr
	}

	stale := request(initialized.Checkpoint.Binding().Revision, strings.Repeat("0", 64))
	assert.Equal(t, http.StatusConflict, stale.Status)
	var staleBody struct {
		Status         string `json:"status"`
		CurrentBinding struct {
			Revision uint64 `json:"revision"`
			Digest   string `json:"digest"`
		} `json:"currentBinding"`
	}
	require.NoError(t, json.Unmarshal(stale.Raw, &staleBody))
	assert.Equal(t, "stale", staleBody.Status)
	assert.Equal(t, initialized.Checkpoint.Binding().Revision, staleBody.CurrentBinding.Revision)
	assert.Equal(t, initialized.Checkpoint.Binding().Digest, staleBody.CurrentBinding.Digest)

	blocked := request(initialized.Checkpoint.Binding().Revision, initialized.Checkpoint.Binding().Digest)
	assert.Equal(t, http.StatusUnprocessableEntity, blocked.Status)
	var blockedBody struct {
		Status   string `json:"status"`
		Blockers []struct {
			Code  string `json:"code"`
			Token string `json:"token"`
		} `json:"blockers"`
	}
	require.NoError(t, json.Unmarshal(blocked.Raw, &blockedBody))
	assert.Equal(t, "blocked", blockedBody.Status)
	require.NotEmpty(t, blockedBody.Blockers)
	assert.Len(t, blockedBody.Blockers[0].Token, 64)
}
