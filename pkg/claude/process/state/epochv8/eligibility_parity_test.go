package epochv8_test

import (
	"fmt"
	"strings"
	"testing"

	processexec "github.com/tofutools/tclaude/pkg/claude/process/exec"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/state/epochv8"
)

// This test is deliberately external to package epochv8. S3 may later import
// epochv8 from an execution package; keeping the diagnostic parity edge in the
// test binary avoids an epochv8 -> exec -> epochv8 production/test cycle.
func TestFrozenContactEligibilityParitySignalsProductionDrift(t *testing.T) {
	cases := []model.Performer{
		{Kind: model.PerformerAgent, Prompt: "default"},
		{Kind: model.PerformerHuman, Ask: "default"},
		{Kind: model.PerformerAgent, Prompt: "explicit", Contact: &model.ContactSchedule{Cadence: "5m", Budget: 2, EscalationTarget: "human:operator"}},
		{Kind: model.PerformerAgent, Prompt: "bad cadence", Contact: &model.ContactSchedule{Cadence: "-1s", Budget: 2, EscalationTarget: "human:operator"}},
		{Kind: model.PerformerAgent, Prompt: "bad budget", Contact: &model.ContactSchedule{Cadence: "5m", Budget: 0, EscalationTarget: "human:operator"}},
		{Kind: model.PerformerAgent, Prompt: "long escalation", Contact: &model.ContactSchedule{Cadence: "5m", Budget: 2, EscalationTarget: strings.Repeat("x", 257)}},
		{Kind: model.PerformerHuman, Ask: "long assignee", Assignee: strings.Repeat("x", 257)},
	}
	for i, performer := range cases {
		source, err := contactTemplateSource(performer, i)
		if err != nil {
			t.Fatal(err)
		}
		classification, err := epochv8.ClassifyTemplateSource(source)
		if err != nil {
			t.Fatal(err)
		}
		frozen := classification.Status == epochv8.EligibilitySupported
		moving := processexec.PreflightSchema7Contact(performer) == nil
		if frozen != moving {
			t.Fatalf("case %d: frozen matrix=%v current production selector=%v; review and version intentional eligibility drift", i, frozen, moving)
		}
	}
}

func contactTemplateSource(performer model.Performer, index int) ([]byte, error) {
	template := &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: fmt.Sprintf("contact-parity-%d", index), Start: "work",
		Nodes: map[string]model.Node{
			"work": {Type: model.NodeTypeTask, Performer: &performer, Next: model.Next{"pass": "done"}},
			"done": {Type: model.NodeTypeEnd, Result: "completed"},
		},
	}
	return model.CanonicalYAML(template)
}
