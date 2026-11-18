package agentd

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

// sameAsRetiredDiagnostics warns about a step that is to be the same agent
// as a step whose new agent is retired once that step is done (fallback
// lifetime "step"): by the time it runs, that agent is gone, and the step
// falls back to its own selector. Keeping the agent for later steps (lifetime
// "run", the default) is what such a pair means.
func sameAsRetiredDiagnostics(tmpl *model.Template) []processEditDiag {
	if tmpl == nil {
		return nil
	}
	var out []processEditDiag
	var walk func(nodes map[string]model.Node, prefix string)
	walk = func(nodes map[string]model.Node, prefix string) {
		retiredAfter := func(id string) bool {
			node, ok := nodes[id]
			if !ok || node.Performer == nil || node.Performer.Kind != model.PerformerAgent {
				return false
			}
			fallback := node.Performer.Fallback
			if fallback == nil || fallback.Action != model.FallbackSpawn {
				return false
			}
			lifetime := fallback.Lifetime
			if lifetime == "" {
				lifetime = model.DefaultLifetime()
			}
			return lifetime == model.LifetimeStep
		}
		name := func(id string) string {
			if node, ok := nodes[id]; ok && strings.TrimSpace(node.Name) != "" {
				return node.Name
			}
			return id
		}
		for _, id := range slices.Sorted(maps.Keys(nodes)) {
			node := nodes[id]
			performers := []*model.Performer{node.Performer}
			for i := range node.Deciders {
				performers = append(performers, &node.Deciders[i])
			}
			for _, performer := range performers {
				if performer == nil {
					continue
				}
				if same := strings.TrimSpace(performer.SameAs); same != "" && retiredAfter(same) {
					out = append(out, processEditDiag{
						Scope: "node", TargetID: prefix + id, Severity: model.SeverityWarning, Code: "same_as_retired",
						Message: fmt.Sprintf("%s's new agent is retired once %s is done, so it can't do this step too; "+
							"keep it for later steps (Afterwards) or pick someone else", name(same), name(same)),
					})
				}
			}
			if node.Subprocess != nil && node.Subprocess.IsInline() {
				walk(node.Subprocess.Nodes, prefix+id+model.SubprocessNodeIDSeparator)
			}
		}
	}
	walk(tmpl.Nodes, "")
	return out
}
