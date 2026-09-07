package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
)

type terminalFileProbe struct {
	*applicationProbe
	input *app.StageTerminalFileRequest
}

func (p *terminalFileProbe) StageTerminalFile(_ context.Context, in app.StageTerminalFileRequest) (app.TerminalFileResult, error) {
	p.input = &in
	return app.TerminalFileResult{Operation: model.Operation{ID: "upload", ExecutionID: in.ExecutionID, State: model.OperationUncertain, Detail: "private-secret", Principal: model.ExecutionPrincipal("private-execution", "private-agent", 7)}, File: model.TerminalFile{ExecutionID: in.ExecutionID, Filename: in.Filename}}, app.ErrUncertain
}
func TestTerminalFileTransportAuthenticatesBoundsAndProjectsDurableUnknown(t *testing.T) {
	probe := &terminalFileProbe{applicationProbe: &applicationProbe{}}
	handler := testHandler(t, probe)
	body := `{"request_id":"stage","execution_id":"execution","filename":"image.png","content":"YWJj"}`
	require.Equal(t, 401, request(handler, "POST", "/v2/terminal-files", body, "").Code)
	require.Nil(t, probe.input)
	require.Equal(t, 400, request(handler, "POST", "/v2/terminal-files", strings.TrimSuffix(body, "}")+`,"principal":"operator"}`, testCredential).Code)
	require.Nil(t, probe.input)
	response := request(handler, "POST", "/v2/terminal-files", body, testCredential)
	require.Equal(t, 200, response.Code)
	require.Equal(t, model.PrincipalOperator, probe.input.Context.Principal.Kind)
	require.Equal(t, []byte("abc"), probe.input.Content)
	require.Contains(t, response.Body.String(), `"state":"uncertain"`)
	require.NotContains(t, response.Body.String(), "private-")
	probe.input = nil
	large, err := json.Marshal(map[string]any{"request_id": "large", "execution_id": "execution", "filename": "large.bin", "content": bytes.Repeat([]byte{1}, 8<<20)})
	require.NoError(t, err)
	require.Equal(t, 200, request(handler, "POST", "/v2/terminal-files", string(large), testCredential).Code)
	require.Len(t, probe.input.Content, 8<<20)
	probe.input = nil
	require.Equal(t, 400, request(handler, "POST", "/v2/terminal-files", strings.Repeat(" ", 12<<20)+body, testCredential).Code)
	require.Nil(t, probe.input)
}
