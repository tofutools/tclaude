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

// `tclaude agent drawing-board` — collab drawing boards (tcl-4f86kp): a shared drawing board
// the operator edits in the dashboard and agents read and write here. Its
// elements are a subset of Excalidraw's element JSON. Reading needs
// drawing-boards.read; creating and changing drawing boards needs drawing-boards.write. A drawing board linked
// to a group is also its members' (groups.resources.read / .write).

// drawingBoardAskHuman is the drawing board commands' --ask-human, a persistent flag on
// `drawing board` so every subcommand takes it. It is checked as it is parsed.
var drawingBoardAskHuman askHumanValue

type askHumanValue struct {
	raw string
	d   time.Duration
}

func (v *askHumanValue) String() string { return v.raw }
func (v *askHumanValue) Type() string   { return "duration" }
func (v *askHumanValue) Set(s string) error {
	d, err := ParseAskHuman(s)
	if err != nil {
		return err
	}
	v.raw, v.d = s, d
	return nil
}

// drawingBoardOpts are the daemon options of a drawing board command's request.
func drawingBoardOpts() DaemonOpts { return DaemonOpts{AskHuman: drawingBoardAskHuman.d} }

func drawingBoardCmd() *cobra.Command {
	cmd := boa.CmdT[struct{}]{
		Use:   "drawing-board",
		Short: "Read and draw on shared collab drawing boards",
		Long: "Collab drawing boards are shared drawing boards: the operator draws in the dashboard's drawing board editor, and agents " +
			"read and change the same drawing board here. Elements are a subset of Excalidraw's element JSON " +
			"(rectangle, ellipse, diamond, arrow, line, freedraw, text), merged per element: a higher version wins, " +
			"and a deletion is a tombstone (isDeleted).\n\n" +
			"Reading (ls, show, snapshot) needs drawing-boards.read; create and every change need drawing-boards.write. " +
			"A drawing board linked to a group is its members' without either: membership confers groups.resources.read and " +
			"groups.resources.write in that group, and `drawing-board ls` then lists the drawing boards of your groups. " +
			"Pass --ask-human <timeout> to ask the operator by popup when a slug is missing. " +
			"Changing a drawing board makes you a participant, so the operator's drawing board activity reaches you as mail.\n\n" +
			"Typical loop: `drawing-board show <id>` to read it, `drawing-board snapshot <id> -o drawing-board.png` to see the operator's last " +
			"rendering, `drawing-board add` / `move` / `text` / `connect` / `rm` to draw, then `drawing-board show <id> --since <version>` for " +
			"what changed since.\n\n" +
			"Your first change starts a drawing board session, which keeps the drawing board in the operator's Background terminals " +
			"menu with no tab open, until `drawing-board close`. `drawing-board open` restarts one when you come back to a drawing board.",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds: []*cobra.Command{
			drawingBoardLsCmd(), drawingBoardShowCmd(), drawingBoardSnapshotCmd(), drawingBoardExportCmd(), drawingBoardCreateCmd(), drawingBoardOpenCmd(), drawingBoardCloseCmd(),
			drawingBoardAddCmd(), drawingBoardPatchCmd(), drawingBoardRmCmd(), drawingBoardMoveCmd(), drawingBoardTextCmd(), drawingBoardConnectCmd(), drawingBoardLayoutCmd(), drawingBoardFromProcessCmd(), drawingBoardFromFleetCmd(), drawingBoardFleetChangesCmd(),
		},
	}.ToCobra()
	cmd.PersistentFlags().Var(&drawingBoardAskHuman, "ask-human",
		"On permission denial, ask the human via popup with this timeout (e.g. '30s'). Capped at 300s. Timeout = deny.")
	_ = cmd.RegisterFlagCompletionFunc("ask-human", func(c *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return completeAskHumanDurations(c, args, toComplete), cobra.ShellCompDirectiveNoFileComp
	})
	return cmd
}

// ---- wire shapes ----

type drawingBoardSummary struct {
	ID           string    `json:"id"`
	Title        string    `json:"title"`
	Version      int64     `json:"version"`
	UpdatedAt    time.Time `json:"updated_at"`
	ElementCount int       `json:"element_count"`
	Participants []string  `json:"participants"`
	Group        string    `json:"group,omitempty"`
	AgentsLocked bool      `json:"agents_locked,omitempty"`
}

type drawingBoardDoc struct {
	ID              string                `json:"id"`
	Title           string                `json:"title"`
	Version         int64                 `json:"version"`
	UpdatedAt       time.Time             `json:"updated_at"`
	UpdatedBy       string                `json:"updated_by"`
	Participants    []string              `json:"participants"`
	SnapshotVersion *int64                `json:"snapshot_version,omitempty"`
	Elements        []json.RawMessage     `json:"elements"`
	Presence        *drawingBoardPresence `json:"presence,omitempty"`
	Group           string                `json:"group,omitempty"`
	GroupMembers    []string              `json:"group_members,omitempty"`
	AgentsLocked    bool                  `json:"agents_locked,omitempty"`
}

// drawingBoardPresence is who is on the drawing board right now (tcl-rawj2c).
type drawingBoardPresence struct {
	Agents []struct {
		AgentID string `json:"agent_id"`
		State   string `json:"state"`
	} `json:"agents"`
	OperatorView *struct {
		X          float64 `json:"x"`
		Y          float64 `json:"y"`
		Width      float64 `json:"width"`
		Height     float64 `json:"height"`
		AgeSeconds int     `json:"age_seconds"`
	} `json:"operator_view"`
}

// drawingBoardViewBinding is one attached end of a line: an element, and the snap
// point on it when one was chosen.
type drawingBoardViewBinding struct {
	ElementID  string    `json:"elementId"`
	FixedPoint []float64 `json:"fixedPoint"`
}

// drawingBoardElementView is the part of an element the CLI reads.
type drawingBoardElementView struct {
	ID           string                   `json:"id"`
	Type         string                   `json:"type"`
	X            float64                  `json:"x"`
	Y            float64                  `json:"y"`
	Width        float64                  `json:"width"`
	Height       float64                  `json:"height"`
	Text         string                   `json:"text"`
	ContainerID  string                   `json:"containerId"`
	Points       [][]float64              `json:"points"`
	Routing      string                   `json:"routing"`
	Version      int64                    `json:"version"`
	IsDeleted    bool                     `json:"isDeleted"`
	StartBinding *drawingBoardViewBinding `json:"startBinding"`
	EndBinding   *drawingBoardViewBinding `json:"endBinding"`
	CustomData   *struct {
		Author string `json:"author"`
	} `json:"customData"`
}

func drawingBoardPath(id string, tail ...string) string {
	p := "/v1/drawing-boards/" + url.PathEscape(id)
	for _, t := range tail {
		p += "/" + t
	}
	return p
}

// drawingBoardWithTombstones as fetchDrawingBoard's since returns deleted elements too. A
// redraw needs them: an element written back at or below its tombstone's
// version loses the merge and stays deleted.
const drawingBoardWithTombstones = 0

// peerDrawingBoardsPath is a linked node's drawing boards through its peer
// view: its own label or instance ID picks the node, and that node's grants
// to this one bound what comes back (an agent also needs drawing-boards.read
// held peer=<node>).
func peerDrawingBoardsPath(node string, tail ...string) string {
	p := "/v1/federation/peer/" + url.PathEscape(node) + "/drawing-boards"
	for _, t := range tail {
		p += "/" + t
	}
	return p
}

func fetchDrawingBoard(id string, since int64) (*drawingBoardDoc, error) {
	return fetchDrawingBoardOn("", id, since)
}

// fetchDrawingBoardOn reads a drawing board on this node, or with node set
// on that linked node.
func fetchDrawingBoardOn(node, id string, since int64) (*drawingBoardDoc, error) {
	path := drawingBoardPath(id)
	if node != "" {
		path = peerDrawingBoardsPath(node, url.PathEscape(id))
	}
	if since >= 0 {
		path += "?since=" + strconv.FormatInt(since, 10)
	}
	var doc drawingBoardDoc
	if err := DaemonRequest(http.MethodGet, path, nil, &doc, drawingBoardOpts()); err != nil {
		return nil, err
	}
	return &doc, nil
}

type drawingBoardPatchResp struct {
	Version  int64 `json:"version"`
	Accepted int   `json:"accepted"`
}

func patchDrawingBoard(id string, elements []json.RawMessage) (*drawingBoardPatchResp, error) {
	var resp drawingBoardPatchResp
	err := DaemonRequest(http.MethodPatch, drawingBoardPath(id), map[string]any{"elements": elements}, &resp, drawingBoardOpts())
	return &resp, err
}

func reportPatch(stdout io.Writer, id string, resp *drawingBoardPatchResp, sent int) {
	if resp.Accepted < sent {
		fmt.Fprintf(stdout, "Drawing board %s is at version %d: %d of %d element(s) accepted (the rest were older than the drawing board's copy).\n",
			id, resp.Version, resp.Accepted, sent)
		return
	}
	fmt.Fprintf(stdout, "Drawing board %s is at version %d (%d element(s) accepted).\n", id, resp.Version, resp.Accepted)
}

func drawingBoardErr(stderr io.Writer, err error) int {
	fmt.Fprintf(stderr, "Error: %v\n", err)
	return MapDaemonErrorToRC(err)
}

// drawingBoardRandomID returns a 20-character element id.
func drawingBoardRandomID() string {
	var b [10]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// drawingBoardRandomNonce returns a positive 31-bit nonce, as Excalidraw uses.
func drawingBoardRandomNonce() int64 {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return int64(binary.BigEndian.Uint32(b[:]) & 0x7fffffff)
}

// ---- ls ----

type drawingBoardLsParams struct {
	JSON bool   `long:"json" help:"Output JSON"`
	Node string `long:"node" optional:"true" help:"List a linked node's drawing boards, by its label or instance ID: those in the groups it grants this node groups.resources.read in"`
}

func drawingBoardLsCmd() *cobra.Command {
	return boa.CmdT[drawingBoardLsParams]{
		Use:         "ls",
		Short:       "List drawing boards",
		Args:        cobra.NoArgs,
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *drawingBoardLsParams, _ *cobra.Command, _ []string) {
			os.Exit(runDrawingBoardLs(p, cliStdout, cliStderr))
		},
	}.ToCobra()
}

func runDrawingBoardLs(p *drawingBoardLsParams, stdout, stderr io.Writer) int {
	var resp struct {
		DrawingBoards []drawingBoardSummary `json:"drawingBoards"`
	}
	path := "/v1/drawing-boards"
	if node := strings.TrimSpace(p.Node); node != "" {
		path = peerDrawingBoardsPath(node)
	}
	if err := DaemonRequest(http.MethodGet, path, nil, &resp, drawingBoardOpts()); err != nil {
		return drawingBoardErr(stderr, err)
	}
	if p.JSON {
		return writeDrawingBoardJSON(stdout, resp)
	}
	if len(resp.DrawingBoards) == 0 {
		fmt.Fprintln(stdout, "No drawing boards.")
		return rcOK
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tTITLE\tVERSION\tELEMENTS\tPARTICIPANTS\tGROUP\tLOCKED\tUPDATED")
	for _, b := range resp.DrawingBoards {
		group := b.Group
		if group == "" {
			group = "-"
		}
		locked := "-"
		if b.AgentsLocked {
			locked = "locked"
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%d\t%s\t%s\t%s\n", b.ID, b.Title, b.Version, b.ElementCount,
			len(b.Participants), group, locked, b.UpdatedAt.Local().Format("2006-01-02 15:04"))
	}
	_ = tw.Flush()
	return rcOK
}

func writeDrawingBoardJSON(stdout io.Writer, v any) int {
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
	return rcOK
}

// ---- show ----

type drawingBoardShowParams struct {
	Since *int64 `long:"since" help:"Only elements changed after this drawing board version, deletions included"`
	JSON  bool   `long:"json" help:"Output the drawing board JSON (elements in Excalidraw form)"`
	View  bool   `long:"view" help:"Only the elements in the part of the drawing board the operator has on screen"`
	Node  string `long:"node" optional:"true" help:"Show a linked node's drawing board, by its label or instance ID, under the grants it gave this node"`
}

func drawingBoardShowCmd() *cobra.Command {
	return boa.CmdT[drawingBoardShowParams]{
		Use:   "show <drawing board>",
		Short: "Show a drawing board's elements",
		Long: "Prints the drawing board version and one line per live element: type, id, rounded x,y,w,h, a text excerpt, " +
			"for arrows and lines their attached ends (element:side) and routing, and for attached text its container. With --since N only the elements changed after version N are " +
			"listed, deletions included. Remember the printed version and pass it as --since next time. " +
			"With --view only the elements the operator currently has on screen are listed (\"this\", \"here\"). " +
			"--node reads a linked node's drawing board under the grants that node gave this one (an agent also needs drawing-boards.read granted peer=<node>).",
		Args:        cobra.ExactArgs(1),
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *drawingBoardShowParams, _ *cobra.Command, args []string) {
			os.Exit(runDrawingBoardShow(p, args[0], cliStdout, cliStderr))
		},
	}.ToCobra()
}

func runDrawingBoardShow(p *drawingBoardShowParams, id string, stdout, stderr io.Writer) int {
	since := int64(-1)
	if p.Since != nil {
		if *p.Since < 0 {
			fmt.Fprintln(stderr, "Error: --since must be >= 0")
			return rcInvalidArg
		}
		since = *p.Since
	}
	if p.View && since >= 0 {
		fmt.Fprintln(stderr, "Error: --view lists the whole operator view; it does not combine with --since")
		return rcInvalidArg
	}
	node := strings.TrimSpace(p.Node)
	if p.View && node != "" {
		fmt.Fprintln(stderr, "Error: --view is this node's operator's view; a linked node does not share its operator's")
		return rcInvalidArg
	}
	doc, err := fetchDrawingBoardOn(node, id, since)
	if err != nil {
		return drawingBoardErr(stderr, err)
	}
	if p.View && p.JSON {
		v := doc.Presence.view()
		if v == nil {
			fmt.Fprintln(stderr, "Error: the operator does not have this drawing board open right now; drop --view")
			return rcNotFound
		}
		doc.Elements = elementsInView(doc.Elements, *v)
		return writeDrawingBoardJSON(stdout, doc)
	}
	if p.JSON {
		return writeDrawingBoardJSON(stdout, doc)
	}
	fmt.Fprintf(stdout, "Drawing board %s %q: version %d", doc.ID, doc.Title, doc.Version)
	if doc.UpdatedBy != "" {
		fmt.Fprintf(stdout, ", last changed by %s", doc.UpdatedBy)
	}
	fmt.Fprintln(stdout)
	if doc.AgentsLocked {
		fmt.Fprintln(stdout, "Locked: agents can read it but not change it until it is unlocked.")
	}
	if len(doc.Participants) > 0 {
		fmt.Fprintf(stdout, "Participants: %s\n", strings.Join(doc.Participants, ", "))
	}
	if doc.Group != "" {
		fmt.Fprintf(stdout, "Group: %s (its %d member(s) get the drawing board's events)\n", doc.Group, len(doc.GroupMembers))
	}
	var view *drawingBoardBox
	if pr := doc.Presence; pr != nil {
		var active []string
		for _, a := range pr.Agents {
			active = append(active, a.AgentID+" "+a.State)
		}
		if len(active) > 0 {
			fmt.Fprintf(stdout, "Active now: %s\n", strings.Join(active, ", "))
		}
		if v := pr.OperatorView; v != nil {
			view = &drawingBoardBox{X: v.X, Y: v.Y, W: v.Width, H: v.Height}
			fmt.Fprintf(stdout, "The operator is looking at x=%d..%d y=%d..%d (as of %ds ago).\n",
				int64(math.Round(v.X)), int64(math.Round(v.X+v.Width)), int64(math.Round(v.Y)), int64(math.Round(v.Y+v.Height)), v.AgeSeconds)
		}
	}
	if p.View {
		if view == nil {
			fmt.Fprintln(stdout, "The operator does not have this drawing board open right now; drop --view to list everything.")
			return rcOK
		}
		kept := elementsInView(doc.Elements, *view)
		fmt.Fprintf(stdout, "%d of %d element(s) are in the operator's view:\n", len(kept), len(doc.Elements))
		doc.Elements = nil
		for _, raw := range kept {
			var e drawingBoardElementView
			if json.Unmarshal(raw, &e) == nil {
				fmt.Fprintln(stdout, "  "+formatDrawingBoardElement(e))
			}
		}
		return rcOK
	}
	if since >= 0 {
		fmt.Fprintf(stdout, "%d element(s) changed since version %d:\n", len(doc.Elements), since)
	} else {
		fmt.Fprintf(stdout, "%d element(s):\n", len(doc.Elements))
	}
	for _, raw := range doc.Elements {
		var e drawingBoardElementView
		if err := json.Unmarshal(raw, &e); err != nil {
			continue
		}
		fmt.Fprintln(stdout, "  "+formatDrawingBoardElement(e))
	}
	return rcOK
}

type drawingBoardBox struct{ X, Y, W, H float64 }

// view is the operator's view of the drawing board, or nil.
func (p *drawingBoardPresence) view() *drawingBoardBox {
	if p == nil || p.OperatorView == nil {
		return nil
	}
	v := p.OperatorView
	return &drawingBoardBox{X: v.X, Y: v.Y, W: v.Width, H: v.Height}
}

func elementsInView(elems []json.RawMessage, view drawingBoardBox) []json.RawMessage {
	kept := []json.RawMessage{}
	for _, raw := range elems {
		var e drawingBoardElementView
		if json.Unmarshal(raw, &e) == nil && !e.IsDeleted && view.overlaps(elementBox(e)) {
			kept = append(kept, raw)
		}
	}
	return kept
}

func (b drawingBoardBox) overlaps(o drawingBoardBox) bool {
	return o.X <= b.X+b.W && o.X+o.W >= b.X && o.Y <= b.Y+b.H && o.Y+o.H >= b.Y
}

// elementBox is the area an element covers; arrows, lines and strokes by
// their points.
func elementBox(e drawingBoardElementView) drawingBoardBox {
	if len(e.Points) == 0 {
		return drawingBoardBox{X: e.X, Y: e.Y, W: e.Width, H: e.Height}
	}
	minX, minY, maxX, maxY := math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)
	for _, pt := range e.Points {
		if len(pt) < 2 {
			continue
		}
		minX, maxX = math.Min(minX, pt[0]), math.Max(maxX, pt[0])
		minY, maxY = math.Min(minY, pt[1]), math.Max(maxY, pt[1])
	}
	if math.IsInf(minX, 1) {
		return drawingBoardBox{X: e.X, Y: e.Y}
	}
	return drawingBoardBox{X: e.X + minX, Y: e.Y + minY, W: maxX - minX, H: maxY - minY}
}

func formatDrawingBoardElement(e drawingBoardElementView) string {
	if e.IsDeleted {
		return fmt.Sprintf("%-9s %s  (deleted)", e.Type, e.ID)
	}
	line := fmt.Sprintf("%-9s %s  x=%d y=%d w=%d h=%d", e.Type, e.ID,
		int64(math.Round(e.X)), int64(math.Round(e.Y)), int64(math.Round(e.Width)), int64(math.Round(e.Height)))
	if e.StartBinding != nil || e.EndBinding != nil {
		line += fmt.Sprintf("  %s -> %s", describeDrawingBoardEnd(e.StartBinding), describeDrawingBoardEnd(e.EndBinding))
	}
	if e.Routing != "" && e.Routing != "straight" {
		line += "  " + e.Routing
	}
	if e.ContainerID != "" {
		line += "  on " + e.ContainerID
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

type drawingBoardSnapshotParams struct {
	Output string `long:"output" short:"o" help:"File to write the PNG to"`
	Region int    `long:"region" optional:"true" help:"Save the image of one operator change instead, as numbered in its drawing board event mail"`
}

func drawingBoardSnapshotCmd() *cobra.Command {
	return boa.CmdT[drawingBoardSnapshotParams]{
		Use:   "snapshot <drawing board>",
		Short: "Save the drawing board's latest PNG snapshot",
		Long: "Writes the latest PNG rendering of the drawing board, as the operator's dashboard last saved it, and says " +
			"how far it lags the drawing board's current version. Read it with an image-capable tool to see the layout. " +
			"With --region N it saves the cropped image of the change a drawing board event mail named instead.",
		Args:        cobra.ExactArgs(1),
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *drawingBoardSnapshotParams, _ *cobra.Command, args []string) {
			os.Exit(runDrawingBoardSnapshot(p, args[0], cliStdout, cliStderr))
		},
	}.ToCobra()
}

func runDrawingBoardSnapshot(p *drawingBoardSnapshotParams, id string, stdout, stderr io.Writer) int {
	if strings.TrimSpace(p.Output) == "" {
		fmt.Fprintln(stderr, "Error: -o <file.png> is required")
		return rcInvalidArg
	}
	if p.Region < 0 {
		fmt.Fprintln(stderr, "Error: --region must be a positive number from a drawing board event")
		return rcInvalidArg
	}
	path := drawingBoardPath(id, "snapshot")
	if p.Region > 0 {
		path += "?region=" + strconv.Itoa(p.Region)
	}
	data, hdr, err := DaemonGetRawWithOptions(path, drawingBoardOpts())
	if err != nil {
		return drawingBoardErr(stderr, err)
	}
	if err := os.WriteFile(p.Output, data, 0o600); err != nil {
		fmt.Fprintf(stderr, "Error: write %s: %v\n", p.Output, err)
		return rcIOFailure
	}
	if p.Region > 0 {
		fmt.Fprintf(stdout, "Wrote %s (%d bytes): the region the operator changed in event image %d. "+
			"Use `drawing-board show` for the drawing board's current elements.\n", p.Output, len(data), p.Region)
		return rcOK
	}
	fmt.Fprintf(stdout, "Wrote %s (%d bytes). %s\n", p.Output, len(data),
		snapshotAge(hdr.Get("X-Drawing-board-Snapshot-Version"), hdr.Get("X-Drawing-board-Version")))
	return rcOK
}

func snapshotAge(snapHeader, drawingBoardHeader string) string {
	snap, err1 := strconv.ParseInt(snapHeader, 10, 64)
	drawingBoard, err2 := strconv.ParseInt(drawingBoardHeader, 10, 64)
	switch {
	case err1 != nil:
		return "The snapshot's drawing board version is unknown."
	case err2 != nil:
		return fmt.Sprintf("It shows drawing board version %d.", snap)
	case snap >= drawingBoard:
		return fmt.Sprintf("It shows the current drawing board version (%d).", drawingBoard)
	default:
		return fmt.Sprintf("It shows drawing board version %d; the drawing board is now at version %d (%d version(s) newer), "+
			"so use `drawing-board show` for the latest elements.", snap, drawingBoard, drawingBoard-snap)
	}
}

// ---- create ----

type drawingBoardCreateParams struct {
	Title string `long:"title" optional:"true" help:"Drawing board title (defaults to the id)"`
	Group string `long:"group" optional:"true" help:"Make it the drawing board of a group you are in: every member gets its events"`
}

func drawingBoardCreateCmd() *cobra.Command {
	return boa.CmdT[drawingBoardCreateParams]{
		Use:   "create <drawing board>",
		Short: "Create a drawing board",
		Long: "Creates an empty drawing board. The id is lowercase letters, digits and dashes (at most 64). " +
			"With --group the drawing board belongs to that group (one you are a member of): its members are on the drawing board " +
			"without an invitation and get the operator's drawing board events.",
		Args:        cobra.ExactArgs(1),
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *drawingBoardCreateParams, _ *cobra.Command, args []string) {
			os.Exit(runDrawingBoardCreate(p, args[0], cliStdout, cliStderr))
		},
	}.ToCobra()
}

func runDrawingBoardCreate(p *drawingBoardCreateParams, id string, stdout, stderr io.Writer) int {
	var doc drawingBoardDoc
	if err := DaemonRequest(http.MethodPost, "/v1/drawing-boards", map[string]string{"id": id, "title": p.Title, "group": p.Group}, &doc, drawingBoardOpts()); err != nil {
		return drawingBoardErr(stderr, err)
	}
	if doc.Group != "" {
		fmt.Fprintf(stdout, "Created drawing board %s %q for group %s.\n", doc.ID, doc.Title, doc.Group)
	} else {
		fmt.Fprintf(stdout, "Created drawing board %s %q.\n", doc.ID, doc.Title)
	}
	return rcOK
}

// ---- open, close ----

func drawingBoardOpenCmd() *cobra.Command {
	return boa.CmdT[struct{}]{
		Use:   "open <drawing board>",
		Short: "Start your drawing board session again",
		Long: "A drawing board session is a durable record that you are working on a drawing board, like a tmux session: it needs no " +
			"tab open, survives reincarnation and daemon restarts, and has no time limit. While you have one, the " +
			"operator can resume the drawing board from the dashboard's Background terminals menu. Changing the drawing board starts " +
			"one too, so this is for coming back to a drawing board later. It also makes you a participant. Needs drawing-boards.write.\n\n" +
			"A session ends with `drawing-board close`, when you retire, when the drawing board is deleted, or when the operator ends it. " +
			"`drawing-board show` works without a dashboard; the PNG of `drawing-board snapshot` does not, because an open dashboard renders it.",
		Args:        cobra.ExactArgs(1),
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(_ *struct{}, _ *cobra.Command, args []string) {
			os.Exit(runDrawingBoardOpen(args[0], cliStdout, cliStderr))
		},
	}.ToCobra()
}

func runDrawingBoardOpen(id string, stdout, stderr io.Writer) int {
	var doc drawingBoardDoc
	if err := DaemonRequest(http.MethodPost, drawingBoardPath(id, "session"), nil, &doc, drawingBoardOpts()); err != nil {
		return drawingBoardErr(stderr, err)
	}
	fmt.Fprintf(stdout, "Drawing board session open on %s %q, at version %d. It lasts until `drawing-board close`, with or without a tab open.\n",
		doc.ID, doc.Title, doc.Version)
	fmt.Fprintln(stdout, "`drawing-board show` needs no dashboard, but `drawing-board snapshot` only has what an open dashboard last rendered.")
	return rcOK
}

func drawingBoardCloseCmd() *cobra.Command {
	return boa.CmdT[struct{}]{
		Use:   "close <drawing board>",
		Short: "End your drawing board session",
		Long: "Ends your own drawing board session, so the drawing board leaves the operator's Background terminals menu unless another " +
			"agent has one. The drawing board stays, you stay a participant, and your next change starts a new session. " +
			"Needs drawing-boards.write.",
		Args:        cobra.ExactArgs(1),
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(_ *struct{}, _ *cobra.Command, args []string) {
			os.Exit(runDrawingBoardClose(args[0], cliStdout, cliStderr))
		},
	}.ToCobra()
}

func runDrawingBoardClose(id string, stdout, stderr io.Writer) int {
	if err := DaemonRequest(http.MethodDelete, drawingBoardPath(id, "session"), nil, nil, drawingBoardOpts()); err != nil {
		return drawingBoardErr(stderr, err)
	}
	fmt.Fprintf(stdout, "Drawing board session on %s closed.\n", id)
	return rcOK
}

// ---- add ----

type drawingBoardAddParams struct {
	X     *float64 `long:"x" help:"Left edge (default 0)"`
	Y     *float64 `long:"y" help:"Top edge (default 0)"`
	W     *float64 `long:"w" help:"Width (default 160 for shapes; arrows and lines: the x extent)"`
	H     *float64 `long:"h" help:"Height (default 80 for shapes; arrows and lines: the y extent)"`
	Text  string   `long:"text" optional:"true" help:"Text (required for text elements); on a shape, arrow or line it attaches centred text"`
	Color string   `long:"color" optional:"true" help:"Stroke color: #rrggbb, #rgb or transparent (default #1e1e1e)"`
	Bg    string   `long:"bg" optional:"true" help:"Fill color: #rrggbb, #rgb or transparent (default transparent)"`
	From  string   `long:"from" optional:"true" help:"Arrow or line: element id to start from (points are computed between the element centers)"`
	To    string   `long:"to" optional:"true" help:"Arrow or line: element id to end at"`
	ID    string   `long:"id" optional:"true" help:"Element id (default: generated)"`

	FromSide string `long:"from-side" optional:"true" help:"With --from: snap point to attach to: top, right, bottom or left (default: wherever the line meets it)"`
	ToSide   string `long:"to-side" optional:"true" help:"With --to: snap point to attach to: top, right, bottom or left"`
	Routing  string `long:"routing" optional:"true" help:"Arrow or line: straight (default), elbow or curved; with --link, the linking arrow"`

	RightOf string   `long:"right-of" optional:"true" help:"Place the shape to the right of this element id (centred on it), instead of --x/--y"`
	LeftOf  string   `long:"left-of" optional:"true" help:"Place the shape to the left of this element id"`
	Below   string   `long:"below" optional:"true" help:"Place the shape below this element id"`
	Above   string   `long:"above" optional:"true" help:"Place the shape above this element id"`
	Gap     *float64 `long:"gap" help:"Space to the --right-of/--left-of/--below/--above element (default 60)"`
	Link    bool     `long:"link" optional:"true" help:"With a relative placement: also draw an arrow from that element to the new shape"`

	FontSize *float64 `long:"font-size" help:"Text size, 4-200 (default 20)"`
	Font     string   `long:"font" optional:"true" help:"Text font: hand (default), sans or mono"`
	Style    string   `long:"style" optional:"true" help:"Text style: normal (default), bold, italic or bold-italic"`
}

var drawingBoardAddTypes = map[string]bool{"rectangle": true, "ellipse": true, "diamond": true, "text": true, "arrow": true, "line": true}

func drawingBoardAddCmd() *cobra.Command {
	return boa.CmdT[drawingBoardAddParams]{
		Use:   "add <drawing board> <rectangle|ellipse|diamond|text|arrow|line>",
		Short: "Add an element to a drawing board",
		Long: "Adds one element and prints its id. Shapes take --x --y --w --h; text takes --text (and --x --y); " +
			"--text on a shape, arrow or line attaches centred text, a second element that stays on it; " +
			"an arrow or line either connects two elements (--from and --to, by element id, with --from-side and --to-side " +
			"for a snap point) or spans --x --y by --w --h, and --routing picks straight, elbow or curved.\n\n" +
			"Place a shape relative to another with --right-of, --left-of, --below or --above <id> (centred on it, --gap " +
			"apart); --link also draws an arrow from that element to the new one, so a flow is built step by step.\n\n" +
			"Example: tclaude agent drawing-board add plan rectangle --x 100 --y 80 --text 'API' --id api\n" +
			"         tclaude agent drawing-board add plan rectangle --right-of api --link --text 'DB' --id db\n" +
			"         tclaude agent drawing-board add plan arrow --from <idA> --to <idB>\n" +
			"         tclaude agent drawing-board add plan arrow --from <idA> --from-side right --to <idB> --to-side left --routing elbow",
		Args:        cobra.ExactArgs(2),
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *drawingBoardAddParams, _ *cobra.Command, args []string) {
			os.Exit(runDrawingBoardAdd(p, args[0], args[1], cliStdout, cliStderr))
		},
	}.ToCobra()
}

func floatOr(v *float64, def float64) float64 {
	if v == nil {
		return def
	}
	return *v
}

func runDrawingBoardAdd(p *drawingBoardAddParams, id, typ string, stdout, stderr io.Writer) int {
	if !drawingBoardAddTypes[typ] {
		fmt.Fprintln(stderr, "Error: type must be one of rectangle, ellipse, diamond, text, arrow, line")
		return rcInvalidArg
	}
	if typ == "text" && strings.TrimSpace(p.Text) == "" {
		fmt.Fprintln(stderr, "Error: a text element needs --text")
		return rcInvalidArg
	}
	style := drawingBoardTextStyle{Size: p.FontSize, Font: p.Font, Style: p.Style}
	if err := style.validate(); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcInvalidArg
	}
	if style.set() && typ != "text" && p.Text == "" {
		fmt.Fprintln(stderr, "Error: --font-size, --font and --style need --text")
		return rcInvalidArg
	}
	isLine := typ == "arrow" || typ == "line"
	if (p.From != "" || p.To != "") && !isLine {
		fmt.Fprintln(stderr, "Error: --from and --to only apply to arrows and lines")
		return rcInvalidArg
	}
	if (p.From == "") != (p.To == "") {
		fmt.Fprintln(stderr, "Error: give both --from and --to")
		return rcInvalidArg
	}
	if (p.FromSide != "" && p.From == "") || (p.ToSide != "" && p.To == "") {
		fmt.Fprintln(stderr, "Error: --from-side needs --from, and --to-side needs --to")
		return rcInvalidArg
	}
	if p.Routing != "" && !isLine && !p.Link {
		fmt.Fprintln(stderr, "Error: --routing only applies to arrows and lines, or with --link")
		return rcInvalidArg
	}
	for _, err := range []error{checkDrawingBoardRouting(p.Routing), checkDrawingBoardSide("--from-side", p.FromSide), checkDrawingBoardSide("--to-side", p.ToSide)} {
		if err != nil {
			fmt.Fprintf(stderr, "Error: %v\n", err)
			return rcInvalidArg
		}
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
	var ref drawingBoardElementView
	if relRef != "" {
		doc, err := fetchDrawingBoard(id, -1)
		if err != nil {
			return drawingBoardErr(stderr, err)
		}
		v, ok := loadDrawingBoardScene(doc).view[relRef]
		if !ok {
			fmt.Fprintf(stderr, "Error: drawing board %s has no live element %s\n", id, relRef)
			return rcNotFound
		}
		ref = v
	}
	elemID := p.ID
	if elemID == "" {
		elemID = drawingBoardRandomID()
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
		"id": elemID, "type": typ, "version": 1, "versionNonce": drawingBoardRandomNonce(), "isDeleted": false,
		"updated": time.Now().UnixMilli(), "strokeColor": stroke, "backgroundColor": bg,
		"fillStyle": "solid", "strokeWidth": 2, "strokeStyle": "solid", "roughness": 1, "opacity": 100, "angle": 0,
	}
	x, y := floatOr(p.X, 0), floatOr(p.Y, 0)
	if relRef != "" {
		w, h := floatOr(p.W, 160), floatOr(p.H, 80)
		if typ == "text" {
			tw, th := drawingBoardTextSizeAt(p.Text, style.size())
			w, h = floatOr(p.W, tw), floatOr(p.H, th)
		}
		x, y = placeRelative(ref, w, h, relDir, floatOr(p.Gap, drawingBoardDefaultGap))
	}
	if isLine && p.Routing != "" {
		e["routing"] = p.Routing
	}
	// Attached text is its own text element naming its container, the way the
	// drawing board editor reads and draws it.
	var labelX, labelY float64
	switch typ {
	case "text":
		w, h := drawingBoardTextSizeAt(p.Text, style.size())
		e["text"], e["fontSize"], e["textAlign"] = p.Text, style.size(), "left"
		style.apply(e)
		e["x"], e["y"] = x, y
		e["width"], e["height"] = floatOr(p.W, w), floatOr(p.H, h)
	case "arrow", "line":
		line := drawingBoardElementView{ID: elemID, Type: typ, X: x, Y: y, Routing: p.Routing}
		scene := &drawingBoardScene{raw: map[string]map[string]any{elemID: e}, view: map[string]drawingBoardElementView{}}
		if p.From != "" {
			doc, err := fetchDrawingBoard(id, -1)
			if err != nil {
				return drawingBoardErr(stderr, err)
			}
			if _, _, ok := findDrawingBoardElements(doc, p.From, p.To); !ok {
				fmt.Fprintf(stderr, "Error: drawing board %s has no live element %s or %s\n", id, p.From, p.To)
				return rcNotFound
			}
			scene = loadDrawingBoardScene(doc)
			scene.raw[elemID] = e
			e["startBinding"], line.StartBinding = drawingBoardBindingFor(p.From, p.FromSide)
			e["endBinding"], line.EndBinding = drawingBoardBindingFor(p.To, p.ToSide)
		} else {
			line.Points = [][]float64{{0, 0}, {floatOr(p.W, 160), floatOr(p.H, 0)}}
		}
		scene.view[elemID] = line
		scene.setPath(elemID, scene.routeLine(line))
		labelX, labelY = labelPoint(scene.view[elemID])
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
			labelID = drawingBoardRandomID()
		}
		w, h := drawingBoardTextSizeAt(p.Text, style.size())
		label := map[string]any{
			"id": labelID, "type": "text", "version": 1, "versionNonce": drawingBoardRandomNonce(), "isDeleted": false,
			"updated": e["updated"], "strokeColor": stroke, "backgroundColor": "transparent", "opacity": 100, "angle": 0,
			"text": p.Text, "fontSize": style.size(), "textAlign": "center", "containerId": elemID,
			"x": labelX - w/2, "y": labelY - h/2, "width": w, "height": h,
		}
		style.apply(label)
		elems = append(elems, label)
	}
	linkID := ""
	if p.Link {
		linkID = drawingBoardRandomID()
		self := drawingBoardElementView{ID: elemID, Type: typ, X: e["x"].(float64), Y: e["y"].(float64)}
		self.Width, _ = e["width"].(float64)
		self.Height, _ = e["height"].(float64)
		link := map[string]any{
			"id": linkID, "type": "arrow", "version": 1, "versionNonce": drawingBoardRandomNonce(), "isDeleted": false,
			"updated": e["updated"], "strokeColor": stroke, "backgroundColor": "transparent",
			"fillStyle": "solid", "strokeWidth": 2, "strokeStyle": "solid", "roughness": 1, "opacity": 100, "angle": 0,
			"startBinding": map[string]string{"elementId": relRef}, "endBinding": map[string]string{"elementId": elemID},
		}
		if p.Routing != "" {
			link["routing"] = p.Routing
		}
		scene := &drawingBoardScene{raw: map[string]map[string]any{linkID: link}, view: map[string]drawingBoardElementView{relRef: ref, elemID: self}}
		arrow := drawingBoardElementView{ID: linkID, Type: "arrow", Routing: p.Routing,
			StartBinding: &drawingBoardViewBinding{ElementID: relRef}, EndBinding: &drawingBoardViewBinding{ElementID: elemID}}
		scene.view[linkID] = arrow
		scene.setPath(linkID, scene.routeLine(arrow))
		elems = append(elems, link)
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
	resp, err := patchDrawingBoard(id, raws)
	if err != nil {
		return drawingBoardErr(stderr, err)
	}
	extra := ""
	if labelID != "" {
		extra += " with label " + labelID
	}
	if linkID != "" {
		extra += fmt.Sprintf(" and arrow %s from %s", linkID, relRef)
	}
	if extra != "" {
		fmt.Fprintf(stdout, "Added %s %s%s to drawing board %s (version %d).\n", typ, elemID, extra, id, resp.Version)
		return rcOK
	}
	fmt.Fprintf(stdout, "Added %s %s to drawing board %s (version %d).\n", typ, elemID, id, resp.Version)
	return rcOK
}

const drawingBoardFontSize = 20.0

// drawingBoardTextSize estimates a text element's box at drawingBoardFontSize.
func drawingBoardTextSize(text string) (w, h float64) {
	return drawingBoardTextSizeAt(text, drawingBoardFontSize)
}

func drawingBoardTextSizeAt(text string, size float64) (w, h float64) {
	lines := strings.Split(text, "\n")
	widest := 0
	for _, l := range lines {
		widest = max(widest, len([]rune(l)))
	}
	return math.Ceil(float64(widest) * size * 0.6), math.Ceil(float64(len(lines)) * size * 1.25)
}

// drawingBoardFonts names Excalidraw's fontFamily numbers, which the drawing board stores.
var drawingBoardFonts = map[string]int{"hand": 1, "sans": 2, "mono": 3}

// drawingBoardTextStyle is the --font-size, --font and --style of a command; what was
// not given leaves an element as it is.
type drawingBoardTextStyle struct {
	Size  *float64
	Font  string
	Style string
}

func (s drawingBoardTextStyle) set() bool { return s.Size != nil || s.Font != "" || s.Style != "" }

func (s drawingBoardTextStyle) size() float64 { return floatOr(s.Size, drawingBoardFontSize) }

func (s drawingBoardTextStyle) validate() error {
	if s.Size != nil && (*s.Size < 4 || *s.Size > 200) {
		return errors.New("--font-size must be 4-200")
	}
	if _, ok := drawingBoardFonts[s.Font]; s.Font != "" && !ok {
		return errors.New("--font must be hand, sans or mono")
	}
	switch s.Style {
	case "", "normal", "bold", "italic", "bold-italic":
		return nil
	}
	return errors.New("--style must be normal, bold, italic or bold-italic")
}

func (s drawingBoardTextStyle) apply(e map[string]any) {
	if s.Size != nil {
		e["fontSize"] = *s.Size
	}
	if s.Font != "" {
		e["fontFamily"] = drawingBoardFonts[s.Font]
	}
	if s.Style == "" {
		return
	}
	delete(e, "fontWeight")
	delete(e, "fontStyle")
	if strings.Contains(s.Style, "bold") {
		e["fontWeight"] = "bold"
	}
	if strings.Contains(s.Style, "italic") {
		e["fontStyle"] = "italic"
	}
}

func findDrawingBoardElements(doc *drawingBoardDoc, fromID, toID string) (from, to drawingBoardElementView, ok bool) {
	var haveFrom, haveTo bool
	for _, raw := range doc.Elements {
		var e drawingBoardElementView
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
func arrowBetween(from, to drawingBoardElementView) (sx, sy, ex, ey float64) {
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

type drawingBoardPatchParams struct {
	File string `long:"file" help:"JSON file of elements (- reads stdin): an array, {\"elements\": [...]}, or an .excalidraw file"`
}

func drawingBoardPatchCmd() *cobra.Command {
	return boa.CmdT[drawingBoardPatchParams]{
		Use:   "patch <drawing board> --file <elements.json>",
		Short: "Apply Excalidraw elements to a drawing board",
		Long: "Sends raw Excalidraw elements. Each merges on its own: it replaces the drawing board's copy when its version " +
			"is higher (a tie goes to the lower versionNonce). To change an element, send it with version + 1; " +
			"to delete it, also set isDeleted. Unknown keys are dropped; customData.author is set to you.",
		Args:        cobra.ExactArgs(1),
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *drawingBoardPatchParams, _ *cobra.Command, args []string) {
			os.Exit(runDrawingBoardPatch(p, args[0], os.Stdin, cliStdout, cliStderr))
		},
	}.ToCobra()
}

func runDrawingBoardPatch(p *drawingBoardPatchParams, id string, stdin io.Reader, stdout, stderr io.Writer) int {
	if p.File == "" {
		fmt.Fprintln(stderr, "Error: --file is required (- reads stdin)")
		return rcInvalidArg
	}
	text, rc := resolveBodyInput("", p.File, "", stdin, stderr)
	if rc != rcOK {
		return rc
	}
	elements, err := parseDrawingBoardElements([]byte(text))
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcInvalidArg
	}
	resp, err := patchDrawingBoard(id, elements)
	if err != nil {
		return drawingBoardErr(stderr, err)
	}
	reportPatch(stdout, id, resp, len(elements))
	return rcOK
}

func parseDrawingBoardElements(data []byte) ([]json.RawMessage, error) {
	var list []json.RawMessage
	// A bare `null` decodes without error into a nil list; it holds no elements.
	if err := json.Unmarshal(data, &list); err == nil && list != nil {
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

func drawingBoardRmCmd() *cobra.Command {
	return boa.CmdT[struct{}]{
		Use:   "rm <drawing board> <element>...",
		Short: "Delete elements from a drawing board",
		Long: "Writes a tombstone (isDeleted, version + 1) for each named element and its attached text (containerId or " +
			"`<id>-label`); other text inside a shape stays. " +
			"An arrow or line attached to a deleted element stays where it is, with that end free.",
		Args:        cobra.MinimumNArgs(2),
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(_ *struct{}, _ *cobra.Command, args []string) {
			os.Exit(runDrawingBoardRm(args[0], args[1:], cliStdout, cliStderr))
		},
	}.ToCobra()
}

func runDrawingBoardRm(id string, elemIDs []string, stdout, stderr io.Writer) int {
	doc, err := fetchDrawingBoard(id, -1)
	if err != nil {
		return drawingBoardErr(stderr, err)
	}
	s := loadDrawingBoardScene(doc)
	gone := map[string]bool{}
	for _, elemID := range elemIDs {
		if _, ok := s.view[elemID]; !ok {
			fmt.Fprintf(stderr, "Error: drawing board %s has no live element %s\n", id, elemID)
			return rcNotFound
		}
		gone[elemID] = true
	}
	// Only text named as attached: there is no undo here to bring back a note
	// that merely lay inside a deleted box.
	for _, textID := range s.order {
		v := s.view[textID]
		owner, labelled := strings.CutSuffix(textID, "-label")
		if v.Type == "text" && (gone[v.ContainerID] || (labelled && v.ContainerID == "" && gone[owner])) {
			gone[textID] = true
		}
	}
	var changed []string
	for _, elemID := range s.order {
		v := s.view[elemID]
		if gone[elemID] {
			s.raw[elemID]["isDeleted"] = true
			changed = append(changed, elemID)
			continue
		}
		start := v.StartBinding != nil && gone[v.StartBinding.ElementID]
		end := v.EndBinding != nil && gone[v.EndBinding.ElementID]
		if !start && !end {
			continue
		}
		// The line stays as it is drawn now, free at that end.
		if v.Type != "freedraw" && !handBent(v) {
			s.setPath(elemID, s.routeLine(v))
		}
		if start {
			delete(s.raw[elemID], "startBinding")
		}
		if end {
			delete(s.raw[elemID], "endBinding")
		}
		changed = append(changed, elemID)
	}
	return patchScene(id, s, changed, stdout, stderr)
}

type drawingBoardMoveParams struct {
	X *float64 `long:"x" help:"New left edge"`
	Y *float64 `long:"y" help:"New top edge"`
}

func drawingBoardMoveCmd() *cobra.Command {
	return boa.CmdT[drawingBoardMoveParams]{
		Use:         "move <drawing board> <element> [--x X] [--y Y]",
		Short:       "Move an element (its text and attached lines follow); a moved line detaches",
		Args:        cobra.ExactArgs(2),
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *drawingBoardMoveParams, _ *cobra.Command, args []string) {
			os.Exit(runDrawingBoardMove(p, args[0], args[1], cliStdout, cliStderr))
		},
	}.ToCobra()
}

func runDrawingBoardMove(p *drawingBoardMoveParams, id, elemID string, stdout, stderr io.Writer) int {
	if p.X == nil && p.Y == nil {
		fmt.Fprintln(stderr, "Error: give --x and/or --y")
		return rcInvalidArg
	}
	doc, err := fetchDrawingBoard(id, -1)
	if err != nil {
		return drawingBoardErr(stderr, err)
	}
	s := loadDrawingBoardScene(doc)
	v, ok := s.view[elemID]
	if !ok {
		fmt.Fprintf(stderr, "Error: drawing board %s has no live element %s\n", id, elemID)
		return rcNotFound
	}
	if v.ContainerID != "" {
		if _, attached := s.view[v.ContainerID]; attached {
			fmt.Fprintf(stderr, "Error: %s is attached to %s and sits on it; move %s instead\n", elemID, v.ContainerID, v.ContainerID)
			return rcInvalidArg
		}
	}
	// A line moved away is pulled off what it was attached to, as a drag in the
	// dashboard does: an attached end stays on its shape otherwise.
	if isDrawingBoardLine(v.Type) {
		v.StartBinding, v.EndBinding = nil, nil
		s.view[elemID] = v
		delete(s.raw[elemID], "startBinding")
		delete(s.raw[elemID], "endBinding")
	}
	// The shape's label and the arrows bound to it move with it.
	d := [2]float64{floatOr(p.X, v.X) - v.X, floatOr(p.Y, v.Y) - v.Y}
	return patchScene(id, s, s.moveBy(map[string][2]float64{elemID: d}), stdout, stderr)
}

type drawingBoardTextParams struct {
	Text     string   `long:"text" optional:"true" help:"New text; without it the text stays and only its style changes"`
	FontSize *float64 `long:"font-size" help:"Text size, 4-200"`
	Font     string   `long:"font" optional:"true" help:"Text font: hand, sans or mono"`
	Style    string   `long:"style" optional:"true" help:"Text style: normal, bold, italic or bold-italic"`
}

func drawingBoardTextCmd() *cobra.Command {
	return boa.CmdT[drawingBoardTextParams]{
		Use:         "text <drawing board> <element> [--text <text>] [--font-size <n>] [--font <font>] [--style <style>]",
		Short:       "Change a text element, or the text attached to any other element, and its style",
		Args:        cobra.ExactArgs(2),
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *drawingBoardTextParams, _ *cobra.Command, args []string) {
			os.Exit(runDrawingBoardText(p, args[0], args[1], cliStdout, cliStderr))
		},
	}.ToCobra()
}

// runDrawingBoardText writes the element the drawing board shows the text of: only text
// elements draw theirs, so a shape's text lives on its attached element, which
// this adds if there is none yet.
func runDrawingBoardText(p *drawingBoardTextParams, id, elemID string, stdout, stderr io.Writer) int {
	doc, err := fetchDrawingBoard(id, drawingBoardWithTombstones)
	if err != nil {
		return drawingBoardErr(stderr, err)
	}
	s := loadDrawingBoardScene(doc)
	v, ok := s.view[elemID]
	if !ok {
		fmt.Fprintf(stderr, "Error: drawing board %s has no live element %s\n", id, elemID)
		return rcNotFound
	}
	style := drawingBoardTextStyle{Size: p.FontSize, Font: p.Font, Style: p.Style}
	if err := style.validate(); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcInvalidArg
	}
	if strings.TrimSpace(p.Text) == "" && !style.set() {
		fmt.Fprintln(stderr, "Error: --text is empty; `drawing-board rm` deletes text")
		return rcInvalidArg
	}
	target := elemID
	if v.Type != "text" {
		if target, ok = s.labelOf(elemID); !ok {
			if strings.TrimSpace(p.Text) == "" {
				fmt.Fprintf(stderr, "Error: element %s has no text to style; give --text too\n", elemID)
				return rcInvalidArg
			}
			target = s.addAttachedText(doc, elemID, p.Text)
		}
	}
	text := s.view[target].Text
	if strings.TrimSpace(p.Text) != "" {
		text = p.Text
		s.raw[target]["text"] = text
	}
	style.apply(s.raw[target])
	size, _ := s.raw[target]["fontSize"].(float64)
	if size == 0 {
		size = drawingBoardFontSize
	}
	t := s.view[target]
	if t.ContainerID != "" || style.Size != nil {
		t.Text = text
		t.Width, t.Height = drawingBoardTextSizeAt(text, size)
		s.view[target] = t
		s.raw[target]["width"], s.raw[target]["height"] = t.Width, t.Height
	}
	if t.ContainerID != "" {
		s.placeAttachedText(target)
	}
	return patchScene(id, s, []string{target}, stdout, stderr)
}

// addAttachedText adds an empty text element attached to elemID and returns
// its id. An `<id>-label` deleted earlier is written over at a higher version,
// or its tombstone would win the merge.
func (s *drawingBoardScene) addAttachedText(doc *drawingBoardDoc, elemID, text string) string {
	textID := elemID + "-label"
	if len(textID) > 64 {
		textID = drawingBoardRandomID()
	}
	stroke, _ := s.raw[elemID]["strokeColor"].(string)
	if stroke == "" {
		stroke = "#1e1e1e"
	}
	w, h := drawingBoardTextSize(text)
	s.raw[textID] = map[string]any{
		"id": textID, "type": "text", "version": float64(drawingBoardElementVersion(doc, textID)), "isDeleted": false,
		"strokeColor": stroke, "backgroundColor": "transparent", "opacity": 100, "angle": 0,
		"fontSize": drawingBoardFontSize, "textAlign": "center", "containerId": elemID, "x": 0.0, "y": 0.0, "width": w, "height": h,
	}
	s.view[textID] = drawingBoardElementView{ID: textID, Type: "text", Text: text, ContainerID: elemID, Width: w, Height: h}
	s.order = append(s.order, textID)
	s.placeAttachedText(textID)
	return textID
}

// drawingBoardElementVersion is the version doc holds of an element, tombstones
// included; 0 when it has none.
func drawingBoardElementVersion(doc *drawingBoardDoc, id string) int64 {
	for _, raw := range doc.Elements {
		var e drawingBoardElementView
		if json.Unmarshal(raw, &e) == nil && e.ID == id {
			return e.Version
		}
	}
	return 0
}
