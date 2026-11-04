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
