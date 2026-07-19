package processexec

import (
	"fmt"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/state/pathv1"
)

// HumanContactAssignee is the single derivation of a human performer's
// durable assignee: explicit assignee, else profile, else the operator
// default, with bare names prefixed "human:". The human dispatch adapter and
// the schema-7 contact preflight both consume it, so the preflighted value is
// the dispatched value by construction.
func HumanContactAssignee(performer model.Performer) string {
	assignee := strings.TrimSpace(performer.Assignee)
	if assignee == "" {
		assignee = strings.TrimSpace(performer.Profile)
	}
	if assignee == "" {
		return "human:operator"
	}
	if !strings.HasPrefix(assignee, "human:") && !strings.HasPrefix(assignee, "role:") {
		return "human:" + assignee
	}
	return assignee
}

// PreflightSchema7Contact proves, without any side effect, that a deferred
// performer's eventual durable schema-7 contact fields fit the pathv1
// bounds: schedule derivation (defaults or explicit), cadence/budget,
// escalation target, and — for humans — the exact template-derived assignee.
// Both the schema-7 eligibility gate and the executor run it before any
// claim or dispatch, so an incompatible template stays on v6 and an
// incompatible run fails closed before creating external work it could never
// seal a contact for. Agent assignees are generated fixed-length ids
// ("agent:agt_<32 hex>") and are bounded by construction; the durable
// post-dispatch validation remains as defense in depth.
func PreflightSchema7Contact(performer model.Performer) error {
	cadence, budget, escalation, err := ContactScheduleFor(performer)
	if err != nil {
		return err
	}
	if budget <= 0 {
		return fmt.Errorf("contact budget must be positive")
	}
	if _, err := pathv1.ParseContactCadence(cadence.String()); err != nil {
		return err
	}
	if escalation == "" || len(escalation) > pathv1.MaxContactFieldBytes {
		return fmt.Errorf("contact escalation target is empty or over %d bytes", pathv1.MaxContactFieldBytes)
	}
	if performer.Kind == model.PerformerHuman {
		if assignee := HumanContactAssignee(performer); len(assignee) > pathv1.MaxContactFieldBytes {
			return fmt.Errorf("contact assignee is over %d bytes", pathv1.MaxContactFieldBytes)
		}
	}
	return nil
}
