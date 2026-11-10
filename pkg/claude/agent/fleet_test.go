package agent

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRenderFleetLeadsWithWhatNeedsYou(t *testing.T) {
	var r fleetResp
	require.NoError(t, json.Unmarshal([]byte(`{
	  "generated_at": "2026-10-30T12:00:00Z",
	  "counts": {"agents": 3, "online": 2, "working": 1, "needs_you": 1, "offline": 1},
	  "attention": [
	    {"kind": "prompt", "agent_id": "agt_0123456789abcdef", "title": "asker", "detail": "is waiting on a permission prompt", "since": "2026-10-30T11:55:00Z"},
	    {"kind": "pr", "agent_id": "agt_fedcba9876543210", "title": "builder", "detail": "presented a PR: fleet", "url": "https://github.com/o/r/pull/7"}
	  ],
	  "agents": [
	    {"agent_id": "agt_0123456789abcdef", "title": "asker", "groups": ["api"], "online": true, "status": "awaiting_permission"},
	    {"agent_id": "agt_fedcba9876543210", "title": "builder", "groups": ["api"], "online": true, "status": "working", "task": "https://x/tcl-1", "task_label": "tcl-1"},
	    {"agent_id": "agt_aaaaaaaaaaaaaaaa", "title": "solo", "groups": [], "online": false}
	  ]
	}`), &r))
	var out bytes.Buffer
	renderFleet(&out, &r, false)
	got := out.String()
	require.Contains(t, got, "Fleet: 3 agents, 2 online (1 working, 0 idle, 1 need you), 1 offline")
	require.Contains(t, got, "Needs you:\n  - asker is waiting on a permission prompt (5m ago) [agt_01234567]\n  - builder presented a PR: fleet https://github.com/o/r/pull/7")
	require.Contains(t, got, "api:\n")
	require.Contains(t, got, "task:tcl-1")
	require.NotContains(t, got, "solo")
	require.Contains(t, got, "(1 offline agents not shown; --all lists them)")

	out.Reset()
	renderFleet(&out, &r, true)
	require.Contains(t, out.String(), "(no group):\n  solo")
}
