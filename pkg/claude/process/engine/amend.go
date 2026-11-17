package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

// Amending a run (tk_e2fa2bd0 3c): the run gets its own copy of its template,
// changed only where the run has not been yet.
//
// A node of the template's own graph has STARTED once any prepared node of it
// (its compound stages, gate votes and subprocess body included) has left
// pending. An amendment may add, remove, change and reroute steps that have
// not started, reroute a started step none of whose ways out has settled yet
// (insert a step after the one being worked on), and change template-level
// settings. Every started step otherwise stays exactly as it was, which is
// what lets the checkpoint carry over by node id. To change a started step, send the run back to
// before it or park it first.
//
// An amendment never adds or changes a program step or a subprocess: host
// programs run under the profiles the run was started with, and a subprocess
// pulls in another template.

// ErrAmendmentRefused refuses an amendment the run cannot take.
var ErrAmendmentRefused = errors.New("process run amendment refused")

// AmendSummary is an amendment's change to the run's template, by authored
// step id, sorted.
type AmendSummary struct {
	Added    []string `json:"added,omitempty"`
	Removed  []string `json:"removed,omitempty"`
	Changed  []string `json:"changed,omitempty"`
	Rerouted []string `json:"rerouted,omitempty"`
	Settings bool     `json:"settings,omitempty"`
}

// Empty reports whether the amendment changes nothing.
func (s AmendSummary) Empty() bool {
	return len(s.Added)+len(s.Removed)+len(s.Changed)+len(s.Rerouted) == 0 && !s.Settings
}

// sameJSON compares two values by their JSON form, so an empty list or map
// and an absent one (as a YAML round trip may give) count as the same.
func sameJSON(a, b any) bool {
	x, errX := json.Marshal(a)
	y, errY := json.Marshal(b)
	empty := func(raw []byte) bool { v := string(raw); return v == "null" || v == "{}" || v == "[]" }
	return errX == nil && errY == nil && (string(x) == string(y) || (empty(x) && empty(y)))
}

// DiffTemplates summarises how amended differs from current, by step.
func DiffTemplates(current, amended *model.Template) AmendSummary {
	var s AmendSummary
	for id, node := range amended.Nodes {
		old, ok := current.Nodes[id]
		switch {
		case !ok:
			s.Added = append(s.Added, id)
		default:
			oldNext, newNext := old.Next, node.Next
			old.Next, node.Next = nil, nil
			if !sameJSON(old, node) {
				s.Changed = append(s.Changed, id)
			}
			if !sameJSON(oldNext, newNext) {
				s.Rerouted = append(s.Rerouted, id)
			}
		}
	}
	for id := range current.Nodes {
		if _, ok := amended.Nodes[id]; !ok {
			s.Removed = append(s.Removed, id)
		}
	}
	// Where the editor drew the steps is not a change to the run.
	a, b := *current, *amended
	a.Nodes, b.Nodes = nil, nil
	a.Layout, b.Layout = nil, nil
	s.Settings = !sameJSON(a, b)
	for _, list := range [][]string{s.Added, s.Removed, s.Changed, s.Rerouted} {
		slices.Sort(list)
	}
	return s
}

// authoredRoot is the authored step a prepared node id belongs to.
func authoredRoot(nodeID string) string {
	if i := strings.IndexAny(nodeID, "./"); i >= 0 {
		return nodeID[:i]
	}
	return nodeID
}

// StartedSteps are the authored steps of a run that have started.
func StartedSteps(checkpoint Checkpoint) map[string]bool {
	started := map[string]bool{}
	for id, status := range checkpoint.Nodes {
		if status != NodePending {
			started[authoredRoot(id)] = true
		}
	}
	return started
}

// ReroutableSteps are the started steps an amendment may still reroute: none
// of their ways out has settled yet.
func ReroutableSteps(checkpoint Checkpoint) map[string]bool {
	out := map[string]bool{}
	settled := map[string]bool{}
	for from, outcomes := range checkpoint.Edges {
		for _, disposition := range outcomes {
			if disposition != EdgeUnresolved {
				settled[authoredRoot(from)] = true
			}
		}
	}
	for id := range StartedSteps(checkpoint) {
		if !settled[id] {
			out[id] = true
		}
	}
	return out
}

// Amend carries a running run's checkpoint over to its amended template. It
// returns the new checkpoint and what changed, or ErrAmendmentRefused saying
// why the run cannot take the amendment.
func Amend(checkpoint Checkpoint, oldDef *Definition, current, amended *model.Template, newDef *Definition) (Checkpoint, AmendSummary, error) {
	refuse := func(format string, args ...any) (Checkpoint, AmendSummary, error) {
		return Checkpoint{}, AmendSummary{}, fmt.Errorf("%w: %s", ErrAmendmentRefused, fmt.Sprintf(format, args...))
	}
	if checkpoint.Status != RunRunning || doomed(checkpoint) {
		return refuse("the run is not running")
	}
	if amended.ID != current.ID || amended.Start != current.Start {
		return refuse("an amendment keeps the template's id and start")
	}
	if !sameJSON(amended.Params, current.Params) {
		return refuse("an amendment keeps the run's params")
	}
	summary := DiffTemplates(current, amended)
	if summary.Empty() {
		return refuse("the amendment changes nothing")
	}
	started := StartedSteps(checkpoint)
	// A step that ran before (and was reset by a send-back or a new loop
	// round) keeps its attempt numbers: removing it would lose them, and
	// adding it back would reuse attempt ids already handed out.
	ran := map[string]bool{}
	for id := range checkpoint.Attempts {
		ran[authoredRoot(id)] = true
	}
	for _, id := range summary.Removed {
		if ran[id] {
			return refuse("step %s has run before in this run, so it cannot be removed; reroute around it instead", id)
		}
	}
	for _, id := range slices.Concat(summary.Changed, summary.Removed) {
		if started[id] {
			return refuse("step %s has started, so it stays as it is; send the run back to before it, or park it, and amend again", id)
		}
	}
	// A started step may still be rerouted while none of its ways out has
	// settled: where it goes next is not decided until it finishes.
	for _, id := range summary.Rerouted {
		if !started[id] {
			continue
		}
		for from, outcomes := range checkpoint.Edges {
			if authoredRoot(from) != id {
				continue
			}
			for _, disposition := range outcomes {
				if disposition != EdgeUnresolved {
					return refuse("step %s has already gone on, so where it leads stays as it is; send the run back to it and amend again", id)
				}
			}
		}
	}
	for _, id := range slices.Concat(summary.Added, summary.Changed) {
		node := amended.Nodes[id]
		if node.Type == model.NodeTypeSubprocess || node.Subprocess != nil {
			return refuse("step %s is a subprocess, which an amendment cannot add or change", id)
		}
		if raw, err := json.Marshal(node); err != nil || strings.Contains(string(raw), `"kind":"program"`) {
			return refuse("step %s runs a program, which an amendment cannot add or change: programs run under the profiles the run was started with", id)
		}
	}

	// A rule reads what the run recorded of a step; a step that already ran
	// with nothing recording it has no record for a new rule to read.
	for _, node := range newDef.nodes {
		if !node.recorded {
			continue
		}
		if index, ok := oldDef.index[node.id]; ok && oldDef.nodes[index].recorded {
			continue
		}
		if checkpoint.Attempts[node.id] > 0 {
			return refuse("a rule would read step %s, which has already run with nothing recording it; send the run back to before it and amend again", authoredRoot(node.id))
		}
	}

	oldTargets := map[string]map[string]string{}
	for _, edge := range oldDef.edges {
		if oldTargets[edge.from] == nil {
			oldTargets[edge.from] = map[string]string{}
		}
		oldTargets[edge.from][edge.outcome] = edge.to
	}
	next := Checkpoint{
		Version: checkpoint.Version, RunID: checkpoint.RunID, Status: checkpoint.Status,
		Nodes:             make(map[string]NodeStatus, len(newDef.nodes)),
		Edges:             make(map[string]map[string]EdgeDisposition),
		AwaitingDecisions: slices.Clone(checkpoint.AwaitingDecisions),
		Commands:          slices.Clone(checkpoint.Commands),
		Blocked:           slices.Clone(checkpoint.Blocked),
		Canceled:          checkpoint.Canceled,
	}
	for _, node := range newDef.nodes {
		status, ok := checkpoint.Nodes[node.id]
		if !ok {
			status = NodePending
		}
		next.Nodes[node.id] = status
		if n, ok := checkpoint.Attempts[node.id]; ok {
			if next.Attempts == nil {
				next.Attempts = map[string]int{}
			}
			next.Attempts[node.id] = n
		}
		if n, ok := checkpoint.AttemptCeilings[node.id]; ok && n > node.maxAttempts {
			if next.AttemptCeilings == nil {
				next.AttemptCeilings = map[string]int{}
			}
			next.AttemptCeilings[node.id] = n
		}
	}
	for _, edge := range newDef.edges {
		if next.Edges[edge.from] == nil {
			next.Edges[edge.from] = map[string]EdgeDisposition{}
		}
		disposition := EdgeUnresolved
		if oldTargets[edge.from][edge.outcome] == edge.to {
			if d, ok := checkpoint.Edges[edge.from][edge.outcome]; ok {
				disposition = d
			}
		}
		next.Edges[edge.from][edge.outcome] = disposition
	}
	for _, loop := range newDef.loops {
		from := newDef.nodes[loop.from].id
		if n, ok := checkpoint.Iterations[from][loop.outcome]; ok {
			if next.Iterations == nil {
				next.Iterations = map[string]map[string]int{}
			}
			if next.Iterations[from] == nil {
				next.Iterations[from] = map[string]int{}
			}
			next.Iterations[from][loop.outcome] = n
		}
	}
	// A step that has not started must still have something to wait on: an
	// edge into it that is not settled yet. One whose every way in is already
	// settled would never move.
	for index, node := range newDef.nodes {
		if index == 0 || node.parent >= 0 || node.scope >= 0 || next.Nodes[node.id] != NodePending {
			continue
		}
		// A decision-first loop's body step waits on its decision instead.
		if len(node.incoming) == 0 && dormantLoopNode(newDef, index) {
			continue
		}
		waiting := false
		for _, edgeIndex := range candidateEdges(next, newDef, index) {
			edge := newDef.edges[edgeIndex]
			if next.Edges[edge.from][edge.outcome] == EdgeUnresolved {
				waiting = true
				break
			}
		}
		if !waiting {
			return refuse("step %s would have nothing left to wait on: every way into it is already settled", node.id)
		}
	}
	if err := ValidateCheckpoint(next, newDef); err != nil {
		return refuse("the run's state does not carry over: %v", err)
	}
	return next, summary, nil
}
