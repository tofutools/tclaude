package agentd

import (
	"testing"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Only a launch other than the attached one counts as a replacement: a young
// session that simply ended keeps its "session ended".
func TestIsReplacement(t *testing.T) {
	launched := time.Now()
	attached := &db.SessionRow{ID: "lbl", CreatedAt: launched}
	cases := []struct {
		name string
		row  *db.SessionRow
		want bool
	}{
		{"the attached launch itself", &db.SessionRow{ID: "lbl", CreatedAt: launched}, false},
		{"the same row relaunched", &db.SessionRow{ID: "lbl", CreatedAt: launched.Add(time.Second)}, true},
		{"a new row (resume)", &db.SessionRow{ID: "lbl-2", CreatedAt: launched}, true},
	}
	for _, tc := range cases {
		if got := isReplacement(tc.row, attached); got != tc.want {
			t.Errorf("%s: isReplacement = %v, want %v", tc.name, got, tc.want)
		}
	}
}
