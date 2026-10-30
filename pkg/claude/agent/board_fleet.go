package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
	"unicode"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
)

// `tclaude agent board from-fleet` draws the fleet on a board: a column per
// group, a card per agent coloured by what it is doing, so the operator and
// the Steward can talk staffing over a picture (circle an agent, draw an
// arrow to where it should go).

type boardFromFleetParams struct {
	Group string `long:"group" optional:"true" help:"Only this group (name or ID)"`
	All   bool   `long:"all" optional:"true" help:"Include offline agents"`
}

func boardFromFleetCmd() *cobra.Command {
	return boa.CmdT[boardFromFleetParams]{
		Use:   "from-fleet <board>",
		Short: "Draw the fleet on a board: a column per group, a card per agent",
		Long: "Draws every live agent (offline ones too with --all) as a card under its group, coloured by what it is " +
			"doing: green working, grey idle, yellow waiting on the operator, red failed, white offline. Each card " +
			"names the agent, its status and its task or PR. The board is created when it does not exist. A board holds " +
			"one fleet map (elements fg-/fa-): drawing again refreshes it and keeps everything else on the board. " +
			"Needs boards.write and fleet.read.",
		Args:        cobra.ExactArgs(1),
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *boardFromFleetParams, _ *cobra.Command, args []string) {
			os.Exit(runBoardFromFleet(p, args[0], os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runBoardFromFleet(p *boardFromFleetParams, boardID string, stdout, stderr io.Writer) int {
	path := "/v1/fleet"
	if g := strings.TrimSpace(p.Group); g != "" {
		path += "?group=" + url.QueryEscape(g)
	}
	var fleet fleetResp
	if err := DaemonGet(path, &fleet); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	doc, err := fetchBoard(boardID, -1)
	if err != nil {
		var de *DaemonError
		if !errors.As(err, &de) || de.Status != http.StatusNotFound {
			return boardErr(stderr, err)
		}
		if err := DaemonRequest(http.MethodPost, "/v1/boards", map[string]string{"id": boardID, "title": "Fleet"}, &boardDoc{}, DaemonOpts{}); err != nil {
			return boardErr(stderr, err)
		}
		doc = &boardDoc{ID: boardID}
	}
	elems, cards := fleetBoardElements(&fleet, doc, p.All)
	out := make([]json.RawMessage, 0, len(elems))
	for _, e := range elems {
		raw, err := json.Marshal(e)
		if err != nil {
			fmt.Fprintf(stderr, "Error: %v\n", err)
			return rcIOFailure
		}
		out = append(out, raw)
	}
	resp, err := patchBoard(boardID, out)
	if err != nil {
		return boardErr(stderr, err)
	}
	fmt.Fprintf(stdout, "Drew the fleet on board %s: %d agent card(s).\n", boardID, cards)
	reportPatch(stdout, boardID, resp, len(out))
	return rcOK
}

func isFleetSketchID(id string) bool {
	return strings.HasPrefix(id, "fg-") || strings.HasPrefix(id, "fa-")
}

const (
	fleetCardW   = 230.0
	fleetCardH   = 86.0
	fleetColGap  = 50.0
	fleetRowGap  = 18.0
	fleetHeadGap = 46.0
)

// fleetCardColor fills an agent's card by what it is doing.
func fleetCardColor(online bool, status string) string {
	if !online {
		return "#ffffff"
	}
	switch status {
	case "working", "main_agent_idle":
		return "#b2f2bb"
	case "awaiting_permission", "awaiting_input":
		return "#ffec99"
	case "error":
		return "#ffc9c9"
	}
	return "#e9ecef"
}

func fleetStatusWords(online bool, status string) string {
	if !online {
		if status == "away" {
			return "away"
		}
		return "offline"
	}
	switch status {
	case "working", "main_agent_idle":
		return "working"
	case "awaiting_permission":
		return "needs you: permission"
	case "awaiting_input":
		return "needs you: question"
	case "error":
		return "failed"
	case "":
		return "online"
	}
	return status
}

// fleetBoardElements builds the map: a title, then a column per group (in
// the brief's order) with a dashed box around its cards. It replaces the
// board's previous fleet map like from-process replaces its sketch, and goes
// below whatever else the board holds.
func fleetBoardElements(fleet *fleetResp, doc *boardDoc, all bool) ([]map[string]any, int) {
	now := time.Now().UnixMilli()
	existing := loadBoardScene(doc)
	stored := map[string]int64{}
	for _, raw := range doc.Elements {
		var v boardElementView
		if json.Unmarshal(raw, &v) == nil && v.ID != "" {
			stored[v.ID] = v.Version
		}
	}
	base := func(id, typ string) map[string]any {
		return map[string]any{
			"id": id, "type": typ, "version": stored[id] + 1, "versionNonce": boardRandomNonce(), "isDeleted": false,
			"updated": now, "strokeColor": "#1e1e1e", "backgroundColor": "transparent", "fillStyle": "solid",
			"strokeWidth": 2, "strokeStyle": "solid", "roughness": 1, "opacity": 100, "angle": 0,
		}
	}
	text := func(id, s string, x, y, size float64) map[string]any {
		s = boardPrintable(s)
		tw, th := boardTextSize(s)
		scale := size / boardFontSize
		e := base(id, "text")
		e["text"], e["fontSize"], e["textAlign"] = s, size, "left"
		e["x"], e["y"], e["width"], e["height"] = x, y, tw*scale, th*scale
		return e
	}

	// A redraw stays where the previous map is, so the operator's circles
	// and arrows on it still point at the same cards; a first map goes below
	// what the board already has.
	top := 0.0
	if prev, ok := existing.view["fg-title"]; ok && !prev.IsDeleted {
		top = prev.Y
	} else {
		for _, id := range existing.order {
			if isFleetSketchID(id) {
				continue
			}
			box := elementBox(existing.view[id])
			if bottom := box.Y + box.H + 2*boardDefaultGap; bottom > top {
				top = bottom
			}
		}
	}

	var out []map[string]any
	c := fleet.Counts
	title := fmt.Sprintf("Fleet: %d online (%d working, %d idle, %d need you, %d failed), %d offline — %s",
		c.Online, c.Working, c.Idle, c.NeedsYou, c.Errored, c.Offline, fleet.GeneratedAt.Local().Format("Jan 2 15:04"))
	out = append(out, text("fg-title", title, 0, top, 24))

	ids := newProcessIDs()
	ids.taken["fg-title"], ids.taken["fg-title-label"] = true, true
	col := 0
	cards := 0
	var groupOrder []string
	members := map[string][]int{}
	for i, a := range fleet.Agents {
		if !a.Online && !all {
			continue
		}
		// An agent in several groups has a card in each column.
		groups := a.Groups
		if len(groups) == 0 {
			groups = []string{"(no group)"}
		}
		for _, g := range groups {
			if _, ok := members[g]; !ok {
				groupOrder = append(groupOrder, g)
			}
			members[g] = append(members[g], i)
		}
	}
	for _, g := range groupOrder {
		x := float64(col) * (fleetCardW + fleetColGap)
		y := top + 60
		gid := ids.get("fg-", g)
		out = append(out, text(gid+"-label", boardClip(g, 18), x, y, 22))
		cy := y + fleetHeadGap
		for _, i := range members[g] {
			a := fleet.Agents[i]
			aid := ids.get("fa-", a.AgentID, g)
			card := base(aid, "rectangle")
			card["x"], card["y"], card["width"], card["height"] = x, cy, fleetCardW, fleetCardH
			card["backgroundColor"] = fleetCardColor(a.Online, a.Status)
			if !a.Online {
				card["strokeStyle"] = "dashed"
			}
			out = append(out, card)
			lines := []string{boardClip(a.Title, 20), fleetStatusWords(a.Online, a.Status)}
			switch {
			case len(a.PRs) > 0:
				lines = append(lines, "PR "+boardClip(fleetPRShort(a.PRs[0].URL), 18))
			case a.TaskLabel != "":
				lines = append(lines, boardClip(a.TaskLabel, 20))
			case a.Task != "":
				lines = append(lines, boardClip(a.Task, 20))
			}
			label := text(aid+"-label", strings.Join(lines, "\n"), x+12, cy+8, 16)
			out = append(out, label)
			cy += fleetCardH + fleetRowGap
			cards++
		}
		box := base(gid, "rectangle")
		box["strokeStyle"], box["strokeColor"], box["strokeWidth"] = "dashed", "#868e96", 1
		box["x"], box["y"], box["width"], box["height"] = x-12, y+fleetHeadGap-12, fleetCardW+24, cy-(y+fleetHeadGap)+12-fleetRowGap+12
		// The box goes under its cards.
		out = append([]map[string]any{box}, out...)
		col++
	}

	// The previous map's elements this one does not have.
	drawn := map[string]bool{}
	for _, e := range out {
		drawn[e["id"].(string)] = true
	}
	for _, id := range existing.order {
		if drawn[id] || !isFleetSketchID(id) {
			continue
		}
		out = append(out, map[string]any{
			"id": id, "type": existing.view[id].Type, "version": stored[id] + 1, "versionNonce": boardRandomNonce(),
			"isDeleted": true, "updated": now,
		})
	}
	return out, cards
}

// fleetPRShort turns a GitHub PR URL into repo#n.
func fleetPRShort(u string) string {
	parts := strings.Split(strings.TrimSuffix(u, "/"), "/")
	if n := len(parts); n >= 4 && parts[n-2] == "pull" {
		return parts[n-3] + "#" + parts[n-1]
	}
	return u
}

// boardPrintable drops what the board server refuses (control and format
// characters such as a zero-width joiner), keeping line breaks.
func boardPrintable(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || unicode.IsPrint(r) {
			return r
		}
		return -1
	}, s)
}

func boardClip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}
