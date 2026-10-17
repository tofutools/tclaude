package federationcmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/gorilla/websocket"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/common"
	"github.com/tofutools/tclaude/pkg/federation/terminal"
	"golang.org/x/term"
)

type attachParams struct {
	Target   string `pos:"true" help:"Stable agt_…@peer from federation sessions"`
	ReadOnly bool   `long:"read-only" help:"Watch without pane keyboard access"`
	TakeOver bool   `long:"take-over" help:"Disconnect the session's other viewers first, so this terminal sets its size"`
}

// attachHello is the daemon's first frame for a CLI that asked to drive the
// pane size: whether this terminal sets it, and if not, what else is attached.
type attachHello struct {
	Type      string   `json:"type"`
	SizeOwner bool     `json:"size_owner"`
	Others    []string `json:"others"`
}

// attachCloseText words the peer's closure reason for the operator.
func attachCloseText(reason string) string {
	switch reason {
	case "taken_over":
		return "another viewer took over this terminal (one viewer at a time)"
	case "kicked":
		return "the peer's operator disconnected this view"
	case "revoked":
		return "the peer revoked your access to this terminal"
	case "untrusted":
		return "the peer is no longer trusted"
	case "reincarnated":
		return "the session restarted on the peer"
	case "exit":
		return "the agent exited"
	}
	return ""
}

func attachCmd() *cobra.Command {
	return boa.CmdT[attachParams]{Use: "attach", Short: "Watch or type into a shared remote agent pane (Ctrl-] to leave)", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *attachParams, cmd *cobra.Command, _ []string) {
		os.Exit(runAttach(cmd.Context(), p, os.Stdin, os.Stdout, os.Stderr))
	}}.ToCobra()
}
func runAttach(ctx context.Context, p *attachParams, stdin *os.File, stdout, stderr io.Writer) int {
	if rc := agent.RequireDaemonOrExit(stderr); rc != 0 {
		return rc
	}
	if !term.IsTerminal(int(stdin.Fd())) {
		fmt.Fprintln(stderr, "remote attach requires a terminal on stdin")
		return 1
	}
	cols, rows, err := term.GetSize(int(stdin.Fd()))
	if err != nil {
		return fail(stderr, err)
	}
	query := url.Values{"target": {p.Target}, "cols": {fmt.Sprint(cols)}, "rows": {fmt.Sprint(rows)}, "drive_size": {"1"}}
	if p.ReadOnly {
		query.Set("read_only", "1")
	}
	if p.TakeOver {
		query.Set("take_over", "1")
	}
	ws, err := agent.DialDaemonWebSocket(ctx, "/v1/federation/attach?"+query.Encode())
	if err != nil {
		return fail(stderr, err)
	}
	defer func() { _ = ws.Close() }()
	ws.SetReadLimit(terminal.MaxPayload + 5)
	// A current daemon opens with a JSON hello; an older one goes straight to
	// terminal frames, so a binary first message is kept for the reader below.
	var first []byte
	kind, msg, err := ws.ReadMessage()
	if err != nil {
		return fail(stderr, err)
	}
	if kind == websocket.TextMessage {
		var hello attachHello
		if json.Unmarshal(msg, &hello) == nil && hello.Type == "hello" && !hello.SizeOwner && len(hello.Others) > 0 {
			fmt.Fprintf(stderr, "Also attached: %s. Showing the pane at their size; rerun with --take-over to disconnect them.\n", strings.Join(hello.Others, ", "))
		}
	} else {
		first = msg
	}
	fmt.Fprintf(stderr, "Attached to %s (read-only=%t). Ctrl-] to leave.\n", p.Target, p.ReadOnly)
	raw, err := term.MakeRaw(int(stdin.Fd()))
	if err != nil {
		return fail(stderr, err)
	}
	// closedReason is the peer's Closed frame, worded once the terminal is
	// restored.
	var closedReason atomic.Value
	defer func() {
		_ = term.Restore(int(stdin.Fd()), raw)
		fmt.Fprint(stdout, "\x1b[?1049l\x1b[?25h\x1b[?2004l\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1006l\x1b[0m")
		if reason, _ := closedReason.Load().(string); attachCloseText(reason) != "" {
			fmt.Fprintf(stderr, "Closed: %s.\n", attachCloseText(reason))
		}
	}()
	window := terminal.NewWindow()
	defer window.Close()
	var writeMu sync.Mutex
	write := func(f terminal.Frame) error {
		b, err := terminal.Encode(f)
		if err != nil {
			return err
		}
		writeMu.Lock()
		defer writeMu.Unlock()
		return ws.WriteMessage(websocket.BinaryMessage, b)
	}
	errs := make(chan error, 3)
	input := make(chan []byte, 8)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		b := make([]byte, 1024)
		for {
			n, err := stdin.Read(b)
			if err != nil {
				select {
				case errs <- err:
				case <-stop:
				}
				return
			}
			copyB := append([]byte(nil), b[:n]...)
			select {
			case input <- copyB:
			case <-stop:
				return
			}
		}
	}()
	go func() {
		for {
			kind, b, err := websocket.BinaryMessage, first, error(nil)
			if first == nil {
				kind, b, err = ws.ReadMessage()
			}
			first = nil
			if err != nil {
				errs <- err
				return
			}
			if kind != websocket.BinaryMessage {
				errs <- terminal.ErrProtocol
				return
			}
			f, err := terminal.Decode(b)
			if err != nil {
				errs <- err
				return
			}
			switch f.Kind {
			case terminal.Output:
				if _, err := io.Copy(stdout, bytes.NewReader(f.Data)); err != nil {
					errs <- err
					return
				}
				if len(f.Data) > 0 {
					if err := write(terminal.Frame{Kind: terminal.Credit, Data: terminal.Number(len(f.Data))}); err != nil {
						errs <- err
						return
					}
				}
			case terminal.Credit:
				n, err := terminal.ParseNumber(f.Data)
				if err == nil {
					err = window.Grant(n)
				}
				if err != nil {
					errs <- err
					return
				}
			case terminal.Closed:
				closedReason.Store(string(f.Data))
				errs <- io.EOF
				return
			default:
				errs <- terminal.ErrProtocol
				return
			}
		}
	}()
	// Keep reading while input credit is exhausted, so Ctrl-] and output credit
	// cannot be starved by a blocked pane write.
	send := make(chan terminal.Frame, 32)
	go func() {
		for {
			select {
			case f := <-send:
				if f.Kind == terminal.Input {
					for len(f.Data) > 0 {
						n, err := window.Take(min(len(f.Data), 1024))
						if err != nil {
							errs <- err
							return
						}
						if err := write(terminal.Frame{Kind: f.Kind, Data: f.Data[:n]}); err != nil {
							errs <- err
							return
						}
						f.Data = f.Data[n:]
					}
				} else if err := write(f); err != nil {
					errs <- err
					return
				}
			case <-stop:
				return
			}
		}
	}()
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()
	var pending []byte
	flush := func() bool {
		for _, f := range terminal.SplitReports(pending) {
			if f.Kind == terminal.Report || p.ReadOnly && f.Kind == terminal.Input {
				continue
			}
			select {
			case send <- f:
			default:
				return false
			}
		}
		pending = nil
		return true
	}
	for {
		select {
		case <-ctx.Done():
			return 0
		case <-errs:
			return 0
		case <-winch:
			c, r, err := term.GetSize(int(stdin.Fd()))
			if err == nil {
				select {
				case send <- terminal.Frame{Kind: terminal.Resize, Data: terminal.Size(c, r)}:
				default:
					return 1
				}
			}
		case b := <-input:
			for _, c := range b {
				if c == 0x1d {
					return 0
				}
			}
			pending = append(pending, b...)
			if len(pending) >= 4096 {
				if !flush() {
					return 1
				}
			}
			timer.Reset(20 * time.Millisecond)
		case <-timer.C:
			if !flush() {
				return 1
			}
		}
	}
}

type viewersParams struct {
	Session string `pos:"true" optional:"true" help:"Local agent or runtime session ID"`
	JSON    bool   `long:"json" help:"Output JSON"`
}
type remoteViewer struct {
	ID, Peer, Agent, Session, Group string
	ReadOnly                        bool `json:"read_only"`
	Started                         time.Time
}

func viewersCmd() *cobra.Command {
	return boa.CmdT[viewersParams]{Use: "viewers", Short: "List incoming remote terminal viewers (operator only)", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *viewersParams, _ *cobra.Command, _ []string) {
		var rows []remoteViewer
		if err := agent.DaemonGet("/v1/federation/viewers?session="+url.QueryEscape(p.Session), &rows); err != nil {
			os.Exit(fail(os.Stderr, err))
		}
		if p.JSON {
			os.Exit(printJSON(os.Stdout, rows))
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "VIEWER\tPEER\tAGENT\tGROUP\tMODE\tSTARTED")
		for _, v := range rows {
			mode := "interactive"
			if v.ReadOnly {
				mode = "watch"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", v.ID, v.Peer, v.Agent, v.Group, mode, ago(v.Started))
		}
		_ = tw.Flush()
	}}.ToCobra()
}

type kickParams struct {
	ID string `pos:"true" help:"Incoming viewer ID from federation viewers"`
}

func kickCmd() *cobra.Command {
	return boa.CmdT[kickParams]{Use: "kick", Short: "Disconnect one incoming remote terminal viewer (operator only)", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *kickParams, _ *cobra.Command, _ []string) {
		if rc := post(os.Stderr, "/v1/federation/viewers/"+url.PathEscape(p.ID)+"/kick", map[string]any{}, nil); rc != 0 {
			os.Exit(rc)
		}
		fmt.Println("Disconnected remote viewer", p.ID)
	}}.ToCobra()
}
