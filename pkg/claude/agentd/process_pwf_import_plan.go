package agentd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
	"github.com/tofutools/tclaude/pkg/pwf"
)

// The importer's choices for a .pwf import (tcl-sb56fp): map the roles the
// workflow asks for onto local ones, import a template whose id is taken
// here under a new id instead of replacing it, and pin the imported root to
// a group. They ride on the preview and the apply alike
// (?map=<role>=<local>&rename=1&group=<name>), so the review shows their
// effect and the reviewDigest covers them: an apply stores exactly what was
// reviewed.
//
// Mapping and renaming rewrite template content, so each rewritten template
// gets a new ref. Every template in the imported set is rewritten
// consistently: a renamed id is renamed in every reference to it, and a
// reference pinned to a rewritten version is re-pinned to that version's new
// hash.

type pwfImportOptions struct {
	// RoleMap maps a role name the workflow uses to the local name to use
	// instead. Names mapped to themselves are left out.
	RoleMap map[string]string
	Rename  bool
	Group   string
	// Workflow is the key of the workflow chosen from a .paf. It is resolved
	// after parsing, so a review of the default workflow binds to its key.
	Workflow string
}

func parsePWFImportOptions(q url.Values) (pwfImportOptions, error) {
	opts := pwfImportOptions{RoleMap: map[string]string{}, Rename: q.Get("rename") == "1", Group: strings.TrimSpace(q.Get("group"))}
	for _, m := range q["map"] {
		from, to, ok := strings.Cut(m, "=")
		from, to = strings.TrimSpace(from), strings.TrimSpace(to)
		if !ok || from == "" || to == "" {
			return opts, fmt.Errorf("role mapping %q is not <workflow role>=<local role>", m)
		}
		if !pwfLocalNameOK(to) {
			return opts, fmt.Errorf("role mapping %q: %q is not a usable role name", m, to)
		}
		if prev, dup := opts.RoleMap[from]; dup && prev != to {
			return opts, fmt.Errorf("role %q is mapped twice", from)
		}
		if from != to {
			opts.RoleMap[from] = to
		}
	}
	return opts, nil
}

func pwfLocalNameOK(name string) bool {
	if len(name) > 128 {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// digestKey is the options' canonical form for the reviewDigest.
func (o pwfImportOptions) digestKey() string {
	var b strings.Builder
	for _, from := range sortedKeysOf(o.RoleMap) {
		fmt.Fprintf(&b, "map %q=%q\n", from, o.RoleMap[from])
	}
	fmt.Fprintf(&b, "rename %t\ngroup %q\nworkflow %q\n", o.Rename, o.Group, o.Workflow)
	return b.String()
}

func sortedKeysOf[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// pwfRoleMapping is one role name the workflow uses and what its steps use
// after the import.
type pwfRoleMapping struct {
	Name  string   `json:"name"`
	Steps []string `json:"steps"`
	// Target is the name the imported steps use: the mapped local name, or
	// the workflow's own when unmapped.
	Target string `json:"target"`
	Mapped bool   `json:"mapped"`
	// Known reports whether Target names a role a member holds in a group
	// the caller can see, or a spawn profile here. An unknown target is
	// allowed, but those steps wait until the role or profile exists.
	Known bool `json:"known"`
}

// pwfRoleUses lists every role name the workflow's agent steps use: the
// role they ask for (profile, or the role selector) and the profile their
// fallback spawns, including a template's process-wide fallback.
func pwfRoleUses(a *pwf.Archive) map[string][]string {
	uses := map[string][]string{}
	add := func(name, where string) {
		// A {{ params.x }} role is chosen per run, not mapped on import.
		if name != "" && !strings.Contains(name, "{{") {
			uses[name] = append(uses[name], where)
		}
	}
	for _, ref := range a.Order {
		t := a.Templates[ref].Parsed
		if t.Fallback != nil {
			add(t.Fallback.Profile, t.ID+":fallback")
		}
		model.WalkNodes(t, func(id string, node model.Node) {
			forEachPerformer(&node, func(stage string, p *model.Performer) {
				if p.Kind != model.PerformerAgent {
					return
				}
				where := t.ID + ":" + id + stage
				add(p.Profile, where)
				add(p.Role, where)
				if p.Fallback != nil {
					add(p.Fallback.Profile, where+"/fallback")
				}
			})
		})
	}
	for name, steps := range uses {
		uses[name] = dedupeSorted(steps)
	}
	return uses
}

func dedupeSorted(in []string) []string {
	sort.Strings(in)
	out := in[:0]
	for i, s := range in {
		if i == 0 || s != in[i-1] {
			out = append(out, s)
		}
	}
	return out
}

// forEachPerformer visits a node's performer slots by stage suffix, the way
// pwf.Summarize names them.
func forEachPerformer(node *model.Node, visit func(stage string, p *model.Performer)) {
	if node.Performer != nil {
		visit("", node.Performer)
	}
	if node.Plan != nil {
		visit("/plan", &node.Plan.Performer)
	}
	for i := range node.Checks {
		visit(fmt.Sprintf("/check%d", i+1), &node.Checks[i].Performer)
	}
	if node.Review != nil {
		visit("/review", &node.Review.Performer)
	}
}

// pwfLocalRoleNames are the names a role can map to here: roles members hold
// in groups the caller can see, and spawn profiles (names and aliases).
func pwfLocalRoleNames(r *http.Request) ([]string, error) {
	names := map[string]bool{}
	if _, err := os.Stat(db.DBPath()); err != nil {
		return []string{}, nil
	}
	profiles, err := db.ListSpawnProfiles()
	if err != nil {
		return nil, err
	}
	for _, p := range profiles {
		names[p.Name] = true
		for _, alias := range p.Aliases {
			names[alias] = true
		}
	}
	all, groups := pwfRoleVisibility(r)
	roles, err := db.ListActiveGroupMemberRoles()
	if err != nil {
		return nil, err
	}
	for _, role := range roles {
		if all || groups[role.GroupID] {
			names[role.Role] = true
		}
	}
	out := make([]string, 0, len(names))
	for name := range names {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// pwfRoleVisibility is processContextVisibility without its error writes: an
// import needs process.templates.manage, not process.runs.read, so a caller
// that cannot see groups simply gets no group roles to map to.
func pwfRoleVisibility(r *http.Request) (bool, map[int64]bool) {
	p := peerFromContext(r.Context())
	switch classify(p) {
	case classHuman:
		return true, nil
	case classAgent:
	default:
		return false, nil
	}
	if allowed, _, err := permissionAllowsAction(r, p.ConvID, PermProcessRunsRead, ActionContext{}); err == nil && allowed {
		return true, nil
	}
	groups, err := db.ListGroupsForAgent(peerAgentID(p.ConvID))
	if err != nil {
		return false, nil
	}
	visible := make(map[int64]bool, len(groups))
	for _, g := range groups {
		visible[g.ID] = true
	}
	return false, visible
}

// pwfPlanned is one file template as it would be stored here.
type pwfPlanned struct {
	Tmpl *model.Template
	Ref  string
	ID   string
}

// pwfPlanner rewrites the file's templates for the chosen role mapping and
// renames. Plans are memoized per file ref.
type pwfPlanner struct {
	a       *pwf.Archive
	roleMap map[string]string
	// assignees rewrites the steps naming an agent; an export sets it
	// (pwfConvertAssignees).
	assignees map[string]pwfAssigneeRole
	renames   map[string]string
	done      map[string]*pwfPlanned
	active    map[string]bool
}

func (p *pwfPlanner) plan(ref string) (*pwfPlanned, error) {
	if got := p.done[ref]; got != nil {
		return got, nil
	}
	if p.active[ref] {
		return nil, fmt.Errorf("template %s references itself", ref)
	}
	src := p.a.Templates[ref]
	if src == nil {
		return nil, fmt.Errorf("template %s is not in the file", ref)
	}
	p.active[ref] = true
	defer delete(p.active, ref)
	// A fresh parse: the rewrite must not touch the archive's own copy.
	tmpl, _, err := pwf.ParseTemplate(src.Source)
	if err != nil {
		return nil, err
	}
	changed := false
	if id, ok := p.renames[tmpl.ID]; ok {
		tmpl.ID, changed = id, true
	}
	if tmpl.Fallback != nil {
		changed = p.mapName(&tmpl.Fallback.Profile) || changed
	}
	if err := p.rewriteNodes(tmpl.Nodes, &changed); err != nil {
		return nil, err
	}
	planned := &pwfPlanned{Tmpl: tmpl, Ref: ref, ID: tmpl.ID}
	if changed {
		source, err := model.CanonicalYAML(tmpl)
		if err != nil {
			return nil, err
		}
		// Re-parse the rewritten source: it is validated like any import,
		// and its ref is the one the store will record.
		parsed, newRef, err := pwf.ParseTemplate(source)
		if err != nil {
			return nil, fmt.Errorf("rewriting %s: %w", ref, err)
		}
		planned = &pwfPlanned{Tmpl: parsed, Ref: newRef, ID: parsed.ID}
	}
	p.done[ref] = planned
	return planned, nil
}

func (p *pwfPlanner) mapName(name *string) bool {
	if to, ok := p.roleMap[*name]; ok && *name != "" && !strings.Contains(*name, "{{") {
		*name = to
		return true
	}
	return false
}

func (p *pwfPlanner) rewriteNodes(nodes map[string]model.Node, changed *bool) error {
	for id, node := range nodes {
		forEachPerformer(&node, func(_ string, perf *model.Performer) {
			if perf.Kind != model.PerformerAgent {
				return
			}
			if to, ok := p.assignees[perf.Assignee]; ok {
				// A profile already says who can take the step, and a role
				// would narrow it, so only a role asked for by name wins.
				if !to.Held || perf.Profile == "" {
					perf.Role = to.Role
				}
				perf.Assignee, *changed = "", true
			}
			*changed = p.mapName(&perf.Profile) || *changed
			*changed = p.mapName(&perf.Role) || *changed
			if perf.Fallback != nil {
				*changed = p.mapName(&perf.Fallback.Profile) || *changed
			}
		})
		if sp := node.Subprocess; sp != nil {
			switch {
			case sp.TemplateRef != "" && sp.Version != "" && p.a.Templates[sp.TemplateRef+"@"+sp.Version] == nil:
				// A pin the file does not carry stays as written; the review
				// lists it among the workflow's problems.
			case sp.TemplateRef != "" && sp.Version != "":
				child, err := p.plan(sp.TemplateRef + "@" + sp.Version)
				if err != nil {
					return err
				}
				id, version, _ := strings.Cut(child.Ref, "@")
				if id != sp.TemplateRef || version != sp.Version {
					sp.TemplateRef, sp.Version, *changed = id, version, true
				}
			case sp.TemplateRef != "":
				if renamed, ok := p.renames[sp.TemplateRef]; ok {
					sp.TemplateRef, *changed = renamed, true
				}
			default:
				if err := p.rewriteNodes(sp.Nodes, changed); err != nil {
					return err
				}
			}
		}
		nodes[id] = node
	}
	return nil
}

// pwfImportPlan plans every file template and decides renames: with rename,
// an id whose planned head differs from this node's head is imported under
// the first free <id>-<n>. Renaming one template rewrites the templates that
// reference it, which can make them differ too, so it repeats until nothing
// new collides.
func pwfImportPlan(ctx context.Context, fs *store.FS, a *pwf.Archive, opts pwfImportOptions) (map[string]*pwfPlanned, map[string]string, error) {
	renames := map[string]string{}
	fileIDs := map[string]bool{}
	for _, ref := range a.Order {
		fileIDs[a.Templates[ref].Parsed.ID] = true
	}
	taken := func(id string) (bool, error) {
		if fileIDs[id] {
			return true, nil
		}
		for _, to := range renames {
			if to == id {
				return true, nil
			}
		}
		_, err := fs.GetTemplateHead(ctx, id)
		if err == nil {
			return true, nil
		}
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	for range len(a.Order) + 1 {
		p := &pwfPlanner{a: a, roleMap: opts.RoleMap, renames: renames, done: map[string]*pwfPlanned{}, active: map[string]bool{}}
		for _, ref := range a.Order {
			if _, err := p.plan(ref); err != nil {
				return nil, nil, err
			}
		}
		if !opts.Rename {
			return p.done, renames, nil
		}
		var collide []string
		for _, ref := range a.Order {
			planned := p.done[ref]
			if !a.Templates[ref].Head || renames[a.Templates[ref].Parsed.ID] != "" {
				continue
			}
			head, err := fs.GetTemplateHead(ctx, planned.ID)
			if errors.Is(err, store.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return nil, nil, err
			}
			if head.Ref != planned.Ref {
				collide = append(collide, a.Templates[ref].Parsed.ID)
			}
		}
		if len(collide) == 0 {
			return p.done, renames, nil
		}
		sort.Strings(collide)
		for _, id := range collide {
			for n := 2; ; n++ {
				candidate := fmt.Sprintf("%s-%d", id, n)
				if n > 999 || !pwfTemplateIDRE.MatchString(candidate) {
					return nil, nil, fmt.Errorf("no free id to import %q under", id)
				}
				busy, err := taken(candidate)
				if err != nil {
					return nil, nil, err
				}
				if !busy {
					renames[id] = candidate
					break
				}
			}
		}
	}
	return nil, nil, errors.New("renaming did not settle")
}

// pwfTemplateIDRE mirrors pwf's portable template id rule.
var pwfTemplateIDRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)
