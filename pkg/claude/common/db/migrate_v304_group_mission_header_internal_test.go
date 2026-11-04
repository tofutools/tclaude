package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSplitFoldedMission(t *testing.T) {
	cases := []struct {
		name, context, mission, wantRest, wantHeader string
		ok                                           bool
	}{
		{"mission after boilerplate", "Rules.\n\n## Mission\n\nShip it.", "Ship it.", "Rules.", "Mission", true},
		{"task only", "## Task\n\nShip it.", " Ship it.\r\n", "", "Task", true},
		{"crlf mission normalized", "Rules.\n\n## Mission\n\nLine 1\nLine 2", "Line 1\r\nLine 2", "Rules.", "Mission", true},
		{"edited after deploy", "Rules.\n\n## Mission\n\nShip it.\n\nMore notes.", "Ship it.", "", "", false},
		{"no mission section", "Rules.", "Ship it.", "", "", false},
		{"blank mission", "Rules.", "  ", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rest, header, ok := splitFoldedMission(tc.context, tc.mission)
			assert.Equal(t, tc.ok, ok)
			assert.Equal(t, tc.wantRest, rest)
			assert.Equal(t, tc.wantHeader, header)
		})
	}
}

// The data move on real rows: a context still ending with the folded section
// is split, an edited one is kept byte for byte, and a second run changes
// nothing.
func TestMigrateV304MovesFoldedMissionsOut(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	seed := func(name, context, mission string) {
		_, err := CreateAgentGroup(name, "")
		if err != nil {
			t.Fatal(err)
		}
		mustExec(t, d, `UPDATE agent_groups SET default_context = ?, mission = ?, mission_header = '' WHERE name = ?`,
			context, mission, name)
	}
	const edited = "Rules.\n\n## Mission\n\nShip it.\n\nOperator notes added later."
	seed("folded", "Rules.\n\n## Mission\n\nShip it.", "Ship it.")
	seed("task-only", "## Task\n\nDo the thing.", "Do the thing.")
	seed("edited", edited, "Ship it.")
	seed("plain", "Just rules.", "")
	mustExec(t, d, `UPDATE schema_version SET version = 303`)

	for run := 0; run < 2; run++ {
		if err := migrateV303toV304(d); err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
		mustExec(t, d, `UPDATE schema_version SET version = 303`)
	}

	check := func(name, wantContext, wantHeader string) {
		t.Helper()
		var context, header string
		if err := d.QueryRow(`SELECT default_context, mission_header FROM agent_groups WHERE name = ?`, name).
			Scan(&context, &header); err != nil {
			t.Fatal(err)
		}
		assert.Equal(t, wantContext, context, name)
		assert.Equal(t, wantHeader, header, name)
	}
	check("folded", "Rules.", "Mission")
	check("task-only", "", "Task")
	check("edited", edited, "")
	check("plain", "Just rules.", "")

	g, err := GetAgentGroupByName("folded")
	if err != nil {
		t.Fatal(err)
	}
	assert.Equal(t, "## Mission\n\nShip it.", g.MissionBlock())
}

// The export format carries no mission, so a split group's mission is folded
// back into the exported context: an imported force is briefed as before.
func TestGroupExportFoldsMissionBackIn(t *testing.T) {
	setupTestDB(t)
	_, err := CreateAgentGroup("force", "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = SetAgentGroupDefaultContext("force", "Rules.")
	if err != nil {
		t.Fatal(err)
	}
	_, err = SetAgentGroupDeployMeta("force", "Ship it.", "Mission", "")
	if err != nil {
		t.Fatal(err)
	}
	exp, err := CollectGroupExport("force")
	if err != nil {
		t.Fatal(err)
	}
	assert.Equal(t, "Rules.\n\n## Mission\n\nShip it.", exp.Group.DefaultContext)
}
