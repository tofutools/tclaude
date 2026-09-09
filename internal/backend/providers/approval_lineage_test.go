package providers_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
	"github.com/tofutools/tclaude/internal/backend/providers/claude"
	"github.com/tofutools/tclaude/internal/backend/providers/codex"
	"github.com/tofutools/tclaude/internal/backend/providers/copilot"
	"github.com/tofutools/tclaude/internal/backend/providers/opencode"
)

func TestApprovalLineagePreservesV1CrossProviderBoundaries(t *testing.T) {
	providers := map[string]ports.ApprovalLineageProvider{"claude": &claude.Provider{}, "codex": &codex.Provider{}, "copilot": &copilot.Provider{}, "opencode": &opencode.Provider{}}
	type launch struct {
		h      string
		mode   model.ApprovalMode
		review bool
	}
	posture := func(l launch) model.ApprovalPosture {
		return providers[l.h].ApprovalPosture(model.DesiredConfiguration{Harness: l.h, Approval: l.mode, AutoReview: l.review})
	}
	cases := []struct {
		name          string
		parent, child launch
		allowed       bool
	}{
		{"human gated cannot mint commands", launch{"claude", model.ApprovalManual, false}, launch{"codex", model.ApprovalNever, false}, false},
		{"edit only cannot mint commands", launch{"claude", model.ApprovalAcceptEdits, false}, launch{"codex", model.ApprovalOnRequest, false}, false},
		{"commands contain edits", launch{"codex", model.ApprovalNever, false}, launch{"claude", model.ApprovalAcceptEdits, false}, true},
		{"reviewer does not imply edits", launch{"codex", model.ApprovalUntrusted, true}, launch{"claude", model.ApprovalAcceptEdits, false}, false},
		{"edits do not imply reviewer", launch{"claude", model.ApprovalAcceptEdits, false}, launch{"codex", model.ApprovalUntrusted, true}, false},
		{"never does not activate reviewer", launch{"codex", model.ApprovalNever, false}, launch{"codex", model.ApprovalNever, true}, true},
		{"auto supervisor cannot mint reviewer", launch{"claude", model.ApprovalAuto, false}, launch{"codex", model.ApprovalOnRequest, true}, false},
		{"auto supervisor cannot mint bypass", launch{"claude", model.ApprovalAuto, false}, launch{"claude", model.ApprovalBypassPermissions, false}, false},
		{"explicit v1 copilot bridge", launch{"claude", model.ApprovalAuto, false}, launch{"copilot", model.ApprovalYolo, false}, true},
		{"bridge does not extend to codex", launch{"codex", model.ApprovalNever, false}, launch{"copilot", model.ApprovalYolo, false}, false},
		{"yolo contains reviewer", launch{"copilot", model.ApprovalYolo, false}, launch{"codex", model.ApprovalOnRequest, true}, true},
		{"copilot tools cannot mint yolo", launch{"copilot", model.ApprovalAllowTools, false}, launch{"copilot", model.ApprovalYolo, false}, false},
		{"exact claude inherit continuity", launch{"claude", model.ApprovalInherit, false}, launch{"claude", model.ApprovalInherit, false}, true},
		{"inherit parent proves only floor", launch{"claude", model.ApprovalInherit, false}, launch{"claude", model.ApprovalAuto, false}, false},
		{"inherit child charged ceiling", launch{"claude", model.ApprovalAuto, false}, launch{"claude", model.ApprovalInherit, false}, false},
		{"copilot inherit not continuation exception", launch{"copilot", model.ApprovalInherit, false}, launch{"copilot", model.ApprovalInherit, false}, false},
		{"copilot inherit child uncertain", launch{"copilot", model.ApprovalAllowTools, false}, launch{"copilot", model.ApprovalInherit, false}, false},
		{"opencode deny cannot mint edits", launch{"opencode", model.ApprovalDeny, false}, launch{"opencode", model.ApprovalAllowTools, false}, false},
		{"opencode edits contained by acceptEdits", launch{"claude", model.ApprovalAcceptEdits, false}, launch{"opencode", model.ApprovalAllowTools, false}, true},
		{"opencode automatic is not native allow-tools", launch{"claude", model.ApprovalAcceptEdits, false}, launch{"opencode", model.ApprovalAutomatic, false}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { require.Equal(t, tc.allowed, posture(tc.parent).Allows(posture(tc.child))) })
	}
	for harness, provider := range providers {
		t.Run(harness+" invalid policy", func(t *testing.T) {
			for _, mode := range []model.ApprovalMode{"", "future-mode"} {
				p := provider.ApprovalPosture(model.DesiredConfiguration{Harness: harness, Approval: mode})
				require.False(t, p.Known)
				require.False(t, p.Allows(posture(launch{"claude", model.ApprovalManual, false})))
			}
		})
		t.Run(harness+" foreign harness", func(t *testing.T) {
			require.False(t, provider.ApprovalPosture(model.DesiredConfiguration{Harness: "foreign", Approval: model.ApprovalAutomatic}).Known)
		})
		if harness != "codex" {
			t.Run(harness+" foreign reviewer", func(t *testing.T) {
				require.False(t, provider.ApprovalPosture(model.DesiredConfiguration{Harness: harness, Approval: model.ApprovalAutomatic, AutoReview: true}).Known)
			})
		}
	}
}
