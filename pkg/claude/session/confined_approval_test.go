package session

import (
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/harness"
)

func TestRequireConfinementForApproval(t *testing.T) {
	pi := harness.MustGet(harness.PiName)
	cases := []struct {
		name                                  string
		h                                     *harness.Harness
		policy                                string
		confined, allowUnenforced, continuing bool
		refused                               bool
	}{
		{name: "pi auto unconfined", h: pi, policy: harness.PiApprovalAuto, refused: true},
		{name: "pi blank is its auto default", h: pi, policy: "", refused: true},
		{name: "pi auto confined", h: pi, policy: harness.PiApprovalAuto, confined: true},
		{name: "pi auto allowed by the human", h: pi, policy: harness.PiApprovalAuto, allowUnenforced: true},
		{name: "pi auto continuation", h: pi, policy: harness.PiApprovalAuto, continuing: true},
		{name: "pi read-only", h: pi, policy: harness.PiApprovalReadOnly},
		{name: "other harnesses declare nothing", h: harness.MustGet(harness.GeminiName), policy: harness.GeminiApprovalYolo},
	}
	for _, tc := range cases {
		err := RequireConfinementForApproval(tc.h, tc.policy, tc.confined, tc.allowUnenforced, tc.continuing)
		if (err != nil) != tc.refused {
			t.Errorf("%s: err = %v, refused want %v", tc.name, err, tc.refused)
		}
	}
}
