package agentd

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
)

// Structural validation never reads the template store, so a subprocess step
// that references a stored template is checked only at run start. The editor
// wants those problems while authoring: a missing or unloadable template,
// in-ports the referenced template does not declare, problems inside it, and
// a resolved tree past the nesting or size budget. templateRefDiagnostics
// resolves each reference the way run creation does and attributes what it
// finds to the referencing ▣ node; fixing them means opening the stored
// template, never editing it in place.

// templateRefDiagnostics returns editor diagnostics for every templateRef
// subprocess in tmpl, at any inline depth. resolve loads stored templates.
func templateRefDiagnostics(tmpl *model.Template, resolve model.TemplateResolver) []processEditDiag {
	if tmpl == nil || resolve == nil {
		return nil
	}
	var out []processEditDiag
	failed := false
	var walk func(scope *model.Template, prefix string)
	walk = func(scope *model.Template, prefix string) {
		for _, nodeID := range slices.Sorted(maps.Keys(scope.Nodes)) {
			node := scope.Nodes[nodeID]
			if node.Type != model.NodeTypeSubprocess || node.Subprocess == nil {
				continue
			}
			sp := node.Subprocess
			target := prefix + nodeID
			if sp.IsInline() {
				walk(&model.Template{Nodes: sp.Nodes}, target+model.SubprocessNodeIDSeparator)
				continue
			}
			if sp.TemplateRef == "" {
				continue
			}
			found := templateRefNodeDiagnostics(tmpl.ID, scope, nodeID, target, resolve)
			if len(found) > 0 && found[0].Code == "subprocess_ref_unresolved" {
				failed = true
			}
			out = append(out, found...)
		}
	}
	walk(tmpl, "")
	// Nesting depth and the resolved-size budget are properties of the whole
	// tree: a reference can be fine on its own and still overflow it.
	if !failed {
		if _, err := model.ResolveSubprocessRefs(tmpl, resolve); err != nil {
			out = append(out, processEditDiag{
				Scope: "template", Severity: model.SeverityError, Code: "subprocess_ref_unresolved",
				Message: "resolving subprocess templates: " + err.Error(),
			})
		}
	}
	return out
}

// templateRefNodeDiagnostics checks one templateRef node. It validates a probe
// holding just that node (and stub ends for its outcomes) in its own scope,
// once as authored and once with the reference resolved; the diagnostics only
// the resolved probe raises are the reference's.
func templateRefNodeDiagnostics(rootID string, scope *model.Template, nodeID, target string, resolve model.TemplateResolver) []processEditDiag {
	sp := scope.Nodes[nodeID].Subprocess
	ref := sp.TemplateRef
	unresolved := func(message string) []processEditDiag {
		return []processEditDiag{{
			Scope: "node", TargetID: target, Severity: model.SeverityError,
			Code: "subprocess_ref_unresolved", Message: message, TemplateRef: ref,
		}}
	}
	if _, _, err := resolve(ref, sp.Version); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return unresolved(fmt.Sprintf("template %s does not exist or has no saved version", ref))
		}
		return unresolved(fmt.Sprintf("template %s cannot be loaded: %v", ref, err))
	}
	probe := &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: rootID,
		Params: scope.Params, Start: nodeID, Nodes: map[string]model.Node{nodeID: scope.Nodes[nodeID]},
	}
	for _, to := range scope.Nodes[nodeID].Next {
		if _, ok := probe.Nodes[to]; !ok {
			probe.Nodes[to] = model.Node{Type: model.NodeTypeEnd}
		}
	}
	resolved, err := model.ResolveSubprocessRefs(probe, resolve)
	if err != nil {
		// The reference itself loaded, so the failure is further down: a
		// reference cycle, nesting, size, or a template it references in turn.
		return unresolved(fmt.Sprintf("inside template %s: %s", ref, strings.TrimPrefix(err.Error(), fmt.Sprintf("subprocess %q: ", nodeID))))
	}
	authored := map[string]bool{}
	for _, diagnostic := range model.Validate(probe, model.NormalizeEdges(probe)) {
		authored[diagnostic.Code+"\x00"+diagnostic.Path] = true
	}
	inside := "nodes." + nodeID + ".subprocess.resolved."
	var out []processEditDiag
	for _, diagnostic := range model.Validate(resolved, model.NormalizeEdges(resolved)) {
		if authored[diagnostic.Code+"\x00"+diagnostic.Path] {
			continue
		}
		message := fmt.Sprintf("template %s: %s", ref, diagnostic.Message)
		if strings.HasPrefix(diagnostic.Path, inside) {
			message = fmt.Sprintf("inside template %s: %s", ref, diagnostic.Message)
		}
		out = append(out, processEditDiag{
			Scope: "node", TargetID: target, Severity: diagnostic.Severity,
			Code: diagnostic.Code, Message: message, TemplateRef: ref,
		})
	}
	return out
}
