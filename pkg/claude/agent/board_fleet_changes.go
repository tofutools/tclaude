package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"sort"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
)

// `tclaude agent board fleet-changes` reads what the operator changed on a
// fleet map as staffing proposals: a card dragged to another column, an arrow
// from a card to another column, a deleted card, a note in a column. It only
// reads; acting on a proposal is the Steward's plan for the operator to
// approve.

const boardOperatorAuthor = "operator"

type boardFleetChangesParams struct {
	JSON bool `long:"json" help:"Output JSON"`
}

// fleetChange is one staffing proposal.
type fleetChange struct {
	Kind    string `json:"kind"` // move, remove or add
	AgentID string `json:"agent_id,omitempty"`
	Title   string `json:"title,omitempty"`
	From    string `json:"from,omitempty"` // empty for an agent in no group
	To      string `json:"to,omitempty"`
	Text    string `json:"text,omitempty"`
	Via     string `json:"via"` // card, arrow, deleted card or note
}

func boardFleetChangesCmd() *cobra.Command {
	return boa.CmdT[boardFleetChangesParams]{
		Use:   "fleet-changes <board>",
		Short: "Read the operator's edits on a fleet map as staffing proposals",
		Long: "Reads a board drawn with from-fleet and lists what the operator changed as proposals: a card dragged to " +
			"another column or an arrow from a card to another column (move), a deleted card (remove), and a note " +
			"written in a column (add). It changes nothing: confirm with the operator, act, then draw the map again. " +
			"Needs boards.read and fleet.read.",
		Args:        cobra.ExactArgs(1),
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *boardFleetChangesParams, _ *cobra.Command, args []string) {
			os.Exit(runBoardFleetChanges(p, args[0], os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runBoardFleetChanges(p *boardFleetChangesParams, boardID string, stdout, stderr io.Writer) int {
	var fleet fleetResp
	if err := DaemonGet("/v1/fleet", &fleet); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	doc, err := fetchBoard(boardID, boardWithTombstones)
	if err != nil {
		return boardErr(stderr, err)
	}
	changes, ok := fleetChanges(&fleet, doc)
	if !ok {
		fmt.Fprintf(stderr, "Error: board %s holds no fleet map; draw one with `tclaude agent board from-fleet %s`\n", boardID, boardID)
		return rcInvalidArg
	}
	if p.JSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(map[string]any{"board": boardID, "changes": changes})
		return rcOK
	}
	if len(changes) == 0 {
		fmt.Fprintf(stdout, "The operator has drawn no staffing changes on board %s.\n", boardID)
		return rcOK
	}
	fmt.Fprintf(stdout, "Staffing changes drawn on board %s (proposals; nothing was changed):\n", boardID)
	for _, c := range changes {
		switch c.Kind {
		case "move":
			if c.From == "" {
				fmt.Fprintf(stdout, "  - add %s [%s] to %s (%s)\n", c.Title, shortAgentID(c.AgentID, ""), c.To, c.Via)
				continue
			}
			fmt.Fprintf(stdout, "  - move %s [%s] from %s to %s (%s)\n", c.Title, shortAgentID(c.AgentID, ""), c.From, c.To, c.Via)
		case "remove":
			from := c.From
			if from == "" {
				from = fleetNoGroup
			}
			fmt.Fprintf(stdout, "  - remove %s [%s] from %s (%s)\n", c.Title, shortAgentID(c.AgentID, ""), from, c.Via)
		case "add":
			fmt.Fprintf(stdout, "  - add to %s: %q (note)\n", c.To, c.Text)
		}
	}
	return rcOK
}

// fleetChanges compares a fleet map with the fleet it was drawn from. Only
// the operator's own edits count: a redraw, written by an agent, replaces
// the cards it moves or deletes. ok is false when the board has no map.
func fleetChanges(fleet *fleetResp, doc *boardDoc) (changes []fleetChange, ok bool) {
	elements := map[string]boardElementView{}
	var order []string
	for _, raw := range doc.Elements {
		var v boardElementView
		if json.Unmarshal(raw, &v) == nil && v.ID != "" {
			if _, seen := elements[v.ID]; !seen {
				order = append(order, v.ID)
			}
			elements[v.ID] = v
		}
	}
	if title, found := elements["fg-title"]; !found || title.IsDeleted {
		return nil, false
	}

	type cardRef struct {
		agent int
		group string
	}
	cards := map[string]cardRef{}
	boxes := map[string]string{} // group box id → group
	var bands []fleetBand
	for _, column := range fleetColumns(fleet, true) {
		// Cards count even when their column's box is gone: selecting a
		// whole column and deleting it removes each card in it.
		for _, c := range column.cards {
			cards[c.id] = cardRef{agent: c.agent, group: column.group}
		}
		box, drawn := elements[column.id]
		if !drawn || box.IsDeleted {
			continue
		}
		boxes[column.id] = column.group
		bands = append(bands, fleetBand{group: column.group, left: box.X, right: box.X + box.Width, top: box.Y - fleetHeadGap})
	}
	cardOf := func(id string) (cardRef, string, bool) {
		id = strings.TrimSuffix(id, "-label")
		ref, found := cards[id]
		return ref, id, found
	}
	isOperator := func(e boardElementView) bool {
		return e.CustomData != nil && e.CustomData.Author == boardOperatorAuthor
	}

	seen := map[string]bool{}
	add := func(c fleetChange) {
		key := strings.Join([]string{c.Kind, c.AgentID, c.From, c.To, c.Text}, "\x00")
		if !seen[key] {
			seen[key] = true
			changes = append(changes, c)
		}
	}
	remove := func(ref cardRef, via string) {
		a := fleet.Agents[ref.agent]
		add(fleetChange{Kind: "remove", AgentID: a.AgentID, Title: a.Title, From: fleetRealGroup(ref.group), Via: via})
	}
	move := func(ref cardRef, to, via string) {
		a := fleet.Agents[ref.agent]
		// Dropped where the agent already is, or out of every group, the
		// edit says leave this group.
		if to == fleetNoGroup || slices.Contains(a.Groups, to) {
			remove(ref, via)
			return
		}
		add(fleetChange{Kind: "move", AgentID: a.AgentID, Title: a.Title, From: fleetRealGroup(ref.group), To: to, Via: via})
	}

	for _, id := range order {
		e := elements[id]
		if !isOperator(e) {
			continue
		}
		ref, cardID, isCard := cardOf(id)
		switch {
		case isCard && e.IsDeleted:
			// A card is its rectangle; its label alone going says nothing.
			if id == cardID {
				remove(ref, "deleted card")
			}
		case e.IsDeleted:
		case isCard:
			box := elementBox(e)
			if to := fleetBandAt(bands, box.X+box.W/2, box.Y+box.H/2); to != "" && to != ref.group {
				move(ref, to, "card")
			}
		case e.Type == "arrow" && len(e.Points) >= 2:
			first, last := e.Points[0], e.Points[len(e.Points)-1]
			if len(first) < 2 || len(last) < 2 {
				continue
			}
			var from cardRef
			found := false
			if e.StartBinding != nil {
				// Bound to something else, or to a card a redraw replaced:
				// whatever sits under the arrow's tail now is not what the
				// operator pointed at.
				from, _, found = cardOf(e.StartBinding.ElementID)
			} else {
				sx, sy := e.X+first[0], e.Y+first[1]
				for _, cid := range order {
					c, isC := cards[cid]
					box := elementBox(elements[cid])
					if isC && !elements[cid].IsDeleted && sx >= box.X && sx <= box.X+box.W && sy >= box.Y && sy <= box.Y+box.H {
						from, found = c, true
						break
					}
				}
			}
			if !found {
				continue
			}
			to := ""
			if e.EndBinding != nil {
				if g, isBox := boxes[strings.TrimSuffix(e.EndBinding.ElementID, "-label")]; isBox {
					to = g
				} else if c, _, isC := cardOf(e.EndBinding.ElementID); isC {
					to = c.group
				}
			}
			if to == "" {
				to = fleetBandAt(bands, e.X+last[0], e.Y+last[1])
			}
			if to != "" && to != from.group {
				move(from, to, "arrow")
			}
		case e.Type == "text" && !isFleetSketchID(id) && strings.TrimSpace(e.Text) != "":
			box := elementBox(e)
			if to := fleetBandAt(bands, box.X+box.W/2, box.Y+box.H/2); to != "" && to != fleetNoGroup {
				add(fleetChange{Kind: "add", To: to, Text: strings.TrimSpace(e.Text), Via: "note"})
			}
		}
	}
	rank := map[string]int{"move": 0, "remove": 1, "add": 2}
	sort.SliceStable(changes, func(i, j int) bool { return rank[changes[i].Kind] < rank[changes[j].Kind] })
	return changes, true
}

// fleetRealGroup is the group to name in a change: none for the map's column
// of agents outside every group, which no command can address.
func fleetRealGroup(group string) string {
	if group == fleetNoGroup {
		return ""
	}
	return group
}

// fleetBand is a column's horizontal extent; a column reads as everything
// under its heading, however far the cards have grown past the dashed box.
type fleetBand struct {
	group            string
	left, right, top float64
}

func fleetBandAt(bands []fleetBand, x, y float64) string {
	for _, b := range bands {
		if x >= b.left && x <= b.right && y >= b.top {
			return b.group
		}
	}
	return ""
}
