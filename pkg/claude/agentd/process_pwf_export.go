package agentd

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/pwf"
)

// An export can make a workflow more portable: steps naming a specific agent
// can go out with a role instead (assignee=<agent>=<role>, or assignees=roles
// for the role each agent holds here), and strict=1 refuses what remains. Only
// the file changes, never the stored templates.

// pwfAssigneeView is one agent the workflow names. Role is what the agent
// would become: the role it holds here, when it holds exactly one.
type pwfAssigneeView struct {
	Name  string   `json:"name"`
	Steps []string `json:"steps"`
	Role  string   `json:"role,omitempty"`
}

// pwfAssigneeRole is what a named agent's steps become. Held marks the role the
// agent holds here, as opposed to one the exporter chose.
type pwfAssigneeRole struct {
	Role string
	Held bool
}

// pwfPerRun reports a value picked per run from a param: not a named agent.
func pwfPerRun(name string) bool { return strings.Contains(name, "{{") }

func pwfNamedAgents(b pwf.Bundle) ([]pwf.Use, error) {
	uses, err := pwf.Assignees(b)
	if err != nil {
		return nil, err
	}
	named := make([]pwf.Use, 0, len(uses))
	for _, use := range uses {
		if !pwfPerRun(use.Name) {
			named = append(named, use)
		}
	}
	return named, nil
}

func pwfAssigneesWithRoles(r *http.Request, b pwf.Bundle) ([]pwfAssigneeView, error) {
	named, err := pwfNamedAgents(b)
	if err != nil {
		return nil, err
	}
	views := make([]pwfAssigneeView, 0, len(named))
	if len(named) == 0 {
		return views, nil
	}
	roles, err := pwfAgentRoles(r)
	if err != nil {
		return nil, err
	}
	for _, use := range named {
		views = append(views, pwfAssigneeView{Name: use.Name, Steps: use.Steps, Role: roles(use.Name)})
	}
	return views, nil
}

// pwfAgentRoles returns a lookup of the single role an agent (by name, agent
// id or conversation id, like a step's assignee) holds in the groups the
// caller can see, or "" when it holds none or several.
func pwfAgentRoles(r *http.Request) (func(string) string, error) {
	var members []processPerformerCandidate
	if _, err := os.Stat(db.DBPath()); err == nil {
		all, visible := pwfRoleVisibility(r)
		groups, err := db.ListAgentGroups()
		if err != nil {
			return nil, err
		}
		for _, group := range groups {
			if group.IsArchived() || (!all && !visible[group.ID]) {
				continue
			}
			rows, err := db.ListAgentGroupMembers(group.ID)
			if err != nil {
				return nil, err
			}
			for _, row := range rows {
				agentID := peerAgentID(row.ConvID)
				if performer, err := db.GetAgent(agentID); strings.TrimSpace(row.Role) == "" || err != nil || !performer.Active() {
					continue
				}
				members = append(members, processPerformerCandidate{AgentID: agentID, ConvID: row.ConvID, Name: agent.FreshTitle(row.ConvID), Role: strings.TrimSpace(row.Role)})
			}
		}
	}
	return func(name string) string {
		role := ""
		for _, member := range members {
			if processCandidateMatches(member, name) {
				if role != "" && role != member.Role {
					return ""
				}
				role = member.Role
			}
		}
		return role
	}, nil
}

// pwfExportRoles reads which agents the export gives a role. An explicit
// assignee= wins over assignees=roles; an agent neither names keeps its name.
func pwfExportRoles(r *http.Request, b pwf.Bundle) (map[string]pwfAssigneeRole, error) {
	q := r.URL.Query()
	if len(q["assignee"]) == 0 && q.Get("assignees") != "roles" {
		return nil, nil
	}
	named, err := pwfNamedAgents(b)
	if err != nil {
		return nil, err
	}
	known := map[string]bool{}
	for _, use := range named {
		known[use.Name] = true
	}
	roles := map[string]pwfAssigneeRole{}
	for _, m := range q["assignee"] {
		from, to, ok := strings.Cut(m, "=")
		from, to = strings.TrimSpace(from), strings.TrimSpace(to)
		if !ok || from == "" || to == "" || !pwfLocalNameOK(to) {
			return nil, fmt.Errorf("assignee role %q is not <agent>=<role>", m)
		}
		if !known[from] {
			return nil, fmt.Errorf("no step names the agent %q", from)
		}
		roles[from] = pwfAssigneeRole{Role: to}
	}
	if q.Get("assignees") == "roles" {
		held, err := pwfAgentRoles(r)
		if err != nil {
			return nil, err
		}
		for _, use := range named {
			if _, chosen := roles[use.Name]; !chosen && held(use.Name) != "" {
				roles[use.Name] = pwfAssigneeRole{Role: held(use.Name), Held: true}
			}
		}
	}
	return roles, nil
}

// pwfConvertAssignees rewrites a bundle so the given agents' steps no longer
// name them. It goes through the import planner, which rewrites templates
// consistently: a subprocess step pinned to a rewritten version follows it.
func pwfConvertAssignees(b pwf.Bundle, roles map[string]pwfAssigneeRole) (pwf.Bundle, error) {
	if len(roles) == 0 {
		return b, nil
	}
	var file bytes.Buffer
	if err := pwf.Write(&file, b, pwf.WriteOptions{}); err != nil {
		return b, err
	}
	a, err := pwf.Open(bytes.NewReader(file.Bytes()), int64(file.Len()), pwf.DefaultLimits())
	if err != nil {
		return b, err
	}
	planner := &pwfPlanner{a: a, assignees: roles, done: map[string]*pwfPlanned{}, active: map[string]bool{}}
	// Everything but the templates carries over, fields added later included.
	converted := b
	converted.Root, converted.Templates = "", nil
	seen := map[string]int{}
	for _, ref := range a.Order {
		planned, err := planner.plan(ref)
		if err != nil {
			return b, err
		}
		if ref == a.Manifest.Root {
			converted.Root = planned.Ref
		}
		// Two versions can become one once their assignees are gone.
		if i, dup := seen[planned.Ref]; dup {
			converted.Templates[i].Head = converted.Templates[i].Head || a.Templates[ref].Head
			continue
		}
		source := a.Templates[ref].Source
		if planned.Ref != ref {
			if source, err = model.CanonicalYAML(planned.Tmpl); err != nil {
				return b, err
			}
		}
		seen[planned.Ref] = len(converted.Templates)
		converted.Templates = append(converted.Templates, pwf.Template{Head: a.Templates[ref].Head, Source: source})
	}
	if converted.Root == "" {
		return b, errors.New("the root template was not converted")
	}
	return converted, nil
}

// pwfStrictIssues lists everything strict=1 refuses.
func pwfStrictIssues(b pwf.Bundle) ([]string, error) {
	var issues []string
	paths, err := pwf.ScanPaths(b)
	if err != nil {
		return nil, err
	}
	for _, p := range paths {
		why := "may not exist elsewhere"
		if p.Kind == pwf.PathHome {
			why = "reveals a username"
		}
		issues = append(issues, fmt.Sprintf("%s: %s (%s)", p.Where, p.Path, why))
	}
	named, err := pwfNamedAgents(b)
	if err != nil {
		return nil, err
	}
	for _, use := range named {
		issues = append(issues, fmt.Sprintf("%s names agent %q (give it a role with --assignee-role)", strings.Join(use.Steps, ", "), use.Name))
	}
	problems, err := pwf.Problems(b)
	if err != nil {
		return nil, err
	}
	return append(issues, problems...), nil
}
