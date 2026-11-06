package agent

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Group retire, group stop/resume and task-force stand-down answer with one
// row per member, rendered as the same table. These pin each command's whole
// output for a two-member reply (one member unnamed) and an empty one.

var memberResultsReply = map[string]any{
	"group": "crew", "action": "retired",
	"members": []map[string]any{
		{"agent_id": "agt_1234567890ab", "conv_id": "c1", "title": "builder", "action": "retired", "detail": "pane closed"},
		{"agent_id": "", "conv_id": "c2b4e6f8-0000-4000-8000-000000000002", "title": "", "action": "skipped", "detail": "already offline",
			"warnings": []string{"no worktree"}, "changes": []string{"model"}},
	},
	"warnings":        []string{"one member was already gone"},
	"rhythms_removed": 2, "waves_cancelled": 1,
}

var memberResultCommands = []struct {
	name string
	run  func(stdout, stderr io.Writer) int
}{
	{"groups retire", func(o, e io.Writer) int { return runGroupsRetire(&groupsRetireParams{Name: "crew"}, o, e) }},
	{"groups stop", func(o, e io.Writer) int { return runGroupsStop(&groupsStopParams{Name: "crew"}, o, e) }},
	{"groups resume", func(o, e io.Writer) int { return runGroupsResume(&groupsResumeParams{Name: "crew"}, o, e) }},
	{"task-force stand-down", func(o, e io.Writer) int {
		return runTaskForceStandDown(&taskForceStandDownParams{Name: "crew"}, o, e)
	}},
}

func memberResultsOutput(t *testing.T, run func(stdout, stderr io.Writer) int, reply map[string]any) string {
	t.Helper()
	prevAvail, prevReq := DaemonAvailableImpl, DaemonRequestImpl
	t.Cleanup(func() { DaemonAvailableImpl, DaemonRequestImpl = prevAvail, prevReq })
	DaemonAvailableImpl = func() bool { return true }
	DaemonRequestImpl = func(_, _ string, _, out any, _ DaemonOpts) error {
		raw, err := json.Marshal(reply)
		if err != nil {
			return err
		}
		return json.Unmarshal(raw, out)
	}
	var stdout, stderr bytes.Buffer
	require.Equal(t, rcOK, run(&stdout, &stderr), stderr.String())
	return stdout.String()
}

func TestMemberResultsGolden(t *testing.T) {
	empty := map[string]any{"group": "crew", "action": "retired", "members": []any{}, "rhythms_removed": 0, "waves_cancelled": 0}
	for _, c := range memberResultCommands {
		for _, reply := range []struct {
			name string
			body map[string]any
		}{{"members", memberResultsReply}, {"empty", empty}} {
			t.Run(c.name+"/"+reply.name, func(t *testing.T) {
				got := memberResultsOutput(t, c.run, reply.body)
				key := c.name + "/" + reply.name
				if os.Getenv("MEMBER_RESULTS_DUMP") != "" {
					t.Logf("GOLDEN %q: %q", key, got)
					return
				}
				assert.Equal(t, memberResultsGolden[key], got)
			})
		}
	}
}
