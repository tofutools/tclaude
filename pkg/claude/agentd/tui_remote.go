package agentd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/GiGurra/boa/pkg/boa"
	"github.com/gorilla/websocket"
	"github.com/muesli/cancelreader"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/common"
	"golang.org/x/term"
)

const (
	tuiHTTPPrefix               = "/api/tui"
	remoteTUIEscapeByte         = byte(0x1d) // Ctrl-]
	remoteTUIDetachCommand      = byte('d')
	remoteTUIDetachCommandUpper = byte('D')
)

var errRemoteTUIDetached = errors.New("remote terminal detached")

type tuiDashboardParams struct {
	ConnectTo           string `long:"connect-to" required:"true" help:"Dashboard URL or host[:port] to connect to (for example 10.0.0.4:8321 or https://agents.example.com)."`
	OperatorToken       string `long:"operator-token" optional:"true" help:"Operator token for this dashboard, overriding local token detection. Prefer the environment or --remote-operator-token when shell history/process listings are a concern."`
	RemoteOperatorToken string `long:"remote-operator-token" optional:"true" help:"Read the operator token over SSH from user@host:/absolute/path (also accepts user@host/absolute/path), overriding local token detection."`
}

// TUIDashboardCmd builds the standalone `tclaude agent tui-dashboard`
// command. Its implementation lives with the shared agentd TUI model, while
// claude.Cmd mounts it beside `tclaude agent dashboard`; this process is a
// client, and quitting it must not imply that the daemon should stop.
func TUIDashboardCmd() *cobra.Command {
	return boa.CmdT[tuiDashboardParams]{
		Use:         "tui-dashboard",
		Aliases:     []string{"tui"},
		Short:       "Connect a terminal dashboard to a running agentd",
		Long:        "Runs the terminal dashboard as a standalone HTTP client. It authenticates with an explicit --operator-token, a token read through SSH with --remote-operator-token, or the normal local operator-token lookup (in that order), and keeps polling through agentd restarts.",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *tuiDashboardParams, cmd *cobra.Command, _ []string) {
			token, err := resolveTUIOperatorToken(
				p,
				cmd.Flags().Changed("operator-token"),
				cmd.Flags().Changed("remote-operator-token"),
			)
			if err == nil {
				err = runRemoteTUIDashboard(p.ConnectTo, token)
			}
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
		},
	}.ToCobra()
}

func runRemoteTUIDashboard(target, token string) error {
	api, err := newRemoteTUIAPI(target, token)
	if err != nil {
		return err
	}
	_, err = tea.NewProgram(newTUIModel(api)).Run()
	if tuiEndedNormally(err) {
		return nil
	}
	return err
}

func resolveTUIOperatorToken(p *tuiDashboardParams, directSet, remoteSet bool) (string, error) {
	if directSet && remoteSet {
		return "", fmt.Errorf("--operator-token and --remote-operator-token are mutually exclusive")
	}
	if directSet {
		if token := strings.TrimSpace(p.OperatorToken); token != "" {
			return token, nil
		}
		return "", fmt.Errorf("--operator-token is empty")
	}
	if remoteSet {
		source := strings.TrimSpace(p.RemoteOperatorToken)
		if source == "" {
			return "", fmt.Errorf("--remote-operator-token is empty")
		}
		return readRemoteTUIOperatorToken(source)
	}
	if token := agent.OperatorToken(); token != "" {
		return token, nil
	}
	return "", fmt.Errorf(
		"no operator token available; use --operator-token, --remote-operator-token, or export %s",
		agent.HumanTokenEnvVar,
	)
}

var runTUIOperatorTokenSSH = func(destination, path string) ([]byte, error) {
	remoteCommand := "cat -- " + shellSingleQuote(path)
	cmd := exec.Command("ssh", destination, remoteCommand)
	cmd.Stdin = os.Stdin
	cmd.Stderr = os.Stderr
	return cmd.Output()
}

func readRemoteTUIOperatorToken(source string) (string, error) {
	destination, path, err := parseRemoteTUIOperatorTokenSource(source)
	if err != nil {
		return "", err
	}
	raw, err := runTUIOperatorTokenSSH(destination, path)
	if err != nil {
		return "", fmt.Errorf("read operator token from %s:%s over SSH: %w", destination, path, err)
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", fmt.Errorf("operator token at %s:%s is empty", destination, path)
	}
	return token, nil
}

func parseRemoteTUIOperatorTokenSource(source string) (destination, path string, err error) {
	source = strings.TrimSpace(source)
	if source == "" {
		return "", "", fmt.Errorf("remote operator token source is empty")
	}
	if i := strings.Index(source, ":/"); i >= 0 {
		destination, path = source[:i], source[i+1:]
	} else if i := strings.IndexByte(source, '/'); i >= 0 {
		destination, path = source[:i], source[i:]
	} else {
		return "", "", fmt.Errorf(
			"--remote-operator-token must be user@host:/absolute/path or user@host/absolute/path",
		)
	}
	if destination == "" || strings.HasPrefix(destination, "-") ||
		strings.ContainsAny(destination, " \t\r\n\x00") {
		return "", "", fmt.Errorf("invalid SSH destination in --remote-operator-token")
	}
	if !strings.HasPrefix(path, "/") || strings.ContainsAny(path, "\r\n\x00") {
		return "", "", fmt.Errorf("remote operator token path must be absolute and contain no newlines")
	}
	return destination, path, nil
}

// remoteTUIAPI is the HTTP implementation of tuiAPI. Its cookie jar matters:
// the operator token bootstraps a dashboard session on the first request, and
// that session has the same clean-restart handoff as the web UI. A new request
// is made on every poll, so a refused/reset connection is transient; the model
// keeps polling and repopulates once the endpoint returns.
type remoteTUIAPI struct {
	baseURL              string
	origin               string
	token                string
	client               *http.Client
	wsDialer             *websocket.Dialer
	mutationRetryBackoff []time.Duration
}

func newRemoteTUIAPI(target, token string) (*remoteTUIAPI, error) {
	base, origin, err := normalizeTUIConnectURL(target)
	if err != nil {
		return nil, err
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, fmt.Errorf("operator token is empty")
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("create HTTP cookie jar: %w", err)
	}
	wsDialer := *websocket.DefaultDialer
	wsDialer.HandshakeTimeout = 10 * time.Second
	return &remoteTUIAPI{
		baseURL: base,
		origin:  origin,
		token:   token,
		client: &http.Client{
			Timeout: 10 * time.Second,
			Jar:     jar,
			// These JSON endpoints never redirect. In particular, do not let
			// net/http copy the custom operator-token header to a redirect
			// target (its automatic cross-host stripping covers Authorization
			// and Cookie, not arbitrary bearer headers).
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		wsDialer: &wsDialer,
		mutationRetryBackoff: []time.Duration{
			time.Second,
			2 * time.Second,
			4 * time.Second,
			8 * time.Second,
			16 * time.Second,
		},
	}, nil
}

func normalizeTUIConnectURL(target string) (base, origin string, err error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return "", "", fmt.Errorf("--connect-to requires a URL or host[:port]")
	}
	if !strings.Contains(target, "://") {
		target = "http://" + target
	}
	u, err := url.Parse(target)
	if err != nil {
		return "", "", fmt.Errorf("parse --connect-to: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", "", fmt.Errorf("--connect-to scheme must be http or https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return "", "", fmt.Errorf("--connect-to must include a host")
	}
	if u.User != nil {
		return "", "", fmt.Errorf("--connect-to must not include URL user information")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", "", fmt.Errorf("--connect-to must not include a query string or fragment")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	return u.String(), u.Scheme + "://" + u.Host, nil
}

func (a *remoteTUIAPI) get(path string, out any) error {
	return a.do(http.MethodGet, path, nil, out)
}

func (a *remoteTUIAPI) post(path string, in, out any) error {
	return a.do(http.MethodPost, path, in, out)
}

func (a *remoteTUIAPI) isOperator() bool { return true }

func (a *remoteTUIAPI) identityWarning() string { return "" }

func (a *remoteTUIAPI) connectionLabel() string { return a.baseURL }

func (a *remoteTUIAPI) capabilities() tuiCapabilities {
	return tuiCapabilities{attachAgent: true}
}

func (a *remoteTUIAPI) attach(agentName, convID, _ string) tea.Cmd {
	command := &remoteTUIAttachCommand{
		api:       a,
		agentName: agentName,
		convID:    convID,
	}
	return tea.Exec(command, func(err error) tea.Msg {
		return tuiAttachedMsg{
			agent:   agentName,
			session: a.baseURL,
			remote:  true,
			err:     err,
		}
	})
}

// remoteTUIAttachCommand lets bubbletea release its alternate screen and input
// reader while the dashboard's existing terminal WebSocket owns this terminal.
// Ctrl-] D is consumed here as a transport-level detach escape, so it works
// even when the dashboard itself is running inside another tmux. Ctrl-] Ctrl-]
// sends one literal Ctrl-] to the remote terminal. When the detach escape, the
// server-side tmux detach, or an agentd restart closes the socket, Run returns
// and bubbletea restores the dashboard.
type remoteTUIAttachCommand struct {
	api       *remoteTUIAPI
	agentName string
	convID    string
	stdin     io.Reader
	stdout    io.Writer
	stderr    io.Writer
}

func (c *remoteTUIAttachCommand) SetStdin(r io.Reader)  { c.stdin = r }
func (c *remoteTUIAttachCommand) SetStdout(w io.Writer) { c.stdout = w }
func (c *remoteTUIAttachCommand) SetStderr(w io.Writer) { c.stderr = w }

func (c *remoteTUIAttachCommand) Run() error {
	if strings.TrimSpace(c.convID) == "" {
		return fmt.Errorf("%s has no conversation id to attach to", c.agentName)
	}
	if c.stdin == nil || c.stdout == nil {
		return fmt.Errorf("remote terminal requires stdin and stdout")
	}
	conn, err := c.dial()
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	cancelInput, err := cancelreader.NewReader(c.stdin)
	if err != nil {
		return fmt.Errorf("prepare terminal input: %w", err)
	}
	defer func() { _ = cancelInput.Close() }()

	var restore func() error
	if file, ok := c.stdin.(interface{ Fd() uintptr }); ok && term.IsTerminal(int(file.Fd())) {
		state, rawErr := term.MakeRaw(int(file.Fd()))
		if rawErr != nil {
			return fmt.Errorf("put terminal in raw mode: %w", rawErr)
		}
		restore = func() error { return term.Restore(int(file.Fd()), state) }
		defer func() { _ = restore() }()
	}

	var writeMu sync.Mutex
	writeMessage := func(messageType int, data []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return conn.WriteMessage(messageType, data)
	}
	sendSize := func() {
		file, ok := c.stdin.(interface{ Fd() uintptr })
		if !ok {
			return
		}
		cols, rows, sizeErr := term.GetSize(int(file.Fd()))
		if sizeErr != nil || cols <= 0 || rows <= 0 {
			return
		}
		raw, marshalErr := json.Marshal(termResizeMsg{Type: "resize", Cols: cols, Rows: rows})
		if marshalErr == nil {
			_ = writeMessage(websocket.TextMessage, raw)
		}
	}
	sendSize()

	stopResize := make(chan struct{})
	resizes := make(chan os.Signal, 1)
	signal.Notify(resizes, syscall.SIGWINCH)
	var pumps sync.WaitGroup
	pumps.Add(1)
	go func() {
		defer pumps.Done()
		for {
			select {
			case <-stopResize:
				return
			case <-resizes:
				sendSize()
			}
		}
	}()

	inputErr := make(chan error, 1)
	pumps.Add(1)
	go func() {
		defer pumps.Done()
		buf := make([]byte, 4096)
		escapePending := false
		for {
			n, readErr := cancelInput.Read(buf)
			if n > 0 {
				data, detach, pending := remoteTUIInput(buf[:n], escapePending)
				escapePending = pending
				if len(data) > 0 {
					if writeErr := writeMessage(websocket.BinaryMessage, data); writeErr != nil {
						inputErr <- writeErr
						_ = conn.Close()
						return
					}
				}
				if detach {
					inputErr <- errRemoteTUIDetached
					_ = conn.Close()
					return
				}
			}
			if readErr != nil {
				if escapePending {
					if writeErr := writeMessage(websocket.BinaryMessage, []byte{remoteTUIEscapeByte}); writeErr != nil {
						inputErr <- writeErr
						_ = conn.Close()
						return
					}
				}
				if !errors.Is(readErr, cancelreader.ErrCanceled) {
					inputErr <- readErr
					_ = conn.Close()
				}
				return
			}
		}
	}()

	var streamErr error
	for {
		messageType, data, readErr := conn.ReadMessage()
		if readErr != nil {
			break
		}
		if messageType != websocket.BinaryMessage && messageType != websocket.TextMessage {
			continue
		}
		if _, writeErr := c.stdout.Write(data); writeErr != nil {
			streamErr = writeErr
			break
		}
	}

	signal.Stop(resizes)
	close(stopResize)
	cancelInput.Cancel()
	_ = conn.Close()
	pumps.Wait()
	select {
	case err := <-inputErr:
		if streamErr == nil && !errors.Is(err, io.EOF) && !errors.Is(err, errRemoteTUIDetached) {
			streamErr = err
		}
	default:
	}
	if streamErr != nil {
		return fmt.Errorf("remote terminal stream: %w", streamErr)
	}
	return nil
}

// remoteTUIInput applies the remote stream's two-byte escape protocol. An
// escape can straddle reads, so pending is passed back to the caller. Doubling
// the prefix quotes it; an unrecognized command remains transparent.
func remoteTUIInput(input []byte, pending bool) (output []byte, detach, stillPending bool) {
	output = make([]byte, 0, len(input))
	for _, b := range input {
		if !pending {
			if b == remoteTUIEscapeByte {
				pending = true
			} else {
				output = append(output, b)
			}
			continue
		}
		switch b {
		case remoteTUIDetachCommand, remoteTUIDetachCommandUpper:
			return output, true, false
		case remoteTUIEscapeByte:
			output = append(output, remoteTUIEscapeByte)
		default:
			output = append(output, remoteTUIEscapeByte, b)
		}
		pending = false
	}
	return output, false, pending
}

func (c *remoteTUIAttachCommand) dial() (*websocket.Conn, error) {
	httpURL, err := url.Parse(c.api.baseURL + tuiHTTPPrefix + "/attach-ws/" + url.PathEscape(c.convID))
	if err != nil {
		return nil, fmt.Errorf("build remote terminal URL: %w", err)
	}
	wsURL := *httpURL
	if wsURL.Scheme == "https" {
		wsURL.Scheme = "wss"
	} else {
		wsURL.Scheme = "ws"
	}
	headers := http.Header{"Origin": []string{c.api.origin}}
	for _, cookie := range c.api.client.Jar.Cookies(httpURL) {
		headers.Add("Cookie", cookie.String())
	}
	conn, resp, err := c.api.wsDialer.Dial(wsURL.String(), headers)
	if resp != nil {
		c.api.client.Jar.SetCookies(httpURL, resp.Cookies())
	}
	if err != nil {
		if resp != nil {
			defer func() { _ = resp.Body.Close() }()
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
			if msg := strings.TrimSpace(string(raw)); msg != "" {
				return nil, fmt.Errorf("connect remote terminal: %s: %s", resp.Status, msg)
			}
			return nil, fmt.Errorf("connect remote terminal: %s", resp.Status)
		}
		return nil, fmt.Errorf("connect remote terminal: %w", err)
	}
	return conn, nil
}

func (a *remoteTUIAPI) do(method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, a.baseURL+tuiHTTPPrefix+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// The token bootstraps (or recovers) the restart-surviving dashboard
	// session. Origin lets subsequent cookie-authenticated requests pass the
	// dashboard's same-origin check; it is derived from the target, never
	// caller-supplied independently.
	req.Header.Set(agent.HumanTokenHeader, a.token)
	req.Header.Set("Origin", a.origin)
	if err := agent.PrepareIdempotentRequest(req); err != nil {
		return fmt.Errorf("prepare %s %s for retry: %w", method, path, err)
	}

	resp, raw, err := a.doRequestBytes(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		msg := strings.TrimSpace(string(raw))
		var payload struct {
			Error string `json:"error"`
			Code  string `json:"code"`
		}
		if json.Unmarshal(raw, &payload) == nil && strings.TrimSpace(payload.Error) != "" {
			msg = strings.TrimSpace(payload.Error)
		}
		if msg == "" {
			msg = http.StatusText(resp.StatusCode)
		}
		if resp.StatusCode == http.StatusNotFound {
			msg += " (the target may be running a tclaude version without remote TUI support)"
			// Typed, so an optional readout can tell "this daemon has no such
			// operation" apart from "this call failed" — see
			// tuiEndpointUnsupported. The message is unchanged: anything that
			// only prints the error reads exactly what it read before.
			return fmt.Errorf("%s %s: %w", method, path, &tuiUnsupportedEndpointError{msg: msg})
		}
		if method == http.MethodPost &&
			resp.StatusCode == http.StatusConflict &&
			payload.Code == "idempotency_unknown" {
			return fmt.Errorf("%s %s: %w", method, path, &tuiAmbiguousMutationError{
				err:      fmt.Errorf("daemon lost the recorded mutation outcome during restart: %s", msg),
				attempts: 1,
			})
		}
		return fmt.Errorf("%s %s: %s", method, path, msg)
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		if method == http.MethodPost {
			return fmt.Errorf("decode %s response: %w", path, &tuiAmbiguousMutationError{
				err:      err,
				attempts: 1,
			})
		}
		return fmt.Errorf("decode %s response: %w", path, err)
	}
	return nil
}

// tuiUnsupportedEndpointError means the daemon answered 404: it has no route
// for this operation at all, which for a standalone console usually means the
// far end is an older tclaude. It carries the same text the untyped error did,
// so it only matters to a caller that asks (tuiEndpointUnsupported) — one whose
// feature is optional and should go quiet rather than report a failure the
// operator can do nothing about.
type tuiUnsupportedEndpointError struct{ msg string }

func (e *tuiUnsupportedEndpointError) Error() string { return e.msg }

// tuiEndpointUnsupported reports whether err is a daemon that does not have
// the endpoint, as opposed to one that has it and failed.
func tuiEndpointUnsupported(err error) bool {
	var unsupported *tuiUnsupportedEndpointError
	return errors.As(err, &unsupported)
}

// tuiAmbiguousMutationError means every safe retry of one idempotency key
// failed before a response arrived. The daemon may have committed it; callers
// must reconcile the listing before offering the operator another mutation.
type tuiAmbiguousMutationError struct {
	err      error
	attempts int
}

func (e *tuiAmbiguousMutationError) Error() string {
	return fmt.Sprintf("outcome unknown after %d attempts: %v", e.attempts, e.err)
}

func (e *tuiAmbiguousMutationError) Unwrap() error { return e.err }

func (a *remoteTUIAPI) doRequestBytes(req *http.Request) (*http.Response, []byte, error) {
	mutating := req.Method == http.MethodPost
	attempts := 0
	for {
		attempts++
		attempt, err := cloneTUIRequest(req)
		if err != nil {
			return nil, nil, err
		}
		resp, err := a.client.Do(attempt)
		if err == nil {
			raw, readErr := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if readErr == nil {
				return resp, raw, nil
			}
			err = fmt.Errorf("read response: %w", readErr)
		}
		retry := attempts - 1
		if !mutating || retry >= len(a.mutationRetryBackoff) {
			if mutating {
				return nil, nil, &tuiAmbiguousMutationError{err: err, attempts: attempts}
			}
			return nil, nil, err
		}
		timer := time.NewTimer(a.mutationRetryBackoff[retry])
		<-timer.C
	}
}

func cloneTUIRequest(req *http.Request) (*http.Request, error) {
	attempt := req.Clone(req.Context())
	if req.Body == nil {
		return attempt, nil
	}
	if req.GetBody == nil {
		return nil, fmt.Errorf("request body cannot be replayed")
	}
	body, err := req.GetBody()
	if err != nil {
		return nil, fmt.Errorf("replay request body: %w", err)
	}
	attempt.Body = body
	return attempt, nil
}
