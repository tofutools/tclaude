package agentd

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

// spawnProfileDiagnostics warns about every spawn fallback that names a
// spawn profile this node does not have: the process's own default, and each
// agent step's (deciders, a compound task's plan, check and review stages,
// and inline subprocess bodies included). A warning,
// not an error: a template travels between nodes, and the profile may exist
// where it runs. At runtime such a spawn is refused and the step fails.
func spawnProfileDiagnostics(tmpl *model.Template) []processEditDiag {
	if tmpl == nil {
		return nil
	}
	known := map[string]bool{}
	exists := func(name string) bool {
		if seen, ok := known[name]; ok {
			return seen
		}
		profile, err := db.ResolveSpawnProfile(name)
		// A lookup that fails is not a missing profile: say nothing.
		known[name] = err != nil || profile != nil
		return known[name]
	}
	var out []processEditDiag
	check := func(fallback *model.PerformerFallback, scope, target, where string) {
		if fallback == nil || fallback.Action != model.FallbackSpawn {
			return
		}
		name := strings.TrimSpace(fallback.Profile)
		if name == "" || exists(name) {
			return
		}
		out = append(out, processEditDiag{
			Scope: scope, TargetID: target, Severity: model.SeverityWarning, Code: "unknown_spawn_profile",
			Message: fmt.Sprintf("%s spawns profile %q, which this node does not have; the spawn will be refused", where, name),
		})
	}
	check(tmpl.Fallback, "template", "", "the process's fallback")
	var walk func(nodes map[string]model.Node, prefix string)
	walk = func(nodes map[string]model.Node, prefix string) {
		for _, id := range slices.Sorted(maps.Keys(nodes)) {
			node := nodes[id]
			target := prefix + id
			if node.Performer != nil {
				check(node.Performer.Fallback, "node", target, "this step")
			}
			for i := range node.Deciders {
				check(node.Deciders[i].Fallback, "node", target, fmt.Sprintf("decider %d", i+1))
			}
			stage := func(step *model.Step, where string) {
				if step != nil {
					check(step.Performer.Fallback, "node", target, where)
				}
			}
			stage(node.Plan, "its plan stage")
			for i := range node.Checks {
				stage(&node.Checks[i], fmt.Sprintf("its check %d", i+1))
			}
			stage(node.Review, "its review stage")
			if node.Wait != nil && node.Wait.Check != nil && node.Wait.Check.Performer != nil {
				check(node.Wait.Check.Performer.Fallback, "node", target, "its wait check")
			}
			if node.Subprocess != nil && node.Subprocess.IsInline() {
				walk(node.Subprocess.Nodes, target+model.SubprocessNodeIDSeparator)
			}
		}
	}
	walk(tmpl.Nodes, "")
	return out
}
