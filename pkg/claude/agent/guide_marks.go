package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
)

// `tclaude agent guide mark|marks|unmark` draw on the operator's view, and
// `guide captures` reads back saved captures.

type guideMarkParams struct {
	Kind    string `pos:"true" help:"highlight, circle, underline, box, arrow, freehand or label"`
	Target  string `long:"target" optional:"true" help:"Visible text of the control to mark"`
	Ref     string `long:"ref" optional:"true" help:"CSS selector of the control, steadier than its text; a capture reports a control's ref"`
	From    string `long:"from" optional:"true" help:"Visible text an arrow starts from"`
	Region  string `long:"region" optional:"true" help:"Screen region as x,y,width,height in viewport pixels, as a capture reports them"`
	Points  string `long:"points" optional:"true" help:"Freehand stroke as space-separated x,y viewport points"`
	Path    string `long:"path" optional:"true" help:"Keep the mark to this dashboard page"`
	Tip     string `long:"tip" optional:"true" help:"Short text beside the mark saying what it is about"`
	Timeout string `long:"timeout" optional:"true" help:"How long it stays up, such as 30s or 5m (default 60s, at most 10m)"`
	Blink   bool   `long:"blink" help:"Pulse the mark; steady for operators who prefer reduced motion"`
	Color   string `long:"color" optional:"true" help:"red, yellow, blue, green or purple"`
	Replace int64  `long:"replace" optional:"true" help:"Mark id this one replaces"`
	JSON    bool   `long:"json" help:"Output JSON"`
}

func parseMarkRegion(raw string) (map[string]float64, error) {
	parts := strings.Split(raw, ",")
	if len(parts) != 4 {
		return nil, fmt.Errorf("--region is x,y,width,height")
	}
	values := make([]float64, 4)
	for i, part := range parts {
		value, err := strconv.ParseFloat(strings.TrimSpace(part), 64)
		if err != nil {
			return nil, fmt.Errorf("--region is x,y,width,height")
		}
		values[i] = value
	}
	return map[string]float64{"x": values[0], "y": values[1], "width": values[2], "height": values[3]}, nil
}

func parseMarkPoints(raw string) ([][2]float64, error) {
	var points [][2]float64
	for _, field := range strings.Fields(raw) {
		x, y, ok := strings.Cut(field, ",")
		px, errX := strconv.ParseFloat(x, 64)
		py, errY := strconv.ParseFloat(y, 64)
		if !ok || errX != nil || errY != nil {
			return nil, fmt.Errorf("--points is space-separated x,y pairs")
		}
		points = append(points, [2]float64{px, py})
	}
	return points, nil
}

func guideMarkCmd() *cobra.Command {
	return boa.CmdT[guideMarkParams]{
		Use:   "mark <kind>",
		Short: "Draw a mark on the operator's view to get their attention",
		Long: "Draws on the operator's dashboard: a highlight, circle, underline, box, arrow, freehand stroke or label, " +
			"with an optional short tip. Marks take no clicks, follow their control and time out on their own. The operator " +
			"can dismiss, acknowledge or reply at the mark, and a reply reaches you as mail naming the mark. " +
			"Confirm it landed with `guide capture`, which draws it and reports each mark.\n\n" +
			"Example: tclaude agent guide mark highlight --target 'Save draft' --blink --tip 'Save once the diff looks right'\n" +
			"         tclaude agent guide mark arrow --from Review --target Rework --tip 'This never loops back'\n" +
			"         tclaude agent guide mark box --region 120,300,400,80 --timeout 5m --tip 'This row'",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *guideMarkParams, _ *cobra.Command, _ []string) {
			os.Exit(runGuideMark(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runGuideMark(p *guideMarkParams, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	spec := map[string]any{"target": p.Target, "ref": p.Ref, "from": p.From, "path": p.Path, "blink": p.Blink, "color": p.Color}
	if p.Region != "" {
		region, err := parseMarkRegion(p.Region)
		if err != nil {
			fmt.Fprintf(stderr, "Error: %v\n", err)
			return rcInvalidArg
		}
		spec["region"] = region
	}
	if p.Points != "" {
		points, err := parseMarkPoints(p.Points)
		if err != nil {
			fmt.Fprintf(stderr, "Error: %v\n", err)
			return rcInvalidArg
		}
		spec["points"] = points
	}
	body := map[string]any{"kind": p.Kind, "spec": spec, "tip": p.Tip, "replaces": p.Replace}
	if p.Timeout != "" {
		timeout, err := time.ParseDuration(p.Timeout)
		if err != nil {
			fmt.Fprintln(stderr, "Error: --timeout is a duration such as 30s or 5m")
			return rcInvalidArg
		}
		body["timeout_seconds"] = timeout.Seconds()
	}
	var resp guideMarkView
	if err := DaemonPost("/v1/dashboard/marks", body, &resp); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if p.JSON {
		return writeGuideJSON(stdout, resp)
	}
	fmt.Fprintf(stdout, "Mark %d is up until %s.\n", resp.ID, resp.ExpiresAt.Local().Format(time.TimeOnly))
	return rcOK
}

func writeGuideJSON(stdout io.Writer, value any) int {
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(value); err != nil {
		return rcIOFailure
	}
	return rcOK
}

type guideMarkView struct {
	ID        int64           `json:"id"`
	AgentID   string          `json:"agent_id,omitempty"`
	AgentName string          `json:"agent_name,omitempty"`
	Kind      string          `json:"kind"`
	Spec      json.RawMessage `json:"spec"`
	Tip       string          `json:"tip,omitempty"`
	State     string          `json:"state"`
	EndedBy   string          `json:"ended_by,omitempty"`
	Reply     string          `json:"reply,omitempty"`
	RepliedAt *time.Time      `json:"replied_at,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
	ExpiresAt time.Time       `json:"expires_at"`
	EndedAt   *time.Time      `json:"ended_at,omitempty"`
}

type guideMarksParams struct {
	State  string `long:"state" optional:"true" help:"Only marks in this state: active, expired, dismissed, acknowledged, cleared or replaced"`
	Limit  int    `long:"limit" optional:"true" help:"Marks per page (default 50, at most 200)"`
	Before int64  `long:"before" optional:"true" help:"Continue below this mark id, as the last page printed"`
	JSON   bool   `long:"json" help:"Output JSON"`
}

func guideListPath(path string, query url.Values, limit int, before int64) string {
	if limit > 0 {
		query.Set("limit", strconv.Itoa(limit))
	}
	if before > 0 {
		query.Set("before", strconv.FormatInt(before, 10))
	}
	if len(query) == 0 {
		return path
	}
	return path + "?" + query.Encode()
}

func guideMarksCmd() *cobra.Command {
	return boa.CmdT[guideMarksParams]{
		Use:         "marks",
		Short:       "List marks, newest first, with who ended each and how",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *guideMarksParams, _ *cobra.Command, _ []string) {
			os.Exit(runGuideMarks(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runGuideMarks(p *guideMarksParams, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	query := url.Values{}
	if p.State != "" {
		query.Set("state", p.State)
	}
	var resp struct {
		Marks      []guideMarkView `json:"marks"`
		NextBefore int64           `json:"next_before,omitempty"`
	}
	if err := DaemonGet(guideListPath("/v1/dashboard/marks", query, p.Limit, p.Before), &resp); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if p.JSON {
		return writeGuideJSON(stdout, resp)
	}
	if len(resp.Marks) == 0 {
		fmt.Fprintln(stdout, "(no marks)")
		return rcOK
	}
	for _, mark := range resp.Marks {
		fmt.Fprintf(stdout, "%d  %-12s %-9s %s", mark.ID, mark.State, mark.Kind, mark.CreatedAt.Local().Format(time.DateTime))
		if mark.EndedBy != "" {
			fmt.Fprintf(stdout, "  ended by %s", mark.EndedBy)
		}
		fmt.Fprintln(stdout)
		if mark.Tip != "" {
			fmt.Fprintf(stdout, "    tip:   %s\n", mark.Tip)
		}
		if mark.Reply != "" {
			fmt.Fprintf(stdout, "    reply: %s\n", mark.Reply)
		}
	}
	if resp.NextBefore != 0 {
		fmt.Fprintf(stdout, "More: --before %d\n", resp.NextBefore)
	}
	return rcOK
}

type guideUnmarkParams struct {
	ID  int64 `pos:"true" optional:"true" help:"Mark to take down"`
	All bool  `long:"all" help:"Take down all your marks"`
}

func guideUnmarkCmd() *cobra.Command {
	return boa.CmdT[guideUnmarkParams]{
		Use:         "unmark [id]",
		Short:       "Take down a mark, or all of yours",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *guideUnmarkParams, _ *cobra.Command, _ []string) {
			os.Exit(runGuideUnmark(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runGuideUnmark(p *guideUnmarkParams, stdout, stderr io.Writer) int {
	if (p.ID == 0) == !p.All {
		fmt.Fprintln(stderr, "Error: pass a mark id or --all")
		return rcInvalidArg
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	if p.All {
		var resp struct {
			Cleared int64 `json:"cleared"`
		}
		if err := DaemonRequest(http.MethodDelete, "/v1/dashboard/marks", nil, &resp, DaemonOpts{}); err != nil {
			fmt.Fprintf(stderr, "Error: %v\n", err)
			return MapDaemonErrorToRC(err)
		}
		fmt.Fprintf(stdout, "Took down %d marks.\n", resp.Cleared)
		return rcOK
	}
	if err := DaemonDelete(fmt.Sprintf("/v1/dashboard/marks/%d", p.ID), nil); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	fmt.Fprintf(stdout, "Took down mark %d.\n", p.ID)
	return rcOK
}

// --- saved captures ---

type guideCapturesParams struct {
	Limit  int   `long:"limit" optional:"true" help:"Captures per page (default 50, at most 200)"`
	Before int64 `long:"before" optional:"true" help:"Continue below this capture id, as the last page printed"`
	JSON   bool  `long:"json" help:"Output JSON"`
}

type guideSavedCapture struct {
	ID        int64           `json:"id"`
	AgentName string          `json:"agent_name,omitempty"`
	Label     string          `json:"label,omitempty"`
	Size      int64           `json:"size"`
	CreatedAt time.Time       `json:"created_at"`
	Context   json.RawMessage `json:"context,omitempty"`
}

func guideCapturesCmd() *cobra.Command {
	return boa.CmdT[guideCapturesParams]{
		Use:   "captures",
		Short: "List saved captures, newest first, or fetch one with `get`",
		Long: "Lists what `guide capture --save` kept, so you can recover what has been going on, for example after compaction. " +
			"The daemon keeps the newest 50 captures, up to 256 MiB in all.",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds:     []*cobra.Command{guideCaptureGetCmd()},
		RunFunc: func(p *guideCapturesParams, _ *cobra.Command, _ []string) {
			os.Exit(runGuideCaptures(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runGuideCaptures(p *guideCapturesParams, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	var resp struct {
		Captures   []guideSavedCapture `json:"captures"`
		NextBefore int64               `json:"next_before,omitempty"`
	}
	if err := DaemonGet(guideListPath("/v1/dashboard/captures", url.Values{}, p.Limit, p.Before), &resp); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if p.JSON {
		return writeGuideJSON(stdout, resp)
	}
	if len(resp.Captures) == 0 {
		fmt.Fprintln(stdout, "(no saved captures)")
		return rcOK
	}
	for _, capture := range resp.Captures {
		fmt.Fprintf(stdout, "%d  %s  %s\n", capture.ID, capture.CreatedAt.Local().Format(time.DateTime), capture.Label)
	}
	if resp.NextBefore != 0 {
		fmt.Fprintf(stdout, "More: --before %d\n", resp.NextBefore)
	}
	return rcOK
}

type guideCaptureGetParams struct {
	ID     int64  `pos:"true" help:"Saved capture id"`
	Output string `short:"o" long:"output" optional:"true" help:"File to write the PNG to (default: a new file in the temp directory)"`
}

func guideCaptureGetCmd() *cobra.Command {
	return boa.CmdT[guideCaptureGetParams]{
		Use:         "get <id>",
		Short:       "Fetch a saved capture's image and context",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *guideCaptureGetParams, _ *cobra.Command, _ []string) {
			os.Exit(runGuideCaptureGet(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runGuideCaptureGet(p *guideCaptureGetParams, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	var saved guideSavedCapture
	if err := DaemonGet(fmt.Sprintf("/v1/dashboard/captures/%d", p.ID), &saved); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	image, _, err := DaemonGetRaw(fmt.Sprintf("/v1/dashboard/captures/%d?image=1", p.ID))
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	output, err := writeGuideCapture(p.Output, image)
	if err != nil {
		fmt.Fprintf(stderr, "Error: write image: %v\n", err)
		return rcIOFailure
	}
	fmt.Fprintf(stdout, "image:   %s\ntaken:   %s\n", output, saved.CreatedAt.Local().Format(time.DateTime))
	if saved.Label != "" {
		fmt.Fprintf(stdout, "label:   %s\n", saved.Label)
	}
	if len(saved.Context) > 0 {
		var context guideCaptureContext
		if err := json.Unmarshal(saved.Context, &context); err == nil {
			printGuideCaptureContext(stdout, &context)
		}
	}
	return rcOK
}
