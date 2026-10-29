package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
)

// Board placement helpers (tcl-hdkqqb). Agents draw by numbers, which makes
// raw x/y a poor way to diagram: these let an agent say where a shape goes
// relative to another (`board add --right-of`), move a shape together with
// its label and arrows (`board move`), and lay a whole flow out along its
// arrows (`board layout`).

const boardDefaultGap = 60.0

// boardScene is a board's live elements, decoded once both as generic JSON
// (to write back unchanged fields) and as views (for geometry).
type boardScene struct {
	order []string
	raw   map[string]map[string]any
	view  map[string]boardElementView
}

func loadBoardScene(doc *boardDoc) *boardScene {
	s := &boardScene{raw: map[string]map[string]any{}, view: map[string]boardElementView{}}
	for _, raw := range doc.Elements {
		var m map[string]any
		var v boardElementView
		if json.Unmarshal(raw, &m) != nil || json.Unmarshal(raw, &v) != nil || v.IsDeleted || v.ID == "" {
			continue
		}
		s.order = append(s.order, v.ID)
		s.raw[v.ID] = m
		s.view[v.ID] = v
	}
	return s
}

func isBoardShape(t string) bool {
	return t == "rectangle" || t == "ellipse" || t == "diamond"
}

// labelOwners maps each text element that labels something to its owner:
// an explicit `<id>-label` text belongs to <id>; otherwise a text belongs to
// the smallest shape containing its centre (so a label inside a shape inside
// a group box belongs to the shape, not the box), or failing that to an arrow
// whose midpoint it sits on.
func (s *boardScene) labelOwners() map[string]string {
	owners := map[string]string{}
	for _, id := range s.order {
		v := s.view[id]
		if v.Type != "text" {
			continue
		}
		if owner, ok := strings.CutSuffix(id, "-label"); ok {
			if _, exists := s.view[owner]; exists {
				owners[id] = owner
				continue
			}
		}
		cx, cy := v.X+v.Width/2, v.Y+v.Height/2
		best, bestArea := "", math.Inf(1)
		for _, sid := range s.order {
			sv := s.view[sid]
			if !isBoardShape(sv.Type) || cx < sv.X || cx > sv.X+sv.Width || cy < sv.Y || cy > sv.Y+sv.Height {
				continue
			}
			if area := sv.Width * sv.Height; area < bestArea {
				best, bestArea = sid, area
			}
		}
		if best == "" {
			for _, aid := range s.order {
				av := s.view[aid]
				if av.Type != "arrow" {
					continue
				}
				if mx, my := arrowMid(av); math.Hypot(cx-mx, cy-my) <= 12 {
					best = aid
					break
				}
			}
		}
		if best != "" {
			owners[id] = best
		}
	}
	return owners
}

// moveBy shifts elements (and the labels they own) by their deltas, then
// re-runs every arrow bound to a moved element: between both ends when both
// are bound, else from the bound end with the free tip kept where it was. It
// returns the ids it changed, in board order.
func (s *boardScene) moveBy(deltas map[string][2]float64) []string {
	owners := s.labelOwners()
	changed := map[string]bool{}
	shift := map[string][2]float64{}
	for id, d := range deltas {
		shift[id] = d
	}
	for label, owner := range owners {
		if _, own := deltas[label]; own {
			continue
		}
		if d, ok := deltas[owner]; ok {
			shift[label] = d
		}
	}
	for id, d := range shift {
		if d == [2]float64{} {
			continue
		}
		v := s.view[id]
		v.X += d[0]
		v.Y += d[1]
		s.view[id] = v
		s.raw[id]["x"], s.raw[id]["y"] = v.X, v.Y
		changed[id] = true
	}
	arrowLabels := map[string][]string{}
	for label, owner := range owners {
		if s.view[owner].Type == "arrow" {
			arrowLabels[owner] = append(arrowLabels[owner], label)
		}
	}
	for _, id := range s.order {
		v := s.view[id]
		if v.Type != "arrow" || changed[id] {
			continue
		}
		var from, to boardElementView
		var okFrom, okTo bool
		if v.StartBinding != nil {
			from, okFrom = s.view[v.StartBinding.ElementID]
		}
		if v.EndBinding != nil {
			to, okTo = s.view[v.EndBinding.ElementID]
		}
		if !(okFrom && changed[from.ID]) && !(okTo && changed[to.ID]) {
			continue
		}
		oldMidX, oldMidY := arrowMid(v)
		var sx, sy, ex, ey float64
		switch {
		case okFrom && okTo:
			sx, sy, ex, ey = arrowBetween(from, to)
		case okFrom:
			// The free tip stays; the tail leaves the moved shape's edge.
			ex, ey = arrowEnd(v)
			sx, sy, _, _ = arrowBetween(from, boardElementView{X: ex, Y: ey})
		default:
			sx, sy = v.X, v.Y
			_, _, ex, ey = arrowBetween(boardElementView{X: sx, Y: sy}, to)
		}
		v.X, v.Y = sx, sy
		v.Points = [][]float64{{0, 0}, {ex - sx, ey - sy}}
		v.Width, v.Height = math.Abs(ex-sx), math.Abs(ey-sy)
		s.view[id] = v
		r := s.raw[id]
		r["x"], r["y"] = sx, sy
		r["points"] = v.Points
		r["width"], r["height"] = v.Width, v.Height
		changed[id] = true
		// An arrow's label rides at its midpoint.
		midX, midY := arrowMid(v)
		for _, label := range arrowLabels[id] {
			if changed[label] {
				continue
			}
			lv := s.view[label]
			lv.X += midX - oldMidX
			lv.Y += midY - oldMidY
			s.view[label] = lv
			s.raw[label]["x"], s.raw[label]["y"] = lv.X, lv.Y
			changed[label] = true
		}
	}
	var out []string
	for _, id := range s.order {
		if changed[id] {
			out = append(out, id)
		}
	}
	return out
}

func arrowEnd(v boardElementView) (float64, float64) {
	if len(v.Points) == 0 || len(v.Points[len(v.Points)-1]) < 2 {
		return v.X, v.Y
	}
	end := v.Points[len(v.Points)-1]
	return v.X + end[0], v.Y + end[1]
}

func arrowMid(v boardElementView) (float64, float64) {
	if len(v.Points) == 0 {
		return v.X, v.Y
	}
	end := v.Points[len(v.Points)-1]
	if len(end) < 2 {
		return v.X, v.Y
	}
	return v.X + end[0]/2, v.Y + end[1]/2
}

// patchScene bumps and patches the named elements.
func patchScene(id string, s *boardScene, ids []string, stdout, stderr io.Writer) int {
	if len(ids) == 0 {
		fmt.Fprintf(stdout, "Board %s: nothing to change.\n", id)
		return rcOK
	}
	out := make([]json.RawMessage, 0, len(ids))
	for _, elemID := range ids {
		m := s.raw[elemID]
		version, _ := m["version"].(float64)
		delete(m, "server_version")
		m["version"] = int64(version) + 1
		m["versionNonce"] = boardRandomNonce()
		m["updated"] = time.Now().UnixMilli()
		raw, err := json.Marshal(m)
		if err != nil {
			fmt.Fprintf(stderr, "Error: %v\n", err)
			return rcIOFailure
		}
		out = append(out, raw)
	}
	resp, err := patchBoard(id, out)
	if err != nil {
		return boardErr(stderr, err)
	}
	reportPatch(stdout, id, resp, len(out))
	return rcOK
}

// placeRelative returns the top-left for a w×h box beside ref in dir
// ("right", "left", "below", "above"), centred on ref along the other axis.
func placeRelative(ref boardElementView, w, h float64, dir string, gap float64) (float64, float64) {
	cx, cy := ref.X+ref.Width/2, ref.Y+ref.Height/2
	switch dir {
	case "left":
		return ref.X - gap - w, cy - h/2
	case "below":
		return cx - w/2, ref.Y + ref.Height + gap
	case "above":
		return cx - w/2, ref.Y - gap - h
	default:
		return ref.X + ref.Width + gap, cy - h/2
	}
}

// relativeTarget picks the one placement flag given on board add.
func relativeTarget(p *boardAddParams) (dir, ref string, err error) {
	set := 0
	for _, c := range []struct{ dir, ref string }{{"right", p.RightOf}, {"left", p.LeftOf}, {"below", p.Below}, {"above", p.Above}} {
		if c.ref != "" {
			dir, ref = c.dir, c.ref
			set++
		}
	}
	if set > 1 {
		return "", "", fmt.Errorf("give only one of --right-of, --left-of, --below, --above")
	}
	if p.Gap != nil && (set == 0 || *p.Gap < 0) {
		return "", "", fmt.Errorf("--gap applies to --right-of, --left-of, --below or --above, and must not be negative")
	}
	if set == 1 && (p.X != nil || p.Y != nil) {
		return "", "", fmt.Errorf("--x/--y and a relative placement (--right-of, ...) are exclusive")
	}
	return dir, ref, nil
}

// ---- layout ----

type boardLayoutParams struct {
	Direction string   `long:"direction" optional:"true" help:"Flow direction: right (default) or down"`
	Gap       *float64 `long:"gap" help:"Space between shapes (default 60)"`
}

func boardLayoutCmd() *cobra.Command {
	return boa.CmdT[boardLayoutParams]{
		Use:   "layout <board>",
		Short: "Lay out a board's shapes along their arrows",
		Long: "Arranges the board's shapes as a flow: each shape is placed one step after the shapes that point at it " +
			"(left to right, or top to bottom with --direction down), shapes in the same step are stacked, labels move " +
			"with their shapes and arrows are re-run. Shapes no arrow touches are stacked after the flow. Text that labels " +
			"nothing, lines and freehand strokes stay where they are.",
		Args:        cobra.ExactArgs(1),
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *boardLayoutParams, _ *cobra.Command, args []string) {
			os.Exit(runBoardLayout(p, args[0], os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runBoardLayout(p *boardLayoutParams, id string, stdout, stderr io.Writer) int {
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
	doc, err := fetchBoard(id, -1)
	if err != nil {
		return boardErr(stderr, err)
	}
	s := loadBoardScene(doc)
	targets := layoutPositions(s, dir, gap)
	deltas := map[string][2]float64{}
	for shapeID, pos := range targets {
		v := s.view[shapeID]
		deltas[shapeID] = [2]float64{pos[0] - v.X, pos[1] - v.Y}
	}
	return patchScene(id, s, s.moveBy(deltas), stdout, stderr)
}

// onCycle reports whether start reaches itself along arrows through shapes
// that are not placed yet.
func onCycle(start string, next map[string][]string, done map[string]bool) bool {
	seen := map[string]bool{}
	stack := append([]string(nil), next[start]...)
	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if n == start {
			return true
		}
		if done[n] || seen[n] {
			continue
		}
		seen[n] = true
		stack = append(stack, next[n]...)
	}
	return false
}

// layoutPositions computes a top-left for every shape: layers by longest
// path along arrows from the shapes nothing points at, in board order within
// a layer. A cycle is broken where it is first met.
func layoutPositions(s *boardScene, dir string, gap float64) map[string][2]float64 {
	var shapes []string
	isShape := map[string]bool{}
	for _, id := range s.order {
		if isBoardShape(s.view[id].Type) {
			shapes = append(shapes, id)
			isShape[id] = true
		}
	}
	next := map[string][]string{}
	indeg := map[string]int{}
	linked := map[string]bool{}
	// The editor binds an arrow drawn from a labelled shape to the label text
	// on top of it; such an arrow joins the shape the text labels.
	owners := s.labelOwners()
	endpoint := func(id string) string {
		if owner, ok := owners[id]; ok && s.view[id].Type == "text" {
			return owner
		}
		return id
	}
	for _, id := range s.order {
		v := s.view[id]
		if v.Type != "arrow" || v.StartBinding == nil || v.EndBinding == nil {
			continue
		}
		a, b := endpoint(v.StartBinding.ElementID), endpoint(v.EndBinding.ElementID)
		if !isShape[a] || !isShape[b] || a == b {
			continue
		}
		next[a] = append(next[a], b)
		indeg[b]++
		linked[a], linked[b] = true, true
	}
	// Longest path from the sources (Kahn's order). A cycle has no source:
	// its first shape in board order is taken as one, which breaks it there.
	layer := map[string]int{}
	done := map[string]bool{}
	var queue []string
	for _, id := range shapes {
		if linked[id] && indeg[id] == 0 {
			queue = append(queue, id)
		}
	}
	for {
		for len(queue) > 0 {
			u := queue[0]
			queue = queue[1:]
			if done[u] {
				continue
			}
			done[u] = true
			for _, n := range next[u] {
				if done[n] {
					continue
				}
				layer[n] = max(layer[n], layer[u]+1)
				if indeg[n]--; indeg[n] == 0 {
					queue = append(queue, n)
				}
			}
		}
		// Only cycles are left: break one at its first shape in board order
		// that is on a cycle (reaches itself through unplaced shapes).
		stuck := ""
		for _, id := range shapes {
			if linked[id] && !done[id] && onCycle(id, next, done) {
				stuck = id
				break
			}
		}
		if stuck == "" {
			break
		}
		queue = append(queue, stuck)
	}
	layers := map[int][]string{}
	maxLayer := -1
	for _, id := range shapes {
		if linked[id] {
			layers[layer[id]] = append(layers[layer[id]], id)
			maxLayer = max(maxLayer, layer[id])
		}
	}
	var loose []string
	for _, id := range shapes {
		if !linked[id] {
			loose = append(loose, id)
		}
	}
	// Anchor the layout where the flow already starts.
	ox, oy := math.Inf(1), math.Inf(1)
	for _, id := range shapes {
		ox, oy = math.Min(ox, s.view[id].X), math.Min(oy, s.view[id].Y)
	}
	out := map[string][2]float64{}
	if len(shapes) == 0 {
		return out
	}
	along := ox
	if dir == "down" {
		along = oy
	}
	keys := make([]int, 0, len(layers))
	for k := range layers {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	for _, k := range keys {
		ids := layers[k]
		thick := 0.0 // the layer's extent along the flow
		for _, id := range ids {
			v := s.view[id]
			if dir == "right" {
				thick = math.Max(thick, v.Width)
			} else {
				thick = math.Max(thick, v.Height)
			}
		}
		across := oy
		if dir == "down" {
			across = ox
		}
		for _, id := range ids {
			v := s.view[id]
			if dir == "right" {
				out[id] = [2]float64{along + (thick-v.Width)/2, across}
				across += v.Height + gap
			} else {
				out[id] = [2]float64{across, along + (thick-v.Height)/2}
				across += v.Width + gap
			}
		}
		along += thick + gap*1.5
	}
	// Loose shapes go in a row (or column) after the flow.
	pos := along
	if maxLayer < 0 {
		pos = ox
		if dir == "down" {
			pos = oy
		}
	}
	cross := oy
	if dir == "down" {
		cross = ox
	}
	for _, id := range loose {
		v := s.view[id]
		if dir == "right" {
			out[id] = [2]float64{pos, cross}
			cross += v.Height + gap
		} else {
			out[id] = [2]float64{cross, pos}
			cross += v.Width + gap
		}
	}
	return out
}
