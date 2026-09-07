package transport

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

type executionFileProbe struct {
	*applicationProbe
	input *app.ReadExecutionFileRequest
}

func (p *executionFileProbe) ReadExecutionFile(_ context.Context, in app.ReadExecutionFileRequest) (ports.ExecutionFileContent, error) {
	p.input = &in
	return ports.ExecutionFileContent{Filename: "report.html", Content: []byte("<script>untrusted</script>")}, nil
}
func TestExecutionFileDownloadAuthenticatesAndForcesAttachment(t *testing.T) {
	probe := &executionFileProbe{applicationProbe: &applicationProbe{}}
	handler := testHandler(t, probe)
	path := "/v2/execution-files?execution_id=execution&path=report.html"
	require.Equal(t, 401, request(handler, "GET", path, "", "").Code)
	require.Nil(t, probe.input)
	require.Equal(t, 422, request(handler, "GET", path+"&root=/outside", "", testCredential).Code)
	require.Nil(t, probe.input)
	require.Equal(t, 422, request(handler, "GET", path+"&execution_id=other", "", testCredential).Code)
	require.Nil(t, probe.input)
	response := request(handler, "GET", path, "", testCredential)
	require.Equal(t, 200, response.Code)
	require.Equal(t, "application/octet-stream", response.Header().Get("Content-Type"))
	require.Equal(t, "nosniff", response.Header().Get("X-Content-Type-Options"))
	require.Contains(t, response.Header().Get("Content-Disposition"), "attachment;")
	require.Contains(t, response.Header().Get("Content-Disposition"), "report.html")
	require.Equal(t, "<script>untrusted</script>", response.Body.String())
	require.Equal(t, model.PrincipalOperator, probe.input.Principal.Kind)
	require.Equal(t, model.ExecutionID("execution"), probe.input.ExecutionID)
}
