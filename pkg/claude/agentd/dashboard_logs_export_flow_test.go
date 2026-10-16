package agentd_test

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/common"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Scenario: the operator clicks "export" on the Logs tab with a search and
// level filter set. The download is an attachment of JSON lines, oldest
// first, holding exactly the matching lines verbatim.
func TestLogsExport_DownloadsFilteredJSONLines(t *testing.T) {
	newFlow(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	path := common.OutputLogPath()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	lines := []string{
		`{"time":"2026-07-01T10:00:00.000Z","level":"WARN","msg":"disk boom"}`,
		`{"time":"2026-07-01T10:00:01.000Z","level":"INFO","msg":"info boom"}`,
		`{"time":"2026-07-01T10:00:02.000Z","level":"ERROR","msg":"other"}`,
		`{"time":"2026-07-01T10:00:03.000Z","level":"ERROR","msg":"net boom","peer":"a"}`,
	}
	require.NoError(t, os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600))

	rec := testharness.Serve(agentd.BuildDashboardHandlerForTest(),
		testharness.JSONRequest(t, http.MethodGet, "/api/logs/export?q=boom&level=warn&page=1&page_size=1", nil))
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	assert.Regexp(t, `^attachment; filename="tclaude-logs-\d{8}-\d{6}\.jsonl"$`, rec.Header().Get("Content-Disposition"))
	assert.Equal(t, lines[0]+"\n"+lines[3]+"\n", rec.Body.String(), "export ignores the pager and keeps the filter")
}
