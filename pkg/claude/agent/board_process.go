package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/common"
)

// Sketching a process on a board (tcl-4yhow7): `board from-process` draws a
// process template as a flowchart, so the operator and agents can talk a
// process through on a board. Steps are shapes (performer by colour, a
// decision a diamond, start and end ellipses), each outcome is an arrow, and
// the flow is laid out like `board layout`. Element ids are `p-<node>`, so
// running it again redraws the same steps in place of the old ones.

type boardFromProcessParams struct {
	Direction string   `long:"direction" optional:"true" help:"Flow direction: right (default) or down"`
	Gap       *float64 `long:"gap" help:"Space between steps (default 60)"`
}

func boardFromProcessCmd() *cobra.Command {
	return boa.CmdT[boardFromProcessParams]{
		Use:   "from-process <board> <process-template>",
		Short: "Draw a process template on a board as a flowchart",
		Long: "Draws the process template's steps as shapes (human steps yellow, agent steps blue, program steps green; " +
			"decisions are diamonds, start and end ellipses) with an arrow per outcome, laid out along the flow. " +
			"The board is created when it does not exist. Elements are named p-<step>, so drawing the same template " +
			"again replaces its steps. Needs boards.write and process.templates.read.",
		Args:        cobra.ExactArgs(2),
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *boardFromProcessParams, _ *cobra.Command, args []string) {
			os.Exit(runBoardFromProcess(p, args[0], args[1], os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runBoardFromProcess(p *boardFromProcessParams, boardID, templateID string, stdout, stderr io.Writer) int {
	dir := strings.ToLower(strings.TrimSpace(p.Direction))
	if dir == "" {
		dir = "right"
	}
	if dir != "right" && dir != "down" {
		fmt.Fprintln(stderr, "Error: --direction must be right or down")
		return rcInvalidArg
	}
	gap := floatOr(p.Gap, boardDefaultGap)
	if gap < 0 {
		fmt.Fprintln(stderr, "Error: --gap must not be negative")
		return rcInvalidArg
	}
	var shown processTemplateShowJSON
	if err := DaemonRequest(http.MethodGet, "/v1/process/templates/"+url.PathEscape(templateID), nil, &shown, DaemonOpts{}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	parsed, err := model.ParseAuthoring([]byte(shown.Source))
	if err != nil || parsed.Template == nil {
		fmt.Fprintf(stderr, "Error: process template %s could not be read: %v\n", templateID, err)
		return rcIOFailure
	}
	doc, err := fetchBoard(boardID, -1)
	if err != nil {
		var de *DaemonError
		if !errors.As(err, &de) || de.Status != http.StatusNotFound {
			return boardErr(stderr, err)
		}
		title := parsed.Template.Name
		if title == "" {
			title = parsed.Template.ID
		}
		if err := DaemonRequest(http.MethodPost, "/v1/boards", map[string]string{"id": boardID, "title": title}, &boardDoc{}, DaemonOpts{}); err != nil {
			return boardErr(stderr, err)
		}
		doc = &boardDoc{ID: boardID}
	}
	elems := processBoardElements(parsed.Template, parsed.Edges, loadBoardScene(doc), dir, gap)
	out := make([]json.RawMessage, 0, len(elems))
	arrowCount := 0
	for _, e := range elems {
		if e["type"] == "arrow" {
			arrowCount++
		}
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
	fmt.Fprintf(stdout, "Drew process %s on board %s: %d step(s), %d outcome arrow(s).\n",
		parsed.Template.ID, boardID, len(parsed.Template.Nodes), arrowCount)
	reportPatch(stdout, boardID, resp, len(out))
	return rcOK
}

var boardIDUnsafe = regexp.MustCompile(`[^A-Za-z0-9_-]`)

// processElementID names a step's element: p-<node>, or a hash when the
// node id does not fit an element id.
func processElementID(parts ...string) string {
	id := "p-" + boardIDUnsafe.ReplaceAllString(strings.Join(parts, "-"), "_")
	if len(id) > 58 { // room for "-label"
		sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
		id = "p-" + hex.EncodeToString(sum[:12])
	}
	return id
}

// processBoardElements builds the flowchart: shapes and labels for the
// steps, arrows (labelled when a step has several outcomes) for the edges.
// Existing p-* elements are replaced at a higher version; the drawing goes
// below anything else on the board.
func processBoardElements(t *model.Template, edges []model.Edge, existing *boardScene, dir string, gap float64) []map[string]any {
	now := time.Now().UnixMilli()
	version := func(id string) int64 {
		if m, ok := existing.raw[id]; ok {
			v, _ := m["version"].(float64)
			return int64(v) + 1
		}
		return 1
	}
	base := func(id, typ string) map[string]any {
		return map[string]any{
			"id": id, "type": typ, "version": version(id), "versionNonce": boardRandomNonce(), "isDeleted": false,
			"updated": now, "strokeColor": "#1e1e1e", "backgroundColor": "transparent", "fillStyle": "solid",
			"strokeWidth": 2, "strokeStyle": "solid", "roughness": 1, "opacity": 100, "angle": 0,
		}
	}
	// Steps in flow order (breadth-first from the start, outcomes by name,
	// unreachable steps last): layout keeps this order within a layer and
	// breaks a loop (rework going back) where the flow first meets it.
	next := map[string][]model.Edge{}
	for _, edge := range edges {
		next[edge.From] = append(next[edge.From], edge)
	}
	var names []string
	seen := map[string]bool{}
	queue := []string{t.Start}
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		if _, ok := t.Nodes[name]; !ok || seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
		out := next[name]
		sort.Slice(out, func(i, j int) bool { return out[i].Outcome < out[j].Outcome })
		for _, edge := range out {
			queue = append(queue, edge.To)
		}
	}
	var rest []string
	for name := range t.Nodes {
		if !seen[name] {
			rest = append(rest, name)
		}
	}
	sort.Strings(rest)
	names = append(names, rest...)

	scene := &boardScene{raw: map[string]map[string]any{}, view: map[string]boardElementView{}}
	put := func(e map[string]any) {
		id := e["id"].(string)
		scene.order = append(scene.order, id)
		scene.raw[id] = e
		data, _ := json.Marshal(e)
		var v boardElementView
		_ = json.Unmarshal(data, &v)
		scene.view[id] = v
	}
	for _, name := range names {
		n := t.Nodes[name]
		typ, w, h := "rectangle", 170.0, 80.0
		switch n.Type {
		case model.NodeTypeDecision:
			typ, w, h = "diamond", 190.0, 110.0
		case model.NodeTypeStart, model.NodeTypeEnd:
			typ, w, h = "ellipse", 130.0, 70.0
		}
		e := base(processElementID(name), typ)
		e["x"], e["y"], e["width"], e["height"] = 0.0, 0.0, w, h
		e["backgroundColor"] = processStepColor(n)
		put(e)
		text := n.Name
		if text == "" {
			text = name
		}
		if len([]rune(text)) > 40 {
			text = string([]rune(text)[:39]) + "…"
		}
		tw, th := boardTextSize(text)
		label := base(processElementID(name)+"-label", "text")
		label["text"], label["fontSize"], label["textAlign"] = text, boardFontSize, "center"
		label["x"], label["y"], label["width"], label["height"] = w/2-tw/2, h/2-th/2, tw, th
		put(label)
	}
	outgoing := map[string]int{}
	for _, edge := range edges {
		outgoing[edge.From]++
	}
	var arrows []model.Edge
	for _, name := range names {
		for _, edge := range next[name] {
			if _, ok := t.Nodes[edge.From]; !ok {
				continue
			}
			if _, ok := t.Nodes[edge.To]; !ok {
				continue
			}
			e := base(processElementID(edge.From, edge.Outcome, edge.To), "arrow")
			e["x"], e["y"], e["points"] = 0.0, 0.0, [][]float64{{0, 0}, {1, 0}}
			e["startBinding"] = map[string]string{"elementId": processElementID(edge.From)}
			e["endBinding"] = map[string]string{"elementId": processElementID(edge.To)}
			put(e)
			arrows = append(arrows, edge)
		}
	}

	// Lay the steps out along the arrows, below what the board already has.
	offsetY := 0.0
	for _, id := range existing.order {
		if strings.HasPrefix(id, "p-") {
			continue
		}
		v := existing.view[id]
		box := elementBox(v)
		if bottom := box.Y + box.H + 2*gap; bottom > offsetY {
			offsetY = bottom
		}
	}
	deltas := map[string][2]float64{}
	for id, pos := range layoutPositions(scene, dir, gap) {
		v := scene.view[id]
		deltas[id] = [2]float64{pos[0] - v.X, pos[1] + offsetY - v.Y}
	}
	scene.moveBy(deltas)

	// An outcome that goes back (rework) detours under the steps (beside
	// them in a downward flow), so it does not lie on the arrow forward.
	rank := map[string]int{}
	for i, name := range names {
		rank[name] = i
	}
	mid := map[string][2]float64{}
	for _, edge := range arrows {
		arrowID := processElementID(edge.From, edge.Outcome, edge.To)
		mx, my := arrowMid(scene.view[arrowID])
		if rank[edge.To] <= rank[edge.From] {
			from, to := scene.view[processElementID(edge.From)], scene.view[processElementID(edge.To)]
			var sx, sy, cx, cy, ex, ey float64
			if dir == "down" {
				sx, sy = from.X+from.Width, from.Y+from.Height/2
				ex, ey = to.X+to.Width, to.Y+to.Height/2
				cx, cy = max(sx, ex)+gap, (sy+ey)/2
			} else {
				sx, sy = from.X+from.Width/2, from.Y+from.Height
				ex, ey = to.X+to.Width/2, to.Y+to.Height
				cx, cy = (sx+ex)/2, max(sy, ey)+gap
			}
			r := scene.raw[arrowID]
			r["x"], r["y"] = sx, sy
			r["points"] = [][]float64{{0, 0}, {cx - sx, cy - sy}, {ex - sx, ey - sy}}
			r["width"], r["height"] = max(sx, cx, ex)-min(sx, cx, ex), max(sy, cy, ey)-min(sy, cy, ey)
			mx, my = cx, cy+40
		}
		mid[arrowID] = [2]float64{mx, my}
	}

	// Outcome names only where a step has a choice.
	for _, edge := range arrows {
		if outgoing[edge.From] < 2 || edge.Outcome == "" {
			continue
		}
		arrowID := processElementID(edge.From, edge.Outcome, edge.To)
		mx, my := mid[arrowID][0], mid[arrowID][1]
		tw, th := boardTextSize(edge.Outcome)
		label := base(arrowID+"-label", "text")
		label["text"], label["fontSize"], label["textAlign"] = edge.Outcome, 16.0, "center"
		label["x"], label["y"], label["width"], label["height"] = mx-tw/2, my-th/2-12, tw, th
		put(label)
	}
	out := make([]map[string]any, 0, len(scene.order))
	for _, id := range scene.order {
		out = append(out, scene.raw[id])
	}
	return out
}

// processStepColor fills a step by who performs it.
func processStepColor(n model.Node) string {
	if n.Performer == nil {
		return "transparent"
	}
	switch n.Performer.Kind {
	case model.PerformerHuman:
		return "#ffec99"
	case model.PerformerAgent:
		return "#a5d8ff"
	case model.PerformerProgram:
		return "#b2f2bb"
	}
	return "transparent"
}
