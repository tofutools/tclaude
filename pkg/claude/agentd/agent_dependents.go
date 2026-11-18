package agentd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
)

// Retiring or deleting an agent takes what exists only for it, and repairs
// what it shares, unless the operator keeps them (?keep_dependents=1, the
// CLI's --keep-dependents):
//
//   - its own cron jobs, standing orders and trigger rules, and the cron jobs
//     and standing orders addressed to it, are removed in the retirement's
//     own transaction (db.RetireOptions.Cascade);
//   - the runs scoped to it are cancelled, since they have nothing left to
//     run on;
//   - a template step pinned to it goes back to its role, or to nobody yet,
//     saved as a new version with a note saying why.
//
// Kept, a retirement disables the cron jobs and standing orders it owns, as
// it always has, and leaves the rest. What happens either way: a running step
// the agent held falls back to its If-nobody rule straight away rather than
// on the next sweep.

// agentDependentsKeep reports whether a retire or delete request keeps what
// refers to the agent. Absent or unparseable means cascade, the default.
func agentDependentsKeep(r *http.Request) bool {
	v := strings.TrimSpace(r.URL.Query().Get("keep_dependents"))
	keep, err := strconv.ParseBool(v)
	return err == nil && keep
}

// agentPinnedStep is a template step that names an agent as its assignee.
type agentPinnedStep struct {
	Template string `json:"template"`
	Node     string `json:"node"`
	// Slot is where on the node: "performer", "decider 2", or "check <id>".
	Slot string `json:"slot"`
}

// agentCascade is what a retire or delete did to what refers to the agent.
type agentCascade struct {
	Kept          bool                `json:"kept,omitempty"`
	Removed       []db.AgentDependent `json:"removed,omitempty"`
	RunsCancelled []db.AgentScopedRun `json:"runs_cancelled,omitempty"`
	StepsCleared  []agentPinnedStep   `json:"steps_cleared,omitempty"`
	Warnings      []string            `json:"warnings,omitempty"`
}

// agentDependentsPreview is what a retire or delete of one agent would take
// or repair, for the confirmation dialogs.
type agentDependentsPreview struct {
	ConvID      string              `json:"conv_id"`
	AgentID     string              `json:"agent_id"`
	Dependents  []db.AgentDependent `json:"dependents,omitempty"`
	Runs        []db.AgentScopedRun `json:"runs,omitempty"`
	PinnedSteps []agentPinnedStep   `json:"pinned_steps,omitempty"`
	Unavailable []string            `json:"unavailable,omitempty"`
}

// cascadeActor is who a cascade's run cancellations and template saves are
// attributed to: the operator, or the agent that asked. A system retirement
// (a sweep) has no actor that may edit a template, so it leaves them.
func cascadeActor(by string) (store.ActorRef, bool) {
	if by == "" || by == "human" {
		return store.ActorRef("human:operator"), true
	}
	if strings.HasPrefix(by, "system:") {
		return "", false
	}
	if agentID := peerAgentID(by); agentID != "" {
		return store.ActorRef("agent:" + agentID), true
	}
	return "", false
}

// cascadeCallerIsAgent reports whether a retirement was asked for by an
// agent rather than the operator or the daemon. An agent's cascade reaches
// only what the target owns: it leaves other people's schedules addressed to
// the target, cancels runs only with process.runs.manage, and clears template
// steps only with process.templates.manage.
func cascadeCallerIsAgent(by string) bool {
	return by != "" && by != "human" && !strings.HasPrefix(by, "system:")
}

// cascadeAgentMay reports whether an agent caller holds slug for the cascade.
func cascadeAgentMay(conv, slug string) bool {
	allowed, err := durableOwnerAllows("retire", conv, slug, ActionContext{})
	return err == nil && allowed
}

// agentCascadeTemplateMu serializes clearing pinned template steps. A bulk
// retire runs its members concurrently, and a template that pins several of
// them would otherwise be read by each and saved by only the first; the
// rest would lose the compare-and-swap.
var agentCascadeTemplateMu sync.Mutex

// agentAssigneeMatcher returns a test for whether an assignee names agentID:
// its stable id, any of its conversations, or its name when that name no
// longer reaches any other active agent. Taken before the agent goes, so the
// name and conversations are still known.
func agentAssigneeMatcher(agentID string) func(string) bool {
	convs, _ := db.ConvsForAgent(agentID)
	ids := map[string]bool{agentID: true}
	name := ""
	for _, conv := range convs {
		ids[conv] = true
	}
	if head, _ := db.CurrentConvForAgent(agentID); head != "" {
		name = strings.TrimSpace(agent.FreshTitle(head))
	}
	return func(assignee string) bool {
		assignee = strings.TrimSpace(assignee)
		if assignee == "" {
			return false
		}
		if ids[assignee] {
			return true
		}
		if name == "" || !strings.EqualFold(assignee, name) {
			return false
		}
		// The name is this agent's only while it still resolves to it alone:
		// a name another agent also answers to (ambiguous), or one that cannot
		// be looked up right now, is left as written.
		resolved, _, err := agent.ResolveSelector(assignee)
		return err == nil && resolved.AgentID == agentID
	}
}

// pinnedSteps walks tmpl's agent performers, calling visit with each one's
// slot; visit may clear the assignee.
func pinnedSteps(tmpl *model.Template, visit func(node, slot string, p *model.Performer)) {
	for id, node := range tmpl.Nodes {
		changed := false
		touch := func(slot string, p *model.Performer) {
			if p == nil || p.Kind != model.PerformerAgent || strings.TrimSpace(p.Assignee) == "" {
				return
			}
			before := p.Assignee
			visit(id, slot, p)
			changed = changed || p.Assignee != before
		}
		if node.Performer != nil {
			performer := *node.Performer
			touch("performer", &performer)
			node.Performer = &performer
		}
		deciders := append([]model.Performer(nil), node.Deciders...)
		for i := range deciders {
			touch(fmt.Sprintf("decider %d", i+1), &deciders[i])
		}
		node.Deciders = deciders
		checks := append([]model.Step(nil), node.Checks...)
		for i := range checks {
			slot := "check " + checks[i].ID
			if checks[i].ID == "" {
				slot = fmt.Sprintf("check %d", i+1)
			}
			touch(slot, &checks[i].Performer)
		}
		node.Checks = checks
		if changed {
			tmpl.Nodes[id] = node
		}
	}
}

// templatesPinnedTo lists every saved template's steps pinned to the agent,
// with each template's parsed editor source when clear is set.
func templatesPinnedTo(ctx context.Context, fs *store.FS, matches func(string) bool, clear func(*model.ParsedTemplate, []agentPinnedStep)) ([]agentPinnedStep, error) {
	heads, err := fs.ListTemplateHeads(ctx)
	if err != nil {
		return nil, err
	}
	var out []agentPinnedStep
	for _, head := range heads {
		source, err := fs.GetTemplateSource(ctx, head.Ref)
		if err != nil {
			continue
		}
		parsed, err := model.Parse(source)
		if err != nil || parsed.Template == nil {
			continue
		}
		var found []agentPinnedStep
		pinnedSteps(parsed.Template, func(node, slot string, p *model.Performer) {
			if matches(p.Assignee) {
				found = append(found, agentPinnedStep{Template: parsed.Template.ID, Node: node, Slot: slot})
				if clear != nil {
					p.Assignee = ""
				}
			}
		})
		if len(found) == 0 {
			continue
		}
		if clear != nil {
			clear(parsed, found)
		}
		out = append(out, found...)
	}
	return out, nil
}

// previewAgentDependents is what retiring or deleting convID's agent would
// take or repair.
func previewAgentDependents(ctx context.Context, convID string) (agentDependentsPreview, error) {
	preview := agentDependentsPreview{ConvID: convID}
	agentID, err := db.AgentIDForConv(convID)
	if err != nil || agentID == "" {
		return preview, err
	}
	preview.AgentID = agentID
	if preview.Dependents, err = db.ListAgentDependents(agentID); err != nil {
		return preview, err
	}
	if preview.Runs, err = db.ListAgentScopedActiveRuns(agentID); err != nil {
		return preview, err
	}
	fs, err := store.NewFS(processStoreRoot())
	if err != nil {
		preview.Unavailable = append(preview.Unavailable, "process templates: "+err.Error())
		return preview, nil
	}
	steps, err := templatesPinnedTo(ctx, fs, agentAssigneeMatcher(agentID), nil)
	if err != nil {
		preview.Unavailable = append(preview.Unavailable, "process templates: "+err.Error())
	}
	preview.PinnedSteps = steps
	return preview, nil
}

// finishAgentCascade does what a retirement's transaction cannot: it
// withdraws hook declarations the removed standing orders needed, cancels the
// agent's runs, clears its pinned template steps, and has the steps it held
// fall back. matches is agentAssigneeMatcher, taken before the retirement.
func finishAgentCascade(ctx context.Context, agentID, by, verb string, keep bool, removed []db.AgentDependent, matches func(string) bool) agentCascade {
	out := agentCascade{Kept: keep, Removed: removed}
	defer kickAgentProcessDispatches(agentID)
	if keep || agentID == "" {
		return out
	}
	harnesses := map[string]bool{}
	for _, dep := range removed {
		for _, h := range dep.Harnesses {
			harnesses[h] = true
		}
	}
	if len(harnesses) > 0 {
		if warning := reconcileStandingOrderHookHarnesses(harnesses); warning != "" {
			out.Warnings = append(out.Warnings, warning)
		}
	}
	actor, canAct := cascadeActor(by)
	cancelActor := string(actor)
	if !canAct {
		cancelActor = by
	}
	note := "the agent it was scoped to was " + verb
	runs, err := db.ListAgentScopedActiveRuns(agentID)
	if err != nil {
		out.Warnings = append(out.Warnings, "list the agent's runs: "+err.Error())
	}
	if cascadeCallerIsAgent(by) && !cascadeAgentMay(by, PermProcessRunsManage) {
		for _, run := range runs {
			out.Warnings = append(out.Warnings, "run "+run.ID+" is left parked: cancelling it needs "+PermProcessRunsManage)
		}
		runs = nil
	}
	for _, run := range runs {
		if _, err := processRuns.submit(run.ID, cancelActor, processRunInput{cancel: &processRunCancelInput{note: note}}); err != nil {
			out.Warnings = append(out.Warnings, fmt.Sprintf("cancel run %s: %v", run.ID, err))
			continue
		}
		kickProcessAgentDispatch(run.ID)
		out.RunsCancelled = append(out.RunsCancelled, run)
	}
	if !canAct || matches == nil {
		return out
	}
	if cascadeCallerIsAgent(by) && !cascadeAgentMay(by, PermProcessTemplatesManage) {
		return out
	}
	agentCascadeTemplateMu.Lock()
	defer agentCascadeTemplateMu.Unlock()
	fs, err := store.NewFS(processStoreRoot())
	if err != nil {
		out.Warnings = append(out.Warnings, "process templates: "+err.Error())
		return out
	}
	label := agentID
	if head, _ := db.CurrentConvForAgent(agentID); head != "" {
		if title := strings.TrimSpace(agent.FreshTitle(head)); title != "" {
			label = title
		}
	}
	saveNote := fmt.Sprintf("cleared %s after it was %s", label, verb)
	_, err = templatesPinnedTo(ctx, fs, matches, func(parsed *model.ParsedTemplate, steps []agentPinnedStep) {
		id := parsed.Template.ID
		if actor != "human:operator" && db.GroupResourceLocked(resourceKindProcess, id) {
			out.Warnings = append(out.Warnings, "template "+id+" is locked by the operator; its steps still name the agent")
			return
		}
		// The editor's own save checks: never rewrite a template whose source
		// the model cannot hold, or one past its graph budgets.
		if parsed.Diagnostics.HasSourceFidelityError() || parsed.Diagnostics.HasNormalizedGraphBudgetError() ||
			model.PreflightNormalizedGraphCardinality(parsed.Template).HasErrors() {
			out.Warnings = append(out.Warnings, "template "+id+" needs fixing in the editor first; its steps still name the agent")
			return
		}
		_, err := fs.PutTemplateEditorSourceNoted(ctx, parsed.Template, parsed.SourceHash, actor, saveNote)
		var conflict *store.TemplateSourceConflictError
		switch {
		case errors.As(err, &conflict):
			out.Warnings = append(out.Warnings, "template "+id+" changed while clearing it; its steps still name the agent")
		case err != nil:
			out.Warnings = append(out.Warnings, "template "+id+": "+err.Error())
		default:
			out.StepsCleared = append(out.StepsCleared, steps...)
		}
	})
	if err != nil {
		out.Warnings = append(out.Warnings, "process templates: "+err.Error())
	}
	return out
}

// kickAgentProcessDispatches has every step the agent holds re-resolve now:
// a retired or deleted performer is released and the step falls back.
func kickAgentProcessDispatches(agentID string) {
	if agentID == "" {
		return
	}
	dispatches, err := db.ListProcessAgentDispatchesForAgent(agentID, true, db.MaxProcessDispatchReadPage)
	if err != nil {
		slog.Warn("retire: list the agent's process steps", "agent", agentID, "error", err)
		return
	}
	seen := map[string]bool{}
	for _, d := range dispatches {
		if !seen[d.RunID] {
			seen[d.RunID] = true
			kickProcessAgentDispatch(d.RunID)
		}
	}
}

// summary is the cascade in a few words for a result table: "removed 2 cron
// job(s), cancelled 1 run(s)". Empty when it changed nothing.
func (c *agentCascade) summary() string {
	if c == nil || c.Kept {
		return ""
	}
	counts := map[string]int{}
	for _, dep := range c.Removed {
		counts[dep.Kind]++
	}
	var parts []string
	for _, kind := range []struct{ key, words string }{
		{db.AgentDependentCron, "cron job(s)"},
		{db.AgentDependentStandingOrder, "standing order(s)"},
		{db.AgentDependentTriggerRule, "automation(s)"},
	} {
		if n := counts[kind.key]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, kind.words))
		}
	}
	var out []string
	if len(parts) > 0 {
		out = append(out, "removed "+strings.Join(parts, ", "))
	}
	if n := len(c.RunsCancelled); n > 0 {
		out = append(out, fmt.Sprintf("cancelled %d run(s)", n))
	}
	if n := len(c.StepsCleared); n > 0 {
		out = append(out, fmt.Sprintf("cleared %d template step(s)", n))
	}
	return strings.Join(out, ", ")
}

// cascadeAgentDelete is the retirement half of deleting an agent: the same
// demotion and cascade a retire does, so a deleted agent's board tasks are
// unassigned with a reason, its Steward seat ends and what refers to it is
// removed or repaired before its rows go. It runs only for an agent's live
// generation; deleting an older one leaves the actor, and everything that
// refers to it, alive. Safe on an agent already retired.
func cascadeAgentDelete(convID, by string, keep bool) (*agentCascade, error) {
	agentID, err := db.AgentIDForConv(convID)
	if err != nil || agentID == "" {
		return nil, err
	}
	if head, _ := db.CurrentConvForAgent(agentID); head != convID {
		return nil, nil
	}
	out, _, err := retireAgentConvCascadingAs(convID, by, "deleted", false, nil, keep, "deleted")
	if err != nil {
		return nil, err
	}
	return out.Dependents, nil
}

// dashboardAgentDependents serves GET /api/agents/{selector}/dependents: what
// retiring or deleting the agent would remove or repair, for the
// confirmation dialogs' "also remove/repair these" row.
func dashboardAgentDependents(w http.ResponseWriter, r *http.Request, convSelector string) {
	convID := convSelector
	if res, _, err := agent.ResolveSelector(convSelector); err == nil {
		convID = res.ConvID
	} else if !looksLikeConvID(convSelector) {
		http.Error(w, "resolve agent: "+err.Error(), http.StatusNotFound)
		return
	}
	preview, err := previewAgentDependents(r.Context(), convID)
	if err != nil {
		http.Error(w, "list dependents: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, preview)
}
