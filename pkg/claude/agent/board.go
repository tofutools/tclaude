package agent

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
)

// `tclaude agent board` — collab boards (tcl-4f86kp): a shared drawing board
// the operator edits in the dashboard and agents read and write here. Its
// elements are a subset of Excalidraw's element JSON. Reading needs
// boards.read; creating and changing boards needs boards.write.

func boardCmd() *cobra.Command {
	return boa.CmdT[struct{}]{
		Use:   "board",
		Short: "Read and draw on shared collab boards",
		Long: "Collab boards are shared drawing boards: the operator draws in the dashboard's board editor, and agents " +
			"read and change the same board here. Elements are a subset of Excalidraw's element JSON " +
			"(rectangle, ellipse, diamond, arrow, line, freedraw, text), merged per element: a higher version wins, " +
			"and a deletion is a tombstone (isDeleted).\n\n" +
			"Reading (ls, show, snapshot) needs boards.read; create and every change need boards.write. " +
			"Changing a board makes you a participant, so the operator's board activity reaches you as mail.\n\n" +
			"Typical loop: `board show <id>` to read it, `board snapshot <id> -o board.png` to see the operator's last " +
			"rendering, `board add` / `move` / `text` / `rm` to draw, then `board show <id> --since <version>` for " +
			"what changed since.",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds: []*cobra.Command{
			boardLsCmd(), boardShowCmd(), boardSnapshotCmd(), boardCreateCmd(),
			boardAddCmd(), boardPatchCmd(), boardRmCmd(), boardMoveCmd(), boardTextCmd(), boardLayoutCmd(),
		},
	}.ToCobra()
}

// ---- wire shapes ----

type boardSummary struct {
	ID           string    `json:"id"`
	Title        string    `json:"title"`
	Version      int64     `json:"version"`
	UpdatedAt    time.Time `json:"updated_at"`
	ElementCount int       `json:"element_count"`
	Participants []string  `json:"participants"`
}

type boardDoc struct {
	ID              string            `json:"id"`
	Title           string            `json:"title"`
	Version         int64             `json:"version"`
	UpdatedAt       time.Time         `json:"updated_at"`
	UpdatedBy       string            `json:"updated_by"`
	Participants    []string          `json:"participants"`
	SnapshotVersion *int64            `json:"snapshot_version,omitempty"`
	Elements        []json.RawMessage `json:"elements"`
}

// boardElementView is the part of an element the CLI reads.
type boardElementView struct {
	ID           string      `json:"id"`
	Type         string      `json:"type"`
	X            float64     `json:"x"`
	Y            float64     `json:"y"`
	Width        float64     `json:"width"`
	Height       float64     `json:"height"`
	Text         string      `json:"text"`
	Points       [][]float64 `json:"points"`
	Version      int64       `json:"version"`
	IsDeleted    bool        `json:"isDeleted"`
	StartBinding *struct {
		ElementID string `json:"elementId"`
	} `json:"startBinding"`
	EndBinding *struct {
		ElementID string `json:"elementId"`
	} `json:"endBinding"`
	CustomData *struct {
		Author string `json:"author"`
	} `json:"customData"`
}

func boardPath(id string, tail ...string) string {
	p := "/v1/boards/" + url.PathEscape(id)
	for _, t := range tail {
		p += "/" + t
	}
	return p
}

func fetchBoard(id string, since int64) (*boardDoc, error) {
	path := boardPath(id)
	if since >= 0 {
		path += "?since=" + strconv.FormatInt(since, 10)
	}
	var doc boardDoc
	if err := DaemonRequest(http.MethodGet, path, nil, &doc, DaemonOpts{}); err != nil {
		return nil, err
	}
	return &doc, nil
}

type boardPatchResp struct {
	Version  int64 `json:"version"`
	Accepted int   `json:"accepted"`
}

func patchBoard(id string, elements []json.RawMessage) (*boardPatchResp, error) {
	var resp boardPatchResp
	err := DaemonRequest(http.MethodPatch, boardPath(id), map[string]any{"elements": elements}, &resp, DaemonOpts{})
	return &resp, err
}

func reportPatch(stdout io.Writer, id string, resp *boardPatchResp, sent int) {
	if resp.Accepted < sent {
		fmt.Fprintf(stdout, "Board %s is at version %d: %d of %d element(s) accepted (the rest were older than the board's copy).\n",
			id, resp.Version, resp.Accepted, sent)
		return
	}
	fmt.Fprintf(stdout, "Board %s is at version %d (%d element(s) accepted).\n", id, resp.Version, resp.Accepted)
}

func boardErr(stderr io.Writer, err error) int {
	fmt.Fprintf(stderr, "Error: %v\n", err)
	return MapDaemonErrorToRC(err)
}

// boardRandomID returns a 20-character element id.
func boardRandomID() string {
	var b [10]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// boardRandomNonce returns a positive 31-bit nonce, as Excalidraw uses.
func boardRandomNonce() int64 {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return int64(binary.BigEndian.Uint32(b[:]) & 0x7fffffff)
}

// ---- ls ----

type boardLsParams struct {
	JSON bool `long:"json" help:"Output JSON"`
}

func boardLsCmd() *cobra.Command {
	return boa.CmdT[boardLsParams]{
		Use:         "ls",
		Short:       "List boards",
		Args:        cobra.NoArgs,
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *boardLsParams, _ *cobra.Command, _ []string) {
			os.Exit(runBoardLs(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runBoardLs(p *boardLsParams, stdout, stderr io.Writer) int {
	var resp struct {
		Boards []boardSummary `json:"boards"`
	}
	if err := DaemonRequest(http.MethodGet, "/v1/boards", nil, &resp, DaemonOpts{}); err != nil {
		return boardErr(stderr, err)
	}
	if p.JSON {
		return writeBoardJSON(stdout, resp)
	}
	if len(resp.Boards) == 0 {
		fmt.Fprintln(stdout, "No boards.")
		return rcOK
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tTITLE\tVERSION\tELEMENTS\tPARTICIPANTS\tUPDATED")
	for _, b := range resp.Boards {
		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%d\t%s\n", b.ID, b.Title, b.Version, b.ElementCount,
			len(b.Participants), b.UpdatedAt.Local().Format("2006-01-02 15:04"))
	}
	_ = tw.Flush()
	return rcOK
}

func writeBoardJSON(stdout io.Writer, v any) int {
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
	return rcOK
}

// ---- show ----

type boardShowParams struct {
	Since *int64 `long:"since" help:"Only elements changed after this board version, deletions included"`
	JSON  bool   `long:"json" help:"Output the board JSON (elements in Excalidraw form)"`
}

func boardShowCmd() *cobra.Command {
	return boa.CmdT[boardShowParams]{
		Use:   "show <board>",
		Short: "Show a board's elements",
		Long: "Prints the board version and one line per live element: type, id, rounded x,y,w,h, a text excerpt, " +
			"and for arrows their bound endpoints. With --since N only the elements changed after version N are " +
			"listed, deletions included. Remember the printed version and pass it as --since next time.",
		Args:        cobra.ExactArgs(1),
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *boardShowParams, _ *cobra.Command, args []string) {
			os.Exit(runBoardShow(p, args[0], os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runBoardShow(p *boardShowParams, id string, stdout, stderr io.Writer) int {
	since := int64(-1)
	if p.Since != nil {
		if *p.Since < 0 {
			fmt.Fprintln(stderr, "Error: --since must be >= 0")
			return rcInvalidArg
		}
		since = *p.Since
	}
	doc, err := fetchBoard(id, since)
	if err != nil {
		return boardErr(stderr, err)
	}
	if p.JSON {
		return writeBoardJSON(stdout, doc)
	}
	fmt.Fprintf(stdout, "Board %s %q: version %d", doc.ID, doc.Title, doc.Version)
	if doc.UpdatedBy != "" {
		fmt.Fprintf(stdout, ", last changed by %s", doc.UpdatedBy)
	}
	fmt.Fprintln(stdout)
	if len(doc.Participants) > 0 {
		fmt.Fprintf(stdout, "Participants: %s\n", strings.Join(doc.Participants, ", "))
	}
	if since >= 0 {
		fmt.Fprintf(stdout, "%d element(s) changed since version %d:\n", len(doc.Elements), since)
	} else {
		fmt.Fprintf(stdout, "%d element(s):\n", len(doc.Elements))
	}
	for _, raw := range doc.Elements {
		var e boardElementView
		if err := json.Unmarshal(raw, &e); err != nil {
			continue
		}
		fmt.Fprintln(stdout, "  "+formatBoardElement(e))
	}
	return rcOK
}

func formatBoardElement(e boardElementView) string {
	if e.IsDeleted {
		return fmt.Sprintf("%-9s %s  (deleted)", e.Type, e.ID)
	}
	line := fmt.Sprintf("%-9s %s  x=%d y=%d w=%d h=%d", e.Type, e.ID,
		int64(math.Round(e.X)), int64(math.Round(e.Y)), int64(math.Round(e.Width)), int64(math.Round(e.Height)))
	if e.StartBinding != nil || e.EndBinding != nil {
		from, to := "-", "-"
		if e.StartBinding != nil {
			from = e.StartBinding.ElementID
		}
		if e.EndBinding != nil {
			to = e.EndBinding.ElementID
		}
		line += fmt.Sprintf("  %s -> %s", from, to)
	}
	if t := strings.TrimSpace(e.Text); t != "" {
		t = strings.Join(strings.Fields(t), " ")
		if r := []rune(t); len(r) > 60 {
			t = string(r[:57]) + "..."
		}
		line += fmt.Sprintf("  %q", t)
	}
	if e.CustomData != nil && e.CustomData.Author != "" {
		line += "  by " + e.CustomData.Author
	}
	return line
}

// ---- snapshot ----

type boardSnapshotParams struct {
	Output string `long:"output" short:"o" help:"File to write the PNG to"`
	Region int    `long:"region" optional:"true" help:"Save the image of one operator change instead, as numbered in its board event mail"`
}

func boardSnapshotCmd() *cobra.Command {
	return boa.CmdT[boardSnapshotParams]{
		Use:   "snapshot <board>",
		Short: "Save the board's latest PNG snapshot",
		Long: "Writes the latest PNG rendering of the board, as the operator's dashboard last saved it, and says " +
			"how far it lags the board's current version. Read it with an image-capable tool to see the layout. " +
			"With --region N it saves the cropped image of the change a board event mail named instead.",
		Args:        cobra.ExactArgs(1),
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *boardSnapshotParams, _ *cobra.Command, args []string) {
			os.Exit(runBoardSnapshot(p, args[0], os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runBoardSnapshot(p *boardSnapshotParams, id string, stdout, stderr io.Writer) int {
	if strings.TrimSpace(p.Output) == "" {
		fmt.Fprintln(stderr, "Error: -o <file.png> is required")
		return rcInvalidArg
	}
	if p.Region < 0 {
		fmt.Fprintln(stderr, "Error: --region must be a positive number from a board event")
		return rcInvalidArg
	}
	path := boardPath(id, "snapshot")
	if p.Region > 0 {
		path += "?region=" + strconv.Itoa(p.Region)
	}
	data, hdr, err := DaemonGetRaw(path)
	if err != nil {
		return boardErr(stderr, err)
	}
	if err := os.WriteFile(p.Output, data, 0o600); err != nil {
		fmt.Fprintf(stderr, "Error: write %s: %v\n", p.Output, err)
		return rcIOFailure
	}
	if p.Region > 0 {
		fmt.Fprintf(stdout, "Wrote %s (%d bytes): the region the operator changed in event image %d. "+
			"Use `board show` for the board's current elements.\n", p.Output, len(data), p.Region)
		return rcOK
	}
	fmt.Fprintf(stdout, "Wrote %s (%d bytes). %s\n", p.Output, len(data),
		snapshotAge(hdr.Get("X-Board-Snapshot-Version"), hdr.Get("X-Board-Version")))
	return rcOK
}

func snapshotAge(snapHeader, boardHeader string) string {
	snap, err1 := strconv.ParseInt(snapHeader, 10, 64)
	board, err2 := strconv.ParseInt(boardHeader, 10, 64)
	switch {
	case err1 != nil:
		return "The snapshot's board version is unknown."
	case err2 != nil:
		return fmt.Sprintf("It shows board version %d.", snap)
	case snap >= board:
		return fmt.Sprintf("It shows the current board version (%d).", board)
	default:
		return fmt.Sprintf("It shows board version %d; the board is now at version %d (%d version(s) newer), "+
			"so use `board show` for the latest elements.", snap, board, board-snap)
	}
}

// ---- create ----

type boardCreateParams struct {
	Title string `long:"title" optional:"true" help:"Board title (defaults to the id)"`
}

func boardCreateCmd() *cobra.Command {
	return boa.CmdT[boardCreateParams]{
		Use:         "create <board>",
		Short:       "Create a board",
		Long:        "Creates an empty board. The id is lowercase letters, digits and dashes (at most 64).",
		Args:        cobra.ExactArgs(1),
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *boardCreateParams, _ *cobra.Command, args []string) {
			os.Exit(runBoardCreate(p, args[0], os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runBoardCreate(p *boardCreateParams, id string, stdout, stderr io.Writer) int {
	var doc boardDoc
	if err := DaemonRequest(http.MethodPost, "/v1/boards", map[string]string{"id": id, "title": p.Title}, &doc, DaemonOpts{}); err != nil {
		return boardErr(stderr, err)
	}
	fmt.Fprintf(stdout, "Created board %s %q.\n", doc.ID, doc.Title)
	return rcOK
}

// ---- add ----

type boardAddParams struct {
	X     *float64 `long:"x" help:"Left edge (default 0)"`
	Y     *float64 `long:"y" help:"Top edge (default 0)"`
	W     *float64 `long:"w" help:"Width (default 160 for shapes; arrows and lines: the x extent)"`
	H     *float64 `long:"h" help:"Height (default 80 for shapes; arrows and lines: the y extent)"`
	Text  string   `long:"text" optional:"true" help:"Text (required for text elements); on a shape or arrow it adds a centred text label element"`
	Color string   `long:"color" optional:"true" help:"Stroke color: #rrggbb, #rgb or transparent (default #1e1e1e)"`
	Bg    string   `long:"bg" optional:"true" help:"Fill color: #rrggbb, #rgb or transparent (default transparent)"`
	From  string   `long:"from" optional:"true" help:"Arrow: element id to start from (points are computed between the element centers)"`
	To    string   `long:"to" optional:"true" help:"Arrow: element id to point at"`
	ID    string   `long:"id" optional:"true" help:"Element id (default: generated)"`

	RightOf string   `long:"right-of" optional:"true" help:"Place the shape to the right of this element id (centred on it), instead of --x/--y"`
	LeftOf  string   `long:"left-of" optional:"true" help:"Place the shape to the left of this element id"`
	Below   string   `long:"below" optional:"true" help:"Place the shape below this element id"`
	Above   string   `long:"above" optional:"true" help:"Place the shape above this element id"`
	Gap     *float64 `long:"gap" help:"Space to the --right-of/--left-of/--below/--above element (default 60)"`
	Link    bool     `long:"link" optional:"true" help:"With a relative placement: also draw an arrow from that element to the new shape"`
}

var boardAddTypes = map[string]bool{"rectangle": true, "ellipse": true, "diamond": true, "text": true, "arrow": true, "line": true}

func boardAddCmd() *cobra.Command {
	return boa.CmdT[boardAddParams]{
		Use:   "add <board> <rectangle|ellipse|diamond|text|arrow|line>",
		Short: "Add an element to a board",
		Long: "Adds one element and prints its id. Shapes take --x --y --w --h; text takes --text (and --x --y); " +
			"--text on a shape or arrow adds a second, centred text element as its label (move it with the shape); " +
			"an arrow either connects two elements (--from and --to, by element id) or spans --x --y by --w --h.\n\n" +
			"Place a shape relative to another with --right-of, --left-of, --below or --above <id> (centred on it, --gap " +
			"apart); --link also draws an arrow from that element to the new one, so a flow is built step by step.\n\n" +
			"Example: tclaude agent board add plan rectangle --x 100 --y 80 --text 'API' --id api\n" +
			"         tclaude agent board add plan rectangle --right-of api --link --text 'DB' --id db\n" +
			"         tclaude agent board add plan arrow --from <idA> --to <idB>",
		Args:        cobra.ExactArgs(2),
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *boardAddParams, _ *cobra.Command, args []string) {
			os.Exit(runBoardAdd(p, args[0], args[1], os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func floatOr(v *float64, def float64) float64 {
	if v == nil {
		return def
	}
	return *v
}

func runBoardAdd(p *boardAddParams, id, typ string, stdout, stderr io.Writer) int {
	if !boardAddTypes[typ] {
		fmt.Fprintln(stderr, "Error: type must be one of rectangle, ellipse, diamond, text, arrow, line")
		return rcInvalidArg
	}
	if typ == "text" && strings.TrimSpace(p.Text) == "" {
		fmt.Fprintln(stderr, "Error: a text element needs --text")
		return rcInvalidArg
	}
	if (p.From != "" || p.To != "") && typ != "arrow" {
		fmt.Fprintln(stderr, "Error: --from and --to only apply to arrows")
		return rcInvalidArg
	}
	if (p.From == "") != (p.To == "") {
		fmt.Fprintln(stderr, "Error: give both --from and --to")
		return rcInvalidArg
	}
	relDir, relRef, err := relativeTarget(p)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcInvalidArg
	}
	if relRef != "" && (typ == "arrow" || typ == "line") {
		fmt.Fprintln(stderr, "Error: arrows and lines are placed with --from/--to or --x/--y, not --right-of and friends")
		return rcInvalidArg
	}
	if p.Link && relRef == "" {
		fmt.Fprintln(stderr, "Error: --link needs a relative placement (--right-of, --left-of, --below or --above)")
		return rcInvalidArg
	}
	var ref boardElementView
	if relRef != "" {
		doc, err := fetchBoard(id, -1)
		if err != nil {
			return boardErr(stderr, err)
		}
		v, ok := loadBoardScene(doc).view[relRef]
		if !ok {
			fmt.Fprintf(stderr, "Error: board %s has no live element %s\n", id, relRef)
			return rcNotFound
		}
		ref = v
	}
	elemID := p.ID
	if elemID == "" {
		elemID = boardRandomID()
	}
	stroke := p.Color
	if stroke == "" {
		stroke = "#1e1e1e"
	}
	bg := p.Bg
	if bg == "" {
		bg = "transparent"
	}
	e := map[string]any{
		"id": elemID, "type": typ, "version": 1, "versionNonce": boardRandomNonce(), "isDeleted": false,
		"updated": time.Now().UnixMilli(), "strokeColor": stroke, "backgroundColor": bg,
		"fillStyle": "solid", "strokeWidth": 2, "strokeStyle": "solid", "roughness": 1, "opacity": 100, "angle": 0,
	}
	x, y := floatOr(p.X, 0), floatOr(p.Y, 0)
	if relRef != "" {
		w, h := floatOr(p.W, 160), floatOr(p.H, 80)
		if typ == "text" {
			tw, th := boardTextSize(p.Text)
			w, h = floatOr(p.W, tw), floatOr(p.H, th)
		}
		x, y = placeRelative(ref, w, h, relDir, floatOr(p.Gap, boardDefaultGap))
	}
	// A shape's or arrow's label is its own text element centred on it, the
	// way the board editor reads and draws labels.
	var labelX, labelY float64
	switch typ {
	case "text":
		w, h := boardTextSize(p.Text)
		e["text"], e["fontSize"], e["textAlign"] = p.Text, boardFontSize, "left"
		e["x"], e["y"] = x, y
		e["width"], e["height"] = floatOr(p.W, w), floatOr(p.H, h)
	case "arrow", "line":
		if p.From != "" {
			doc, err := fetchBoard(id, -1)
			if err != nil {
				return boardErr(stderr, err)
			}
			from, to, ok := findBoardElements(doc, p.From, p.To)
			if !ok {
				fmt.Fprintf(stderr, "Error: board %s has no live element %s or %s\n", id, p.From, p.To)
				return rcNotFound
			}
			sx, sy, ex, ey := arrowBetween(from, to)
			x, y = sx, sy
			e["points"] = [][2]float64{{0, 0}, {ex - sx, ey - sy}}
			e["width"], e["height"] = math.Abs(ex-sx), math.Abs(ey-sy)
			e["startBinding"] = map[string]string{"elementId": p.From}
			e["endBinding"] = map[string]string{"elementId": p.To}
		} else {
			w, h := floatOr(p.W, 160), floatOr(p.H, 0)
			e["points"] = [][2]float64{{0, 0}, {w, h}}
			e["width"], e["height"] = math.Abs(w), math.Abs(h)
		}
		e["x"], e["y"] = x, y
		end := e["points"].([][2]float64)[1]
		labelX, labelY = x+end[0]/2, y+end[1]/2
	default:
		w, h := floatOr(p.W, 160), floatOr(p.H, 80)
		e["x"], e["y"] = x, y
		e["width"], e["height"] = w, h
		labelX, labelY = x+w/2, y+h/2
	}
	elems := []map[string]any{e}
	labelID := ""
	if typ != "text" && p.Text != "" {
		labelID = elemID + "-label"
		if len(labelID) > 64 {
			labelID = boardRandomID()
		}
		w, h := boardTextSize(p.Text)
		elems = append(elems, map[string]any{
			"id": labelID, "type": "text", "version": 1, "versionNonce": boardRandomNonce(), "isDeleted": false,
			"updated": e["updated"], "strokeColor": stroke, "backgroundColor": "transparent", "opacity": 100, "angle": 0,
			"text": p.Text, "fontSize": boardFontSize, "textAlign": "center",
			"x": labelX - w/2, "y": labelY - h/2, "width": w, "height": h,
		})
	}
	linkID := ""
	if p.Link {
		linkID = boardRandomID()
		self := boardElementView{ID: elemID, Type: typ, X: e["x"].(float64), Y: e["y"].(float64)}
		self.Width, _ = e["width"].(float64)
		self.Height, _ = e["height"].(float64)
		sx, sy, ex, ey := arrowBetween(ref, self)
		elems = append(elems, map[string]any{
			"id": linkID, "type": "arrow", "version": 1, "versionNonce": boardRandomNonce(), "isDeleted": false,
			"updated": e["updated"], "strokeColor": stroke, "backgroundColor": "transparent",
			"fillStyle": "solid", "strokeWidth": 2, "strokeStyle": "solid", "roughness": 1, "opacity": 100, "angle": 0,
			"x": sx, "y": sy, "width": math.Abs(ex - sx), "height": math.Abs(ey - sy),
			"points":       [][2]float64{{0, 0}, {ex - sx, ey - sy}},
			"startBinding": map[string]string{"elementId": relRef}, "endBinding": map[string]string{"elementId": elemID},
		})
	}
	raws := make([]json.RawMessage, 0, len(elems))
	for _, el := range elems {
		raw, err := json.Marshal(el)
		if err != nil {
			fmt.Fprintf(stderr, "Error: %v\n", err)
			return rcIOFailure
		}
		raws = append(raws, raw)
	}
	resp, err := patchBoard(id, raws)
	if err != nil {
		return boardErr(stderr, err)
	}
	extra := ""
	if labelID != "" {
		extra += " with label " + labelID
	}
	if linkID != "" {
		extra += fmt.Sprintf(" and arrow %s from %s", linkID, relRef)
	}
	if extra != "" {
		fmt.Fprintf(stdout, "Added %s %s%s to board %s (version %d).\n", typ, elemID, extra, id, resp.Version)
		return rcOK
	}
	fmt.Fprintf(stdout, "Added %s %s to board %s (version %d).\n", typ, elemID, id, resp.Version)
	return rcOK
}

const boardFontSize = 20.0

// boardTextSize estimates a text element's box at boardFontSize.
func boardTextSize(text string) (w, h float64) {
	lines := strings.Split(text, "\n")
	widest := 0
	for _, l := range lines {
		widest = max(widest, len([]rune(l)))
	}
	return math.Ceil(float64(widest) * boardFontSize * 0.6), math.Ceil(float64(len(lines)) * boardFontSize * 1.25)
}

func findBoardElements(doc *boardDoc, fromID, toID string) (from, to boardElementView, ok bool) {
	var haveFrom, haveTo bool
	for _, raw := range doc.Elements {
		var e boardElementView
		if json.Unmarshal(raw, &e) != nil || e.IsDeleted {
			continue
		}
		if e.ID == fromID {
			from, haveFrom = e, true
		}
		if e.ID == toID {
			to, haveTo = e, true
		}
	}
	return from, to, haveFrom && haveTo
}

// arrowBetween runs an arrow along the line between the two elements'
// centers, clipped to their bounding boxes so the head is not hidden under
// the target. Overlapping boxes fall back to the bare centers.
func arrowBetween(from, to boardElementView) (sx, sy, ex, ey float64) {
	fcx, fcy := from.X+from.Width/2, from.Y+from.Height/2
	tcx, tcy := to.X+to.Width/2, to.Y+to.Height/2
	dx, dy := tcx-fcx, tcy-fcy
	clip := func(hw, hh float64) float64 {
		t := math.Inf(1)
		if dx != 0 {
			t = math.Min(t, math.Abs(hw/dx))
		}
		if dy != 0 {
			t = math.Min(t, math.Abs(hh/dy))
		}
		return t
	}
	t0, t1 := clip(from.Width/2, from.Height/2), 1-clip(to.Width/2, to.Height/2)
	if math.IsInf(t0, 0) || t0 >= t1 {
		return fcx, fcy, tcx, tcy
	}
	return fcx + dx*t0, fcy + dy*t0, fcx + dx*t1, fcy + dy*t1
}

// ---- patch ----

type boardPatchParams struct {
	File string `long:"file" help:"JSON file of elements (- reads stdin): an array, {\"elements\": [...]}, or an .excalidraw file"`
}

func boardPatchCmd() *cobra.Command {
	return boa.CmdT[boardPatchParams]{
		Use:   "patch <board> --file <elements.json>",
		Short: "Apply Excalidraw elements to a board",
		Long: "Sends raw Excalidraw elements. Each merges on its own: it replaces the board's copy when its version " +
			"is higher (a tie goes to the lower versionNonce). To change an element, send it with version + 1; " +
			"to delete it, also set isDeleted. Unknown keys are dropped; customData.author is set to you.",
		Args:        cobra.ExactArgs(1),
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *boardPatchParams, _ *cobra.Command, args []string) {
			os.Exit(runBoardPatch(p, args[0], os.Stdin, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runBoardPatch(p *boardPatchParams, id string, stdin io.Reader, stdout, stderr io.Writer) int {
	if p.File == "" {
		fmt.Fprintln(stderr, "Error: --file is required (- reads stdin)")
		return rcInvalidArg
	}
	text, rc := resolveBodyInput("", p.File, "", stdin, stderr)
	if rc != rcOK {
		return rc
	}
	elements, err := parseBoardElements([]byte(text))
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcInvalidArg
	}
	resp, err := patchBoard(id, elements)
	if err != nil {
		return boardErr(stderr, err)
	}
	reportPatch(stdout, id, resp, len(elements))
	return rcOK
}

func parseBoardElements(data []byte) ([]json.RawMessage, error) {
	var list []json.RawMessage
	if err := json.Unmarshal(data, &list); err == nil {
		return list, nil
	}
	var doc struct {
		Elements []json.RawMessage `json:"elements"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("elements must be a JSON array or an object with \"elements\": %v", err)
	}
	if doc.Elements == nil {
		return nil, errors.New("no \"elements\" in the file")
	}
	return doc.Elements, nil
}

// ---- rm / move / text: read, bump the version, patch ----

// editBoardElements reads the board's live elements, applies edit to each
// named one (as a generic JSON object), bumps its version and nonce, and
// patches the result.
func editBoardElements(id string, elemIDs []string, edit func(map[string]any), stdout, stderr io.Writer) int {
	doc, err := fetchBoard(id, -1)
	if err != nil {
		return boardErr(stderr, err)
	}
	byID := map[string]map[string]any{}
	for _, raw := range doc.Elements {
		var m map[string]any
		if json.Unmarshal(raw, &m) != nil {
			continue
		}
		if s, ok := m["id"].(string); ok {
			byID[s] = m
		}
	}
	out := make([]json.RawMessage, 0, len(elemIDs))
	for _, elemID := range elemIDs {
		m, ok := byID[elemID]
		if !ok {
			fmt.Fprintf(stderr, "Error: board %s has no live element %s\n", id, elemID)
			return rcNotFound
		}
		version, _ := m["version"].(float64)
		delete(m, "server_version")
		edit(m)
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

func boardRmCmd() *cobra.Command {
	return boa.CmdT[struct{}]{
		Use:         "rm <board> <element>...",
		Short:       "Delete elements from a board",
		Long:        "Writes a tombstone (isDeleted, version + 1) for each named element.",
		Args:        cobra.MinimumNArgs(2),
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(_ *struct{}, _ *cobra.Command, args []string) {
			os.Exit(editBoardElements(args[0], args[1:], func(m map[string]any) { m["isDeleted"] = true }, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

type boardMoveParams struct {
	X *float64 `long:"x" help:"New left edge"`
	Y *float64 `long:"y" help:"New top edge"`
}

func boardMoveCmd() *cobra.Command {
	return boa.CmdT[boardMoveParams]{
		Use:         "move <board> <element> [--x X] [--y Y]",
		Short:       "Move an element (its label and bound arrows follow)",
		Args:        cobra.ExactArgs(2),
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *boardMoveParams, _ *cobra.Command, args []string) {
			os.Exit(runBoardMove(p, args[0], args[1], os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runBoardMove(p *boardMoveParams, id, elemID string, stdout, stderr io.Writer) int {
	if p.X == nil && p.Y == nil {
		fmt.Fprintln(stderr, "Error: give --x and/or --y")
		return rcInvalidArg
	}
	doc, err := fetchBoard(id, -1)
	if err != nil {
		return boardErr(stderr, err)
	}
	s := loadBoardScene(doc)
	v, ok := s.view[elemID]
	if !ok {
		fmt.Fprintf(stderr, "Error: board %s has no live element %s\n", id, elemID)
		return rcNotFound
	}
	// The shape's label and the arrows bound to it move with it.
	d := [2]float64{floatOr(p.X, v.X) - v.X, floatOr(p.Y, v.Y) - v.Y}
	return patchScene(id, s, s.moveBy(map[string][2]float64{elemID: d}), stdout, stderr)
}

type boardTextParams struct {
	Text string `long:"text" help:"New text"`
}

func boardTextCmd() *cobra.Command {
	return boa.CmdT[boardTextParams]{
		Use:         "text <board> <element> --text <text>",
		Short:       "Change an element's text",
		Args:        cobra.ExactArgs(2),
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *boardTextParams, _ *cobra.Command, args []string) {
			os.Exit(editBoardElements(args[0], []string{args[1]}, func(m map[string]any) { m["text"] = p.Text }, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}
