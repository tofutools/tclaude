package agentd

import (
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/strictjson"
)

// processTemplateOversight is what a run's template says about oversight:
// who oversees its runs by default, and how long its steps may stay stuck.
type processTemplateOversight struct {
	Overseer     string
	Patience     time.Duration
	NodePatience map[string]time.Duration
}

// processDefaultPatience is how long a step may stay stuck when neither the
// step nor its template says.
const processDefaultPatience = 30 * time.Minute

// nodePatience is a step's own patience; a stage of a step (work.plan,
// work.review) takes its step's.
func (t processTemplateOversight) nodePatience(nodeID string) (time.Duration, bool) {
	if d, ok := t.NodePatience[nodeID]; ok {
		return d, true
	}
	if base, _, ok := strings.Cut(nodeID, "."); ok {
		d, ok := t.NodePatience[base]
		return d, ok
	}
	return 0, false
}

// explicitPatience reports whether the step or its template sets a patience.
func (t processTemplateOversight) explicitPatience(nodeID string) bool {
	_, ok := t.nodePatience(nodeID)
	return ok || t.Patience > 0
}

// patienceFor is how long nodeID may stay stuck.
func (t processTemplateOversight) patienceFor(nodeID string) time.Duration {
	if d, ok := t.nodePatience(nodeID); ok {
		return d
	}
	if t.Patience > 0 {
		return t.Patience
	}
	return processDefaultPatience
}

// processTemplateOversightCache holds each run's template oversight: a run's
// template snapshot never changes, and the overseer is resolved on every
// poll of the run view.
var processTemplateOversightCache sync.Map // run id → processTemplateOversight

func processRunTemplateOversight(runID string) processTemplateOversight {
	if v, ok := processTemplateOversightCache.Load(runID); ok {
		return v.(processTemplateOversight)
	}
	record, err := db.GetProcessRun(runID)
	if err != nil || record == nil {
		return processTemplateOversight{}
	}
	var tmpl model.Template
	if err := strictjson.Decode(record.TemplateSnapshotJSON, &tmpl); err != nil {
		return processTemplateOversight{}
	}
	out := processTemplateOversight{Overseer: strings.TrimSpace(tmpl.Overseer), NodePatience: map[string]time.Duration{}}
	if d, err := time.ParseDuration(strings.TrimSpace(tmpl.Patience)); err == nil && d > 0 {
		out.Patience = d
	}
	for id, node := range tmpl.Nodes {
		if d, err := time.ParseDuration(strings.TrimSpace(node.Patience)); err == nil && d > 0 {
			out.NodePatience[id] = d
		}
	}
	processTemplateOversightCache.Store(runID, out)
	return out
}

// processTemplateOverseer resolves a template's overseer selector for a run:
// ok is false when the selector names nobody (no member has the role, or
// the template takes the default), so resolution falls through.
func processTemplateOverseer(selector string, scope db.ProcessRunScope) (processOverseer, bool) {
	switch {
	case selector == model.OverseerOperator:
		return processOverseer{Ref: processOwnerOperator, Via: processOverseerViaTemplate}, true
	case selector == model.OverseerStarter:
		if o, ok := processOverseerOfRef(scope.Owner, processOverseerViaTemplate); ok {
			return o, true
		}
	case strings.HasPrefix(selector, model.OverseerRole):
		role := strings.TrimPrefix(selector, model.OverseerRole)
		if scope.Kind != db.ProcessRunScopeGroup {
			return processOverseer{}, false
		}
		for _, member := range processOversightMembers(scope.GroupID) {
			if strings.EqualFold(member.Role, role) {
				return processOverseer{Ref: processAgentRef(member.AgentID), AgentID: member.AgentID, Via: processOverseerViaTemplate}, true
			}
		}
	}
	return processOverseer{}, false
}
