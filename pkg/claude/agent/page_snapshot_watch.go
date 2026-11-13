package agent

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"
)

// `page snapshot --watch` (tk_83873896): the agent watches its page settle
// and decides when to capture. The daemon streams one event per tick; this
// prints a line per tick, writes the frames asked for, and writes the final
// capture to --out. A capture is asked for with `page snapshot --session ID`
// from another shell, or with a line on this command's stdin.

// pageWatchStream opens the watch's event stream. A var so CLI tests can
// stand in for the daemon.
var pageWatchStream = DaemonStreamRequest

type pageWatchEventWire struct {
	Type       string   `json:"type"`
	Session    string   `json:"session,omitempty"`
	N          int      `json:"n,omitempty"`
	ElapsedMS  int64    `json:"elapsed_ms"`
	EveryMS    int64    `json:"every_ms,omitempty"`
	ForMS      int64    `json:"for_ms,omitempty"`
	Width      int      `json:"width,omitempty"`
	Height     int      `json:"height,omitempty"`
	MarksSeq   int64    `json:"marks_seq,omitempty"`
	Patches    int      `json:"patches,omitempty"`
	Loaded     bool     `json:"loaded,omitempty"`
	ReadyState string   `json:"ready_state,omitempty"`
	Path       string   `json:"path,omitempty"`
	Title      string   `json:"title,omitempty"`
	InFlight   int      `json:"in_flight"`
	LongLived  int      `json:"long_lived,omitempty"`
	Requests   int      `json:"requests"`
	Mutations  int      `json:"mutations"`
	TextHash   string   `json:"text_hash,omitempty"`
	TextLen    int      `json:"text_len"`
	Elements   int      `json:"elements"`
	LayoutHash string   `json:"layout_hash,omitempty"`
	PixelHash  string   `json:"pixel_hash,omitempty"`
	Changed    []string `json:"changed"`
	StillMS    int64    `json:"still_ms"`
	CalmMS     int64    `json:"calm_ms"`
	Unsettled  string   `json:"unsettled,omitempty"`
	Frame      string   `json:"frame,omitempty"`
	FrameFile  string   `json:"frame_file,omitempty"`
	Note       string   `json:"note,omitempty"`
	PNG        string   `json:"png,omitempty"`
	File       string   `json:"file,omitempty"`
	Reason     string   `json:"reason,omitempty"`
	Code       string   `json:"code,omitempty"`
	Error      string   `json:"error,omitempty"`
}

type pageSnapshotWire struct {
	PNG      string `json:"png"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	ScrollX  int    `json:"scroll_x"`
	ScrollY  int    `json:"scroll_y"`
	Path     string `json:"path"`
	MarksSeq int64  `json:"marks_seq,omitempty"`
	Patches  int    `json:"patches,omitempty"`
	Settle   *struct {
		WaitedMS int64  `json:"waited_ms"`
		Settled  bool   `json:"settled"`
		Still    bool   `json:"still,omitempty"`
		Why      string `json:"why,omitempty"`
	} `json:"settle,omitempty"`
	WatchMS  int64  `json:"watch_ms,omitempty"`
	StillMS  *int64 `json:"still_ms,omitempty"`
	CalmMS   *int64 `json:"calm_ms,omitempty"`
	Captured string `json:"captured,omitempty"`
}

func secs(ms int64) string {
	return fmt.Sprintf("%.1fs", float64(ms)/1000)
}

// parseWatchDuration reads a --every or --for value: a Go duration
// ("500ms", "10s") or plain seconds ("10").
func parseWatchDuration(flag, v string) (int64, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		f, ferr := strconv.ParseFloat(v, 64)
		if ferr != nil {
			return 0, fmt.Errorf("--%s takes a duration such as 500ms or 10s, not %q", flag, v)
		}
		d = time.Duration(f * float64(time.Second))
	}
	if d <= 0 {
		return 0, fmt.Errorf("--%s must be more than zero", flag)
	}
	return d.Milliseconds(), nil
}

// stdinLines is the watch's stdin, when it is one a person or a script can
// type into: a terminal or a pipe. Anything else (/dev/null, a file) is not
// read, so a background run never captures by accident.
func stdinLines() io.Reader {
	if term.IsTerminal(int(os.Stdin.Fd())) {
		return os.Stdin
	}
	if st, err := os.Stdin.Stat(); err == nil && st.Mode()&os.ModeNamedPipe != 0 {
		return os.Stdin
	}
	return nil
}

func runPageSnapshotWatch(p *pageSnapshotParams, name string, stdin io.Reader, stdout, stderr io.Writer) int {
	every, err := parseWatchDuration("every", p.Every)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return rcInvalidArg
	}
	watchFor, err := parseWatchDuration("for", p.For)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return rcInvalidArg
	}
	if p.FrameEvery < 0 || p.FrameScale < 0 || p.FrameScale > 1 {
		fmt.Fprintln(stderr, "error: --frame-every is 1 or more, and --frame-scale is from 0.1 to 1")
		return rcInvalidArg
	}
	frameEvery := 0
	if p.Frames != "" {
		frameEvery = max(p.FrameEvery, 1)
		if err := os.MkdirAll(p.Frames, 0o700); err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return rcIOFailure
		}
	} else if p.FrameEvery > 0 || p.FrameScale > 0 {
		fmt.Fprintln(stderr, "error: --frame-every and --frame-scale go with --frames DIR")
		return rcInvalidArg
	}
	body := map[string]any{"seq": p.Seq, "no_marks": p.NoMarks, "every_ms": every, "for_ms": watchFor,
		"frame_every": frameEvery, "frame_scale": p.FrameScale}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := pageWatchStream(ctx, http.MethodPost, pageAPIPath(name, "/snapshot/watch", nil), body)
	if err != nil {
		return drawingBoardErr(stderr, err)
	}
	defer stream.Close()

	var (
		start    pageWatchEventWire
		captures sync.WaitGroup
		outMu    sync.Mutex // stdout: ticks and stdin captures
		wrote    bool       // a stdin capture wrote --out
	)
	printf := func(format string, a ...any) {
		outMu.Lock()
		defer outMu.Unlock()
		fmt.Fprintf(stdout, format, a...)
	}
	emitJSON := func(ev pageWatchEventWire) {
		ev.PNG, ev.Frame = "", ""
		outMu.Lock()
		defer outMu.Unlock()
		_ = json.NewEncoder(stdout).Encode(ev)
	}
	dec := json.NewDecoder(stream)
	for {
		var ev pageWatchEventWire
		if err := dec.Decode(&ev); err != nil {
			if errors.Is(err, io.EOF) {
				fmt.Fprintln(stderr, "error: the watch ended without a capture (the daemon closed the stream)")
			} else {
				fmt.Fprintf(stderr, "error: reading the watch: %v\n", err)
			}
			return rcIOFailure
		}
		switch ev.Type {
		case "start":
			start = ev
			if p.JSON {
				emitJSON(ev)
			} else {
				printf("Watching page %s at %s (%dx%d) every %s for %s: session %s.\n", name, ev.Path, ev.Width, ev.Height,
					time.Duration(ev.EveryMS)*time.Millisecond, time.Duration(ev.ForMS)*time.Millisecond, ev.Session)
				printf("Capture it now with `tclaude agent page snapshot %s --session %s`", name, ev.Session)
				if stdin != nil {
					printf(", or a line here (\"frame\" for a frame that keeps watching)")
				}
				printf(".\n")
			}
			if stdin != nil {
				go func() {
					sc := bufio.NewScanner(stdin)
					for sc.Scan() {
						keep := strings.EqualFold(strings.TrimSpace(sc.Text()), "frame")
						captures.Add(1)
						out := p.Out
						if keep {
							out = strings.TrimSuffix(p.Out, ".png") + "-frame-" + time.Now().Format("150405.000") + ".png"
						}
						var buf strings.Builder
						rc := runPageSnapshotCapture(name, start.Session, keep, out, p.JSON, &buf, &buf)
						printf("%s", buf.String())
						if rc == rcOK && !keep {
							outMu.Lock()
							wrote = true
							outMu.Unlock()
						}
						captures.Done()
						if !keep {
							return
						}
					}
				}()
			}
		case "tick":
			if ev.Frame != "" {
				file := filepath.Join(p.Frames, fmt.Sprintf("frame-%03d.png", ev.N))
				if err := writeB64PNG(file, ev.Frame); err != nil {
					fmt.Fprintf(stderr, "error: %v\n", err)
					return rcIOFailure
				}
				ev.FrameFile = file
			}
			if p.JSON {
				emitJSON(ev)
				continue
			}
			printf("%s\n", pageWatchTickLine(ev))
		case "final":
			if err := writeB64PNG(p.Out, ev.PNG); err != nil {
				fmt.Fprintf(stderr, "error: %v\n", err)
				return rcIOFailure
			}
			ev.File = p.Out
			if p.JSON {
				emitJSON(ev)
				return rcOK
			}
			printf("Wrote %s at t=%s, the watch's end (the page had been calm for %s, still for %s): %s at %dx%d, %s.\n", p.Out, secs(ev.ElapsedMS),
				secs(ev.CalmMS), secs(ev.StillMS), start.Path, start.Width, start.Height, pageSnapshotMarks(start.MarksSeq, start.Patches))
			return rcOK
		case "end":
			captures.Wait()
			if p.JSON {
				emitJSON(ev)
				return rcOK
			}
			outMu.Lock()
			byStdin := wrote
			outMu.Unlock()
			if !byStdin {
				printf("The watch ended at t=%s: captured by `page snapshot --session %s`.\n", secs(ev.ElapsedMS), start.Session)
			}
			return rcOK
		case "error":
			captures.Wait()
			if p.JSON {
				emitJSON(ev)
			}
			fmt.Fprintf(stderr, "Error: %s\n", ev.Error)
			if ev.Code == "no_chrome" {
				return rcNotFound
			}
			return rcIOFailure
		}
	}
}

// pageWatchTickLine is one tick, as key=value pairs an agent can scan.
func pageWatchTickLine(ev pageWatchEventWire) string {
	var sb strings.Builder
	changed := "-"
	if len(ev.Changed) > 0 {
		changed = strings.Join(ev.Changed, ",")
	}
	fmt.Fprintf(&sb, "t=%s #%d calm=%s still=%s changed=%s", secs(ev.ElapsedMS), ev.N, secs(ev.CalmMS), secs(ev.StillMS), changed)
	doc := ev.ReadyState
	if doc == "" {
		doc = "-"
	}
	if !ev.Loaded {
		doc += "(not loaded)"
	}
	fmt.Fprintf(&sb, " doc=%s net=%d", doc, ev.InFlight-ev.LongLived)
	if ev.LongLived > 0 {
		fmt.Fprintf(&sb, "+%dlong", ev.LongLived)
	}
	fmt.Fprintf(&sb, "/%d", ev.Requests)
	if ev.Mutations >= 0 {
		fmt.Fprintf(&sb, " dom=+%d", ev.Mutations)
	}
	if ev.TextHash != "" {
		fmt.Fprintf(&sb, " text=%s/%d els=%d layout=%s", ev.TextHash, ev.TextLen, ev.Elements, ev.LayoutHash)
	}
	if ev.PixelHash != "" {
		fmt.Fprintf(&sb, " px=%s", ev.PixelHash)
	}
	if ev.Path != "" {
		fmt.Fprintf(&sb, " path=%s", ev.Path)
	}
	fmt.Fprintf(&sb, " title=%q", ev.Title)
	if ev.FrameFile != "" {
		fmt.Fprintf(&sb, " frame=%s", ev.FrameFile)
	}
	if ev.Note != "" {
		fmt.Fprintf(&sb, " (%s)", ev.Note)
	}
	if ev.CalmMS == 0 && ev.Unsettled != "" {
		fmt.Fprintf(&sb, " (not calm: %s)", ev.Unsettled)
	}
	return sb.String()
}

func pageSnapshotMarks(seq int64, patches int) string {
	s := "no marks"
	if seq > 0 {
		s = fmt.Sprintf("marks of batch %d", seq)
	}
	if patches > 0 {
		s += fmt.Sprintf(", %d live patches", patches)
	}
	return s
}

func writeB64PNG(file, b64 string) error {
	png, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return fmt.Errorf("the daemon sent an unreadable image: %w", err)
	}
	return os.WriteFile(file, png, 0o600)
}

// runPageSnapshotCapture asks a running watch for a capture: the final one,
// which ends the watch, or with keep a frame, and the watch goes on.
func runPageSnapshotCapture(name, session string, keep bool, out string, asJSON bool, stdout, stderr io.Writer) int {
	var resp pageSnapshotWire
	path := pageAPIPath(name, "/snapshot/watch/"+url.PathEscape(session)+"/capture", nil)
	if err := DaemonRequest(http.MethodPost, path, map[string]any{"keep": keep}, &resp, DaemonOpts{Timeout: 60 * time.Second}); err != nil {
		return drawingBoardErr(stderr, err)
	}
	if err := writeB64PNG(out, resp.PNG); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return rcIOFailure
	}
	var still, calm int64
	if resp.StillMS != nil {
		still = *resp.StillMS
	}
	if resp.CalmMS != nil {
		calm = *resp.CalmMS
	}
	if asJSON {
		return writeDrawingBoardJSON(stdout, map[string]any{"file": out, "captured": resp.Captured, "watch_ms": resp.WatchMS, "still_ms": still, "calm_ms": calm,
			"width": resp.Width, "height": resp.Height, "scroll_x": resp.ScrollX, "scroll_y": resp.ScrollY, "path": resp.Path, "marks_seq": resp.MarksSeq})
	}
	what := "the final capture; the watch has ended"
	if resp.Captured == "frame" {
		what = "a frame (no patches or marks drawn); the watch goes on"
	}
	fmt.Fprintf(stdout, "Wrote %s at t=%s of the watch (the page had been calm for %s, still for %s), %s: %s at %dx%d, %s.\n", out, secs(resp.WatchMS),
		secs(calm), secs(still), what, resp.Path, resp.Width, resp.Height, pageSnapshotMarks(resp.MarksSeq, resp.Patches))
	return rcOK
}
