package agentd

import (
	"maps"
	"slices"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

// waitStepDiagnostics names every wait step, at authoring time, as one that
// cannot run yet: the model stores wait steps (the full editor's Wait / timer
// makes them, as a placeholder for a planned feature), but run creation
// refuses them (engine.CheckEligibility, unsupported_wait). Said here, the
// editors' problems line tells the operator before a Start does.
func waitStepDiagnostics(tmpl *model.Template) []processEditDiag {
	if tmpl == nil {
		return nil
	}
	var out []processEditDiag
	var walk func(nodes map[string]model.Node, prefix string)
	walk = func(nodes map[string]model.Node, prefix string) {
		for _, id := range slices.Sorted(maps.Keys(nodes)) {
			node := nodes[id]
			target := prefix + id
			if node.Type == model.NodeTypeWait {
				out = append(out, processEditDiag{
					Scope: "node", TargetID: target, Severity: model.SeverityError, Code: "wait_not_runnable",
					Message: "a wait step can't run yet, so a process with one can't start a run; keep it as a placeholder or remove it",
				})
			}
			if node.Subprocess != nil && node.Subprocess.IsInline() {
				walk(node.Subprocess.Nodes, target+model.SubprocessNodeIDSeparator)
			}
		}
	}
	walk(tmpl.Nodes, "")
	return out
}
