package processimport

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"

	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/processimport/legacy"
)

// Binding is an explicit target identity/configuration choice, never a legacy
// name lookup. Source instruction text and argv are applied by the converter.
type Binding struct {
	Performer       model.Performer
	DecisionTimeout string
}

type Converted struct {
	Notices    []string
	Name       string
	Source     string
	Parameters []model.ParameterDeclaration
	Process    model.ProcessDefinition
	Layout     *model.DefinitionEditorLayout
	// ProgramExecutables must be checked against the exact pinned revisions by
	// the application before it exposes the draft. This package cannot read them.
	ProgramExecutables map[string]string
}

func Convert(source string, bindings map[string]Binding) (Converted, error) {
	parsed, err := parse(source)
	if err != nil {
		return Converted{}, err
	}
	if parsed.Template == nil || parsed.Diagnostics.HasErrors() {
		return Converted{}, fmt.Errorf("source has validation errors; inspect its diagnostics before conversion")
	}
	inspected, err := Inspect(source)
	if err != nil {
		return Converted{}, err
	}
	needed := map[string]bool{}
	for _, r := range inspected.Requirements {
		needed[r.Path] = true
		if _, ok := bindings[r.Path]; !ok {
			return Converted{}, fmt.Errorf("%s: choose an explicit %s mapping", r.Path, r.Kind)
		}
	}
	for path := range bindings {
		if !needed[path] {
			return Converted{}, fmt.Errorf("%s: mapping does not identify a source performer", path)
		}
	}
	t := parsed.Template
	if err := validateSourceNumbers(source, t); err != nil {
		return Converted{}, err
	}
	c := Converted{Name: t.Name, Source: source, Process: model.ProcessDefinition{ParameterSyntax: "mustache-v1", Graph: model.WorkGraph{CompilerVersion: "1", EntryNodeID: model.WorkNodeID(t.Start), Description: t.Description, Doc: t.Doc}}, Layout: &model.DefinitionEditorLayout{Nodes: map[model.WorkNodeID]model.EditorPosition{}}, ProgramExecutables: map[string]string{}}
	if c.Name == "" {
		c.Name = t.ID
	}
	keys := make([]string, 0, len(t.Params))
	for key := range t.Params {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		p := t.Params[key]
		var raw json.RawMessage
		if p.Default != nil {
			raw, err = json.Marshal(p.Default)
			if err != nil {
				return Converted{}, fmt.Errorf("parameter %s: %w", key, err)
			}
		}
		if err := exactEditorNumbers(raw); err != nil {
			return Converted{}, fmt.Errorf("parameter %s/default: %w", key, err)
		}
		c.Parameters = append(c.Parameters, model.ParameterDeclaration{Name: key, DisplayName: p.Name, Description: p.Description, Doc: p.Doc, Type: model.ParameterType(p.Type), Required: p.Required != nil && *p.Required, Default: raw})
	}
	ids := make([]string, 0, len(t.Nodes))
	for id := range t.Nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		n := t.Nodes[id]
		path := nodePath(id)
		if n.Join != "" {
			return Converted{}, fmt.Errorf("%s/join: structural join expansion is required before this source can be converted", path)
		}
		if len(n.Metadata) > 0 {
			c.Notices = append(c.Notices, path+": metadata is retained verbatim in Source; no metadata becomes authority")
		}
		node := model.WorkNode{ID: model.WorkNodeID(id), Name: n.Name, Description: n.Description, Doc: n.Doc, Captures: append([]string(nil), n.Captures...)}
		switch n.Type {
		case legacy.NodeTypeStart:
			node.Kind = model.WorkNodeStart
		case legacy.NodeTypeEnd:
			node.Kind = model.WorkNodeEnd
			outcome := model.WorkOutcomeVerified
			switch strings.ToLower(strings.TrimSpace(n.Result)) {
			case "fail", "failed", "failure", "error":
				outcome = model.WorkOutcomeRejected
			case "cancel", "canceled", "cancelled":
				outcome = model.WorkOutcomeCancelled
			}
			node.End = &model.EndPolicy{Outcome: outcome}
		case legacy.NodeTypeParallel:
			node.Kind = model.WorkNodeFork
		case legacy.NodeTypeWait:
			node.Kind = model.WorkNodeWait
			d, e := duration(n.Wait.Duration)
			if e != nil {
				return Converted{}, fmt.Errorf("%s/wait/duration: %w", path, e)
			}
			node.Wait = &model.WaitPolicy{Duration: d, Until: n.Wait.Until, Signal: n.Wait.Signal}
		case legacy.NodeTypeTask, legacy.NodeTypeDecision:
			performer, e := c.performer(path+"/performer", *n.Performer, bindings[path+"/performer"])
			if e != nil {
				return Converted{}, e
			}
			node.Kind = model.WorkNodeTask
			node.Performer = &performer
			if n.Type == legacy.NodeTypeDecision {
				node.Kind = model.WorkNodeDecision
				node.Performer = nil
				decision, e := decision(performer, bindings[path+"/performer"])
				if e != nil {
					return Converted{}, fmt.Errorf("%s/performer: %w", path, e)
				}
				decision.PermittedAnswers = append([]string(nil), n.Performer.Choices...)
				if len(decision.PermittedAnswers) == 0 {
					for answer := range n.Next {
						decision.PermittedAnswers = append(decision.PermittedAnswers, answer)
					}
					sort.Strings(decision.PermittedAnswers)
				}
				node.Decision = &decision
			}
			node.Retry, e = retry(n.Retry, performer.Kind)
			if e != nil {
				return Converted{}, fmt.Errorf("%s/retry: %w", path, e)
			}
			if n.IsCompound() {
				node.Stages = &model.TaskStages{}
				if n.Plan != nil {
					stage, e := c.stage(path+"/plan/performer", "plan", *n.Plan, bindings)
					if e != nil {
						return Converted{}, e
					}
					node.Stages.Plan = &stage
					if n.Plan.Approval == "human" {
						b := bindings[path+"/plan/approval"]
						approval, e := decision(b.Performer, b)
						if e != nil || b.Performer.Kind != model.PerformerHuman {
							return Converted{}, fmt.Errorf("%s/plan/approval: choose a human audience and positive decision timeout", path)
						}
						approval.Question = "Approve the plan?"
						approval.PermittedAnswers = []string{"approve", "rework"}
						node.Stages.PlanApproval = &approval
						if r := n.Plan.ApprovalRetry; r != nil {
							node.Stages.Plan.ApprovalRetry = &model.ApprovalRetryPolicy{MaxAttempts: model.ApprovalRetryAttempts(r.MaxAttempts), Backoff: r.Backoff, OnFail: r.OnFail}
						}
					}
				}
				for i, s := range n.Checks {
					stage, e := c.stage(fmt.Sprintf("%s/checks/%d/performer", path, i), fmt.Sprintf("check_%d", i+1), s, bindings)
					if e != nil {
						return Converted{}, e
					}
					node.Stages.Checks = append(node.Stages.Checks, stage)
				}
				if n.Review != nil {
					stage, e := c.stage(path+"/review/performer", "review", *n.Review, bindings)
					if e != nil {
						return Converted{}, e
					}
					node.Stages.Review = &stage
				}
			}
		default:
			return Converted{}, fmt.Errorf("%s: unsupported node kind", path)
		}
		c.Process.Graph.Nodes = append(c.Process.Graph.Nodes, node)
		if t.Layout != nil {
			if pos, ok := t.Layout.Nodes[id]; ok {
				c.Layout.Nodes[node.ID] = model.EditorPosition{X: pos.X, Y: pos.Y}
			}
		}
		outcomes := make([]string, 0, len(n.Next))
		for outcome := range n.Next {
			outcomes = append(outcomes, outcome)
		}
		sort.Strings(outcomes)
		if n.Type != legacy.NodeTypeDecision && n.Type != legacy.NodeTypeParallel && len(outcomes) > 0 {
			if len(outcomes) != 1 {
				return Converted{}, fmt.Errorf("%s/next: multiple verdict routes require explicit legacy routing support before conversion", path)
			}
			pass := false
			for _, label := range legacy.PassOutcomeLabels() {
				pass = pass || outcomes[0] == label
			}
			if !pass {
				return Converted{}, fmt.Errorf("%s/next: exact task verdict routing is not yet representable", path)
			}
		}
		for _, outcome := range outcomes {
			verdict := outcome
			if n.Type != legacy.NodeTypeDecision && n.Type != legacy.NodeTypeParallel {
				c.Notices = append(c.Notices, path+"/next/"+outcome+": represented as the default route; original spelling remains in Source")
				verdict = ""
			}
			edge := model.WorkEdge{From: node.ID, To: model.WorkNodeID(n.Next[outcome]), Verdict: verdict}
			c.Process.Graph.Edges = append(c.Process.Graph.Edges, edge)
			if t.Layout != nil {
				if label, ok := t.Layout.Edges[id][outcome]; ok && label.Pinned != nil {
					c.Layout.EdgeLabels = append(c.Layout.EdgeLabels, model.EditorEdgeLabel{Edge: edge, Pinned: *label.Pinned})
				}
			}
		}
	}
	return c, nil
}

func nodePath(id string) string {
	return "/nodes/" + strings.ReplaceAll(strings.ReplaceAll(id, "~", "~0"), "/", "~1")
}

func duration(value string) (time.Duration, error) {
	if strings.TrimSpace(value) == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(strings.TrimSpace(value))
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("positive Go duration required")
	}
	// The existing editor's numeric duration wire must not round imported intent.
	if d > 9007199254740991 {
		return 0, fmt.Errorf("duration needs exact editor wire support before conversion")
	}
	return d, nil
}

func retry(r *legacy.RetryPolicy, kind model.PerformerKind) (model.RetryPolicy, error) {
	if r == nil {
		return model.RetryPolicy{}, nil
	}
	d, err := duration(r.Backoff)
	if err != nil {
		return model.RetryPolicy{}, err
	}
	class := model.RetryableProgramFailure
	if kind == model.PerformerHuman {
		class = model.RetryableHumanRejection
	}
	if kind == model.PerformerAgent {
		class = model.RetryableAgentRejection
	}
	return model.RetryPolicy{MaxAttempts: model.RetryAttempts(r.MaxAttempts), Backoff: d, OnFail: r.OnFail, Retryable: []string{class}}, nil
}

func (c *Converted) performer(path string, p legacy.Performer, b Binding) (model.Performer, error) {
	// Copy target data so subsequent source overlays cannot mutate caller mappings.
	data, err := json.Marshal(b.Performer)
	if err != nil {
		return model.Performer{}, err
	}
	var result model.Performer
	if err = json.Unmarshal(data, &result); err != nil {
		return result, err
	}
	if string(result.Kind) != string(p.Kind) {
		return result, fmt.Errorf("%s: mapping kind differs from source", path)
	}
	if err := validateBinding(result); err != nil {
		return result, fmt.Errorf("%s: %w", path, err)
	}
	result.Timeout = p.Timeout
	if p.Contact != nil {
		if p.Contact.Budget < 0 || uint64(p.Contact.Budget) > uint64(^uint32(0)) {
			return result, fmt.Errorf("%s/contact: budget exceeds target range", path)
		}
		result.Contact = &model.ContactSchedule{Cadence: p.Contact.Cadence, Budget: uint32(p.Contact.Budget), EscalationTarget: p.Contact.EscalationTarget}
	} else {
		result.Contact = nil
	}
	switch result.Kind {
	case model.PerformerHuman:
		if result.Human == nil {
			return result, fmt.Errorf("%s: human audience required", path)
		}
		result.Human.Ask = p.Ask
		result.Human.Prompt = p.Prompt
		result.Human.Choices = append([]string(nil), p.Choices...)
		result.Human.ChoiceOutcomes = p.ChoiceOutcomes
	case model.PerformerAgent:
		if result.Agent == nil || result.Agent.CreateDesired == nil || result.Agent.AgentID != "" || result.Agent.MemberKey != "" {
			return result, fmt.Errorf("%s: choose an independent worker configuration", path)
		}
		result.Agent.AgentID = ""
		result.Agent.MemberKey = ""
		result.Agent.Brief = p.Prompt
		if p.Model != "" {
			result.Agent.CreateDesired.Model = p.Model
		}
		if p.Effort != "" {
			result.Agent.CreateDesired.Effort = p.Effort
		}
	case model.PerformerProgram:
		if result.Program == nil {
			return result, fmt.Errorf("%s: pinned program profile required", path)
		}
		result.Program.Arguments = append([]string(nil), p.Args...)
		result.Program.Input = nil
		if len(legacy.ParamReferences(p.Run)) > 0 {
			return result, fmt.Errorf("%s/run: parameterized executables require explicit runtime binding support before conversion", path)
		}
		c.ProgramExecutables[path] = p.Run
	default:
		return result, fmt.Errorf("%s: unsupported performer", path)
	}
	return result, nil
}

func (c *Converted) stage(path, fallback string, s legacy.Step, bindings map[string]Binding) (model.TaskStage, error) {
	p, err := c.performer(path, s.Performer, bindings[path])
	if err != nil {
		return model.TaskStage{}, err
	}
	r, err := retry(s.Retry, p.Kind)
	if err != nil {
		return model.TaskStage{}, err
	}
	id := s.ID
	if id == "" {
		id = fallback
	}
	return model.TaskStage{ID: id, Name: s.Name, Description: s.Description, Doc: s.Doc, Performer: p, Retry: r}, nil
}

func decision(p model.Performer, b Binding) (model.DecisionNode, error) {
	raw := p.Timeout
	if strings.TrimSpace(raw) == "" {
		raw = b.DecisionTimeout
	}
	timeout, err := duration(raw)
	if err != nil || timeout == 0 {
		return model.DecisionNode{}, fmt.Errorf("choose a positive decision timeout explicitly")
	}
	d := model.DecisionNode{Kind: model.DecisionWork, ExpiresAfter: timeout}
	if p.Kind != model.PerformerHuman {
		d.Decider = &p
		return d, nil
	}
	if err := validateBinding(p); err != nil {
		return d, err
	}
	d.Question = p.Human.Question()
	h := p.Human
	switch {
	case h.Operator:
		d.Audience = []model.DecisionAudience{{Subject: model.AuthoritySubject{Kind: model.AuthorityOperator}}}
	case h.AgentID != "":
		d.Audience = []model.DecisionAudience{{Subject: model.AuthoritySubject{Kind: model.AuthorityAgent, AgentID: h.AgentID}}}
	case h.RoleID != "":
		d.Audience = []model.DecisionAudience{{RoleID: h.RoleID}}
	default:
		return d, fmt.Errorf("human audience required")
	}
	return d, nil
}

func validateBinding(p model.Performer) error {
	count := 0
	if p.Agent != nil {
		count++
	}
	if p.Program != nil {
		count++
	}
	if p.Human != nil {
		count++
	}
	if count != 1 {
		return fmt.Errorf("choose exactly one typed performer binding")
	}
	switch p.Kind {
	case model.PerformerHuman:
		if p.Human == nil {
			return fmt.Errorf("human audience required")
		}
		h := p.Human
		identities := 0
		if h.Operator {
			identities++
		}
		if h.AgentID != "" {
			identities++
			if err := h.AgentID.Validate(); err != nil {
				return err
			}
		}
		if h.RoleID != "" {
			identities++
			if err := h.RoleID.Validate(); err != nil {
				return err
			}
		}
		if identities != 1 {
			return fmt.Errorf("choose exactly one human audience")
		}
	case model.PerformerAgent:
		if p.Agent == nil {
			return fmt.Errorf("worker configuration required")
		}
	case model.PerformerProgram:
		if p.Program == nil {
			return fmt.Errorf("program profile required")
		}
	default:
		return fmt.Errorf("unknown performer kind")
	}
	return nil
}

// Until the process editor has a lossless JSON-number representation, refuse
// drafts it would change simply by opening and saving. Exact source survives
// inspection; this is a conversion gap, not a replacement value.
func exactEditorNumbers(raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	var visit func(any) error
	visit = func(value any) error {
		switch v := value.(type) {
		case json.Number:
			// Compare decimal values after the browser's binary64 round trip,
			// including fractional/exponent tokens. Bound arbitrary-precision
			// work before constructing rationals from untrusted source.
			if len(v) > 4096 {
				return fmt.Errorf("number requires exact editor JSON support before conversion")
			}
			f, err := v.Float64()
			if err != nil {
				return fmt.Errorf("number requires exact editor JSON support before conversion")
			}
			if f == 0 {
				mantissa := strings.SplitN(strings.ToLower(string(v)), "e", 2)[0]
				if strings.Trim(mantissa, "-+0.") != "" {
					return fmt.Errorf("number requires exact editor JSON support before conversion")
				}
				return nil
			}
			encoded, err := json.Marshal(f)
			if err != nil {
				return fmt.Errorf("number requires exact editor JSON support before conversion")
			}
			original, ok := new(big.Rat).SetString(string(v))
			saved, savedOK := new(big.Rat).SetString(string(encoded))
			if !ok || !savedOK || original.Cmp(saved) != 0 {
				return fmt.Errorf("number requires exact editor JSON support before conversion")
			}

		case []any:
			for _, item := range v {
				if err := visit(item); err != nil {
					return err
				}
			}
		case map[string]any:
			for _, item := range v {
				if err := visit(item); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return visit(value)
}
