package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
)

// ---- export ----

type drawingBoardExportParams struct {
	Output   string   `long:"output" short:"o" help:"File to write the image to (.png or .svg), or - for stdout"`
	Format   string   `long:"format" optional:"true" alts:"png,svg" help:"png or svg (default: from the -o extension, else png)"`
	Elements []string `long:"elements" optional:"true" help:"Only these element ids (comma-separated or repeated), with their attached text and the lines bound between them"`
	Region   string   `long:"region" optional:"true" help:"Only what meets this box, cropped to it: x,y,width,height in drawing board units"`
	Scale    float64  `long:"scale" optional:"true" help:"PNG pixels per drawing board unit (default 2, at most 4; the longest side is capped at 4096px)"`
}

func drawingBoardExportCmd() *cobra.Command {
	return boa.CmdT[drawingBoardExportParams]{
		Use:   "export <drawing board>",
		Short: "Render the drawing board, or part of it, to PNG or SVG",
		Long: "Renders the drawing board as it is now, from its elements, to a PNG or an SVG. Unlike `snapshot` it needs no " +
			"open dashboard and is never stale. By default it draws the whole board; --elements draws only those elements " +
			"(with the text on them and the lines between them), and --region only what meets a box, cropped to it. " +
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
	if strings.TrimSpace(p.Output) == "" {
		fmt.Fprintln(stderr, "Error: -o <file.png|file.svg|-> is required")
		return rcInvalidArg
	}
	format, err := exportFormat(p.Format, p.Output)
	if err != nil {
		fmt.Fprintln(stderr, "Error:", err)
		return rcInvalidArg
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
	body, _ := json.Marshal(req)
	data, hdr, err := DaemonPostRawBytes(drawingBoardPath(id, "export"), "application/json", body, drawingBoardOpts())
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
	if n := hdr.Get("X-Drawing-board-Export-Elements"); n != "" && (len(ids) > 0 || p.Region != "") {
		what = n + " element(s)"
	}
	fmt.Fprintf(stdout, "Wrote %s (%d bytes): %s at version %s.\n", p.Output, len(data), what, hdr.Get("X-Drawing-board-Version"))
	return rcOK
}
