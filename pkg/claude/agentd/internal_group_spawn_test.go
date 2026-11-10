package agentd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewInternalSpawnRequestCarriesPathActorAndReservation(t *testing.T) {
	type baseKey struct{}
	actor := &peer{PID: 1, HumanTokenValid: true}
	req := newInternalSpawnRequest(internalSpawnRequest{
		Ctx:             context.WithValue(context.Background(), baseKey{}, "base"),
		Path:            "/v1/groups/team/spawn",
		Group:           "team",
		Body:            []byte(`{"name":"w"}`),
		Actor:           actor,
		ReservedAgentID: "agt_reserved",
	})
	assert.Equal(t, http.MethodPost, req.Method)
	assert.Equal(t, "/v1/groups/team/spawn", req.URL.Path)
	assert.Equal(t, "team", req.PathValue("name"))
	assert.Equal(t, "base", req.Context().Value(baseKey{}))
	assert.Same(t, actor, req.Context().Value(peerKey{}))
	assert.Equal(t, "agt_reserved", req.Context().Value(reservedAgentIDContextKey{}))

	bare := newInternalSpawnRequest(internalSpawnRequest{Path: "/"})
	assert.Empty(t, bare.PathValue("name"))
	assert.Nil(t, bare.Context().Value(peerKey{}), "no actor unless the caller picks one")
	assert.Nil(t, bare.Context().Value(reservedAgentIDContextKey{}))
}

func TestInternalSpawnResultDecodesTheHandlersShapes(t *testing.T) {
	rec := httptest.NewRecorder()
	writeJSON(rec, http.StatusOK, map[string]any{"agent_id": "agt_1", "conv_id": "c1"})
	ok := recordedSpawnResult(rec)
	assert.True(t, ok.OK())
	resp, err := ok.Response()
	require.NoError(t, err)
	assert.Equal(t, "agt_1", resp.AgentID)
	assert.Equal(t, "c1", resp.ConvID)

	rec = httptest.NewRecorder()
	writeError(rec, http.StatusConflict, fedCodeNodeBusy, "node capacity reached")
	refused := recordedSpawnResult(rec)
	assert.False(t, refused.OK())
	code, msg := refused.Refusal()
	assert.Equal(t, fedCodeNodeBusy, code, "a spawnFailure Kind travels as writeError's code")
	assert.Equal(t, "node capacity reached", msg)

	plain := internalSpawnResult{Status: http.StatusBadGateway, Body: []byte("upstream exploded")}
	code, msg = plain.Refusal()
	assert.Empty(t, code)
	assert.Empty(t, msg)
	_, err = plain.Response()
	assert.Error(t, err)

	out := httptest.NewRecorder()
	refused.WriteTo(out)
	assert.Equal(t, http.StatusConflict, out.Code)
	assert.Equal(t, "application/json", out.Header().Get("Content-Type"))
	assert.Equal(t, string(refused.Body), out.Body.String())
}
