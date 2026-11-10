package agent

import (
	"bytes"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// `archive <group> --done` keeps the archive-done route, which a linked
// node of any version has; --dropped (alone or with --done) takes
// archive-closed with the statuses, so an older node answers 404 rather than
// archiving its done tasks in their place. --older-than goes with either.
func TestRunTaskboardArchiveClosedRoutes(t *testing.T) {
	for _, tc := range []struct {
		name     string
		p        taskboardArchiveParams
		path     string
		statuses []string
		printed  string
	}{
		{"done", taskboardArchiveParams{Target: "crew", Done: true}, "/v1/groups/crew/taskboard/archive-done", nil, "archived 2 done task(s) on crew"},
		{"dropped", taskboardArchiveParams{Target: "crew", Dropped: true, OlderThan: "7d"}, "/v1/groups/crew/taskboard/archive-closed", []string{"dropped"}, "archived 2 dropped task(s) on crew"},
		{"both", taskboardArchiveParams{Target: "crew", Done: true, Dropped: true}, "/v1/groups/crew/taskboard/archive-closed", []string{"done", "dropped"}, "archived 2 done or dropped task(s) on crew"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls []capturedReq
			stubDaemon(t, &calls, ok(`{"group":"crew","archived":["tk_1","tk_2"],"count":2}`))
			var stdout, stderr bytes.Buffer
			p := tc.p
			rc := runTaskboardArchiveClosed(&p, &stdout, &stderr)
			require.Equal(t, rcOK, rc, "stderr=%q", stderr.String())
			require.Len(t, calls, 1)
			assert.Equal(t, http.MethodPost, calls[0].method)
			assert.Equal(t, tc.path, calls[0].path)
			body, _ := calls[0].body.(map[string]any)
			if tc.statuses == nil {
				assert.NotContains(t, body, "statuses")
			} else {
				assert.Equal(t, tc.statuses, body["statuses"])
			}
			if tc.p.OlderThan != "" {
				assert.Equal(t, tc.p.OlderThan, body["older_than"])
			}
			assert.Contains(t, stdout.String(), tc.printed)
		})
	}
}
