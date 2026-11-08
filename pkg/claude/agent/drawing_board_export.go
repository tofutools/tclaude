package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
)

// ---- export ----

type drawingBoardExportParams struct {
	Output   string   `long:"output" short:"o" optional:"true" help:"File to write the image to (.png or .svg), or - for stdout (default: <drawing board>.png, or .svg with --format svg)"`
	Format   string   `long:"format" optional:"true" alts:"png,svg" help:"png or svg (default: from the -o extension, else png)"`
	Elements []string `long:"elements" optional:"true" help:"Only these element ids (comma-separated or repeated), with their attached text and the lines bound between them"`
	Region   string   `long:"region" optional:"true" help:"Only what meets this box, cropped to it: x,y,width,height in drawing board units"`
	Scale    float64  `long:"scale" optional:"true" help:"PNG pixels per drawing board unit (default 2, at most 4; the longest side is capped at 4096px)"`
	AttachTo string   `long:"attach-to" optional:"true" help:"Attach the PNG to this task instead of writing a file; exporting the same part again updates that file"`
	Comment  string   `long:"comment" optional:"true" help:"With --attach-to: post the drawing as a new comment with this text"`
	Name     string   `long:"name" optional:"true" help:"With --attach-to: the file's name (default: the drawing board's id, .png)"`
	Replace  int64    `long:"replace" optional:"true" help:"With --attach-to: update this drawing file (its id from 'taskboard show') instead of matching by board and part"`
	Node     string   `long:"node" optional:"true" help:"Export a linked node's drawing board, by its label or instance ID (operator only; at most about 6 MiB)"`
}

func drawingBoardExportCmd() *cobra.Command {
	return boa.CmdT[drawingBoardExportParams]{
		Use:   "export <drawing board>",
		Short: "Render the drawing board, or part of it, to PNG or SVG",
		Long: "Renders the drawing board as it is now, from its elements, to a PNG or an SVG. Unlike `snapshot` it needs no " +
			"open dashboard and is never stale. By default it draws the whole board; --elements draws only those elements " +
			"(with the text on them and the lines between them), and --region only what meets a box, cropped to it. " +
			"With --attach-to it puts the PNG on a task instead (or on a new comment with --comment), with a link back to " +
			"the drawing board; exporting the same part to the task again updates that file. " +
			"--node exports a linked node's drawing board under the grants it gave this node (operator only). " +
			"Text is drawn in the Go fonts, so it is close to the editor's, not identical. Needs read access to the drawing board.",
		Args:        cobra.ExactArgs(1),
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *drawingBoardExportParams, _ *cobra.Command, args []string) {
			os.Exit(runDrawingBoardExport(p, args[0], cliStdout, cliStderr))
		},
	}.ToCobra()
}

// parseDrawingBoardRegion reads "x,y,width,height".
func parseDrawingBoardRegion(s string) (map[string]float64, error) {
	parts := strings.Split(s, ",")
	if len(parts) != 4 {
		return nil, fmt.Errorf("--region is x,y,width,height")
	}
	var v [4]float64
	for i, part := range parts {
		f, err := strconv.ParseFloat(strings.TrimSpace(part), 64)
		if err != nil {
			return nil, fmt.Errorf("--region: %q is not a number", part)
		}
		v[i] = f
	}
	if v[2] <= 0 || v[3] <= 0 {
		return nil, fmt.Errorf("--region needs a positive width and height")
	}
	return map[string]float64{"x": v[0], "y": v[1], "width": v[2], "height": v[3]}, nil
}

func exportFormat(format, output string) (string, error) {
	format = strings.ToLower(strings.TrimSpace(format))
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(output), "."))
	switch {
	case format == "" && (ext == "png" || ext == "svg"):
		return ext, nil
	case format == "":
		return "png", nil
	case format != "png" && format != "svg":
		return "", fmt.Errorf("--format is png or svg")
	case output != "-" && (ext == "png" || ext == "svg") && ext != format:
		return "", fmt.Errorf("--format %s does not match %s", format, output)
	}
	return format, nil
}

func runDrawingBoardExport(p *drawingBoardExportParams, id string, stdout, stderr io.Writer) int {
	attach := strings.TrimSpace(p.AttachTo)
	switch {
	case attach != "" && p.Output != "":
		fmt.Fprintln(stderr, "Error: --attach-to puts the image on a task; leave out -o")
		return rcInvalidArg
	case attach == "" && (p.Comment != "" || p.Name != "" || p.Replace != 0):
		fmt.Fprintln(stderr, "Error: --comment, --name and --replace go with --attach-to")
		return rcInvalidArg
	case attach != "" && strings.TrimSpace(p.Node) != "":
		fmt.Fprintln(stderr, "Error: --node exports a linked node's drawing board to a file; attaching it to a task is not supported yet")
		return rcInvalidArg
	case attach != "" && strings.EqualFold(strings.TrimSpace(p.Format), "svg"):
		fmt.Fprintln(stderr, "Error: a drawing is attached as a PNG; export an SVG with -o")
		return rcInvalidArg
	}
	format, err := exportFormat(p.Format, p.Output)
	if err != nil {
		fmt.Fprintln(stderr, "Error:", err)
		return rcInvalidArg
	}
	if attach == "" && strings.TrimSpace(p.Output) == "" {
		p.Output = id + "." + format
	}
	req := map[string]any{"format": format}
	var ids []string
	for _, e := range p.Elements {
		for _, id := range strings.Split(e, ",") {
			if id = strings.TrimSpace(id); id != "" {
				ids = append(ids, id)
			}
		}
	}
	if len(ids) > 0 && p.Region != "" {
		fmt.Fprintln(stderr, "Error: --elements and --region pick the part to draw two ways; give one")
		return rcInvalidArg
	}
	if len(ids) > 0 {
		req["elements"] = ids
	}
	if p.Region != "" {
		region, err := parseDrawingBoardRegion(p.Region)
		if err != nil {
			fmt.Fprintln(stderr, "Error:", err)
			return rcInvalidArg
		}
		req["region"] = region
	}
	if p.Scale != 0 {
		if format == "svg" {
			fmt.Fprintln(stderr, "Error: --scale is for a PNG; an SVG scales by itself")
			return rcInvalidArg
		}
		req["scale"] = p.Scale
	}
	if attach != "" {
		return attachDrawingBoardExport(id, attach, p, req, stdout, stderr)
	}
	data, version, count, err := fetchDrawingBoardExport(id, strings.TrimSpace(p.Node), req)
	if err != nil {
		return drawingBoardErr(stderr, err)
	}
	if p.Output == "-" {
		if _, err := stdout.Write(data); err != nil {
			fmt.Fprintf(stderr, "Error: write stdout: %v\n", err)
			return rcIOFailure
		}
		return rcOK
	}
	if err := os.WriteFile(p.Output, data, 0o600); err != nil {
		fmt.Fprintf(stderr, "Error: write %s: %v\n", p.Output, err)
		return rcIOFailure
	}
	what := "the whole drawing board"
	if count != "" && (len(ids) > 0 || p.Region != "") {
		what = count + " element(s)"
	}
	fmt.Fprintf(stdout, "Wrote %s (%d bytes): %s at version %s.\n", p.Output, len(data), what, version)
	return rcOK
}

// fetchDrawingBoardExport renders the export on this node, or on node
// through its peer view, which carries the image base64-encoded in JSON.
func fetchDrawingBoardExport(id, node string, req map[string]any) (data []byte, version, elements string, err error) {
	if node == "" {
		body, _ := json.Marshal(req)
		data, hdr, err := DaemonPostRawBytes(drawingBoardPath(id, "export"), "application/json", body, drawingBoardOpts())
		if err != nil {
			return nil, "", "", err
		}
		return data, hdr.Get("X-Drawing-board-Version"), hdr.Get("X-Drawing-board-Export-Elements"), nil
	}
	var out struct {
		Data     []byte `json:"data"`
		Version  int64  `json:"version"`
		Elements int    `json:"elements"`
	}
	path := "/v1/federation/peer/" + url.PathEscape(node) + "/drawing-boards/" + url.PathEscape(id) + "/export?format=json"
	if err := DaemonRequest(http.MethodPost, path, req, &out, DaemonOpts{Timeout: 30 * time.Second, NoRetry: true}); err != nil {
		return nil, "", "", err
	}
	return out.Data, strconv.FormatInt(out.Version, 10), strconv.Itoa(out.Elements), nil
}

// attachDrawingBoardExport puts the export on a task (or a new comment on
// it) through the taskboard's drawings route.
func attachDrawingBoardExport(board, task string, p *drawingBoardExportParams, req map[string]any, stdout, stderr io.Writer) int {
	delete(req, "format")
	req["board"] = board
	if p.Name != "" {
		req["name"] = p.Name
	}
	if p.Comment != "" {
		req["comment"] = p.Comment
	}
	if p.Replace != 0 {
		req["replace"] = p.Replace
	}
	var out struct {
		ID         string `json:"id"`
		Comment    int64  `json:"comment"`
		Replaced   bool   `json:"replaced"`
		Attachment struct {
			ID      int64  `json:"id"`
			Name    string `json:"name"`
			Size    int64  `json:"size"`
			Drawing *struct {
				Version int64 `json:"version"`
			} `json:"drawing"`
		} `json:"attachment"`
	}
	path := "/v1/taskboard/tasks/" + url.PathEscape(task) + "/drawings"
	if err := DaemonRequest(http.MethodPost, path, req, &out, drawingBoardOpts()); err != nil {
		return drawingBoardErr(stderr, err)
	}
	version := int64(0)
	if out.Attachment.Drawing != nil {
		version = out.Attachment.Drawing.Version
	}
	a := out.Attachment
	switch {
	case out.Comment != 0:
		fmt.Fprintf(stdout, "Commented on %s (#%d) with %s (file %d, %d bytes): drawing board %s at version %d.\n", out.ID, out.Comment, a.Name, a.ID, a.Size, board, version)
	case out.Replaced:
		fmt.Fprintf(stdout, "Updated %s (file %d) on %s: drawing board %s at version %d.\n", a.Name, a.ID, out.ID, board, version)
	default:
		fmt.Fprintf(stdout, "Attached %s (file %d, %d bytes) to %s: drawing board %s at version %d. Exporting the same part again updates it.\n", a.Name, a.ID, a.Size, out.ID, board, version)
	}
	return rcOK
}
