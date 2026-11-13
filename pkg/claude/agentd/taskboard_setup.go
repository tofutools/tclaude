package agentd

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/boardopts"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/groupexport"
)

// A board's setup travels with its group (docs/taskboard.md "Carrying a
// board's setup"): a group export, a clone and a group template carry its
// options and custom columns (with their states), never its tasks, unless
// the caller leaves them out ("board options" in the dashboard,
// --no-board-options in the CLI, board_options=false on the wire).

// queryFalse reports whether a query or form value turns something off.
func queryFalse(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "false", "0", "no", "off":
		return true
	}
	return false
}

// sanitizeTaskboardSetup checks a setup from outside (a file, a template)
// for a board that has the custom columns existing: each column a valid
// slug and label, new to the board and within the cap, each state one a
// board takes, and the options a document this tclaude reads, pruned to the
// columns the board will have. What fails is left out and said in warnings.
// It returns nil when nothing is left.
func sanitizeTaskboardSetup(tb *groupexport.Taskboard, existing []string) (*groupexport.Taskboard, []string) {
	if tb == nil {
		return nil, nil
	}
	var warnings []string
	out := &groupexport.Taskboard{}
	slugs := append([]string{}, existing...)
	var labels []string
	for _, c := range tb.Columns {
		label, err := validateTaskColumnLabel(c.Label)
		if err == nil {
			err = validateTaskColumnSlug(c.Slug)
		}
		switch {
		case err != nil:
			warnings = append(warnings, fmt.Sprintf("board column %q left out: %v", guideText(c.Slug, 40), err))
			continue
		case slices.Contains(slugs, c.Slug):
			if !slices.Contains(existing, c.Slug) {
				warnings = append(warnings, fmt.Sprintf("board column %s left out: named twice", c.Slug))
			}
			continue
		case containsFold(labels, label):
			warnings = append(warnings, fmt.Sprintf("board column %s left out: its label %q is taken", c.Slug, label))
			continue
		case len(slugs) >= db.GroupTaskColumnsCap:
			warnings = append(warnings, fmt.Sprintf("board column %s left out: a board holds at most %d", c.Slug, db.GroupTaskColumnsCap))
			continue
		}
		state := c.State
		if state != "" && state != db.GroupTaskColumnShown && state != db.GroupTaskColumnHidden && state != db.GroupTaskColumnCordoned {
			warnings = append(warnings, fmt.Sprintf("board column %s: state %q left out", c.Slug, guideText(state, 20)))
			state = ""
		}
		if state == db.GroupTaskColumnShown {
			state = ""
		}
		slugs, labels = append(slugs, c.Slug), append(labels, label)
		out.Columns = append(out.Columns, groupexport.TaskboardColumn{Slug: c.Slug, Label: label, State: state})
	}
	for slug, state := range tb.BuiltinStates {
		if (slug != db.GroupTaskColumnIdeas && slug != db.GroupTaskColumnDone) || state != db.GroupTaskColumnHidden {
			if state != "" && state != db.GroupTaskColumnShown {
				warnings = append(warnings, fmt.Sprintf("board: %s %q left out (only ideas and done are hidden)", guideText(slug, 20), guideText(state, 20)))
			}
			continue
		}
		if out.BuiltinStates == nil {
			out.BuiltinStates = map[string]string{}
		}
		out.BuiltinStates[slug] = state
	}
	if raw := strings.TrimSpace(string(tb.Options)); raw != "" && raw != "null" {
		o, err := boardopts.Parse(raw)
		if err == nil {
			o.Prune(slugs)
			err = o.Validate(slugs)
		}
		if err != nil {
			warnings = append(warnings, "board options left out: "+err.Error())
		} else if !o.IsZero() {
			enc, err := o.Encode()
			if err != nil {
				warnings = append(warnings, "board options left out: "+err.Error())
			} else {
				out.Options = json.RawMessage(enc)
			}
		}
	}
	if out.Options == nil && out.Columns == nil && out.BuiltinStates == nil {
		return nil, warnings
	}
	return out, warnings
}

// taskboardSetupView is a setup as a preview shows it: the columns, the
// hidden built-in columns, and the option keys it sets.
type taskboardSetupView struct {
	Columns       []groupexport.TaskboardColumn `json:"columns,omitempty"`
	BuiltinStates map[string]string             `json:"builtin_states,omitempty"`
	// Set lists the option keys the setup sets (boardopts.Options.SetKeys).
	Set []string `json:"set,omitempty"`
}

// taskboardSetupViewOf describes tb for a preview; nil for none.
func taskboardSetupViewOf(tb *groupexport.Taskboard) *taskboardSetupView {
	if tb == nil {
		return nil
	}
	v := &taskboardSetupView{Columns: tb.Columns, BuiltinStates: tb.BuiltinStates}
	if o, err := boardopts.Parse(string(tb.Options)); err == nil {
		slugs := make([]string, 0, len(tb.Columns))
		for _, c := range tb.Columns {
			slugs = append(slugs, c.Slug)
		}
		v.Set = o.SetKeys(slugs)
	}
	return v
}

// copyTaskboardSetup gives the board of group to the setup of from's board
// (a clone): the source is this node's own, checked as any other.
func copyTaskboardSetup(from, to *db.AgentGroup, actor string) ([]string, error) {
	tb, err := db.GroupTaskboardSetup(from.ID)
	if err != nil || tb == nil {
		return nil, err
	}
	return applyTaskboardSetup(to, tb, actor)
}

// applyTaskboardSetup checks tb against g's board and applies what passes.
func applyTaskboardSetup(g *db.AgentGroup, tb *groupexport.Taskboard, actor string) ([]string, error) {
	existing, err := taskboardColumnSlugs(g.ID)
	if err != nil {
		return nil, err
	}
	clean, warnings := sanitizeTaskboardSetup(tb, existing)
	if clean == nil {
		return warnings, nil
	}
	return warnings, db.ApplyGroupTaskboardSetup(g.ID, clean, actor)
}

// templateTaskboard reads a template's stored board setup; nil for none or
// one this tclaude cannot read.
func templateTaskboard(raw string) *groupexport.Taskboard {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var tb groupexport.Taskboard
	if err := json.Unmarshal([]byte(raw), &tb); err != nil {
		return nil
	}
	return &tb
}

// tracedSetupJudged is after as a peer's write of it is judged when its board
// setup was traced from a board the peer reads or kept from the template
// (before; nil for a new one): the setup is then not the peer's writing.
func tracedSetupJudged(after templateJSON, before *templateJSON) templateJSON {
	after.Taskboard = nil
	if before != nil {
		after.Taskboard = before.Taskboard
	}
	return after
}
