package agentd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"runtime/debug"
	"strings"

	tea "charm.land/bubbletea/v2"
)

// inProcessTUIAPI issues requests against the daemon's own /v1 mux from inside the
// daemon process. The console is a client like any other: it reaches the
// real handlers, with their validation, permission checks and audit trail,
// rather than calling spawn/DB internals directly.
//
// Identity: withIdentity normally resolves the peer from the connecting Unix
// socket, which does not exist for an in-process call, so the console builds
// the peer itself — but it asserts nothing. It presents the live operator
// token and lets the real verifier decide (verifyHumanToken, constant-time),
// and it resolves its own harness ancestry through the same convIDForPID walk
// the socket path uses. classify() then applies its ordinary precedence.
//
// That precedence matters here. A daemon launched from inside a harness pane
// has a harness ancestor, and classify deliberately lets that beat any
// operator token — otherwise an agent whose shell inherited TCLAUDE_HUMAN_TOKEN
// could promote itself. Claiming "the console is always the human" would have
// re-opened exactly that hole through a console the owning agent can drive
// with tmux send-keys. So `agentd serve --tui` started from an agent's pane
// gets an agent-class console, scoped and permission-gated like that agent;
// started from the operator's own shell it gets the human.
type inProcessTUIAPI struct {
	handler http.Handler
	// pid and the resolved ancestry are fixed for the daemon's lifetime, so
	// the process-tree walk runs once here rather than on every 2s poll.
	pid                int
	convID             string
	hasHarnessAncestor bool
}

func newInProcessTUIAPI() *inProcessTUIAPI {
	// A second mux instance: buildMux only registers package-level handlers
	// and holds no per-mux state, so this shares every code path with the
	// socket server without sharing its identity middleware (which would
	// overwrite the peer stamped below).
	pid := os.Getpid()
	convID, hasAncestor := convIDForPID(pid)
	return &inProcessTUIAPI{
		handler:            buildTUIConsoleMux(),
		pid:                pid,
		convID:             convID,
		hasHarnessAncestor: hasAncestor,
	}
}

// callerClass is how the daemon classifies this console — the same verdict
// its requests get, computed once so the UI can say up front what the operator
// is working as.
func (a *inProcessTUIAPI) callerClass() callerClass {
	return classify(&peer{
		PID:               a.pid,
		ConvID:            a.convID,
		HasClaudeAncestor: a.hasHarnessAncestor,
		// The console always presents the live token, so it is valid
		// whenever the daemon managed to mint one.
		HumanTokenValid: currentOperatorToken() != "",
	})
}

// identityWarning explains, in one line, a console the daemon will not treat
// as the operator — empty when it will. Without it, a console started from
// the wrong place just answers every keystroke with a bare 403 and no hint
// about why or what to do instead.
func (a *inProcessTUIAPI) identityWarning() string {
	switch a.callerClass() {
	case classHuman:
		return ""
	case classAgent:
		return "Note: agentd was started from inside a harness session, so this console acts as agent " +
			a.convID + " — listings are scoped to its groups and spawns need its permissions."
	case classAgentUnknown:
		return "Note: agentd was started from inside a harness session whose conv-id cannot be resolved, " +
			"so the daemon refuses this console. Restart agentd from a plain shell to use --tui."
	default:
		return "Note: no operator token is available, so the daemon refuses this console."
	}
}

func (a *inProcessTUIAPI) isOperator() bool {
	return a.callerClass() == classHuman
}

func (a *inProcessTUIAPI) connectionLabel() string {
	return "in-process"
}

func (a *inProcessTUIAPI) capabilities() tuiCapabilities {
	return tuiCapabilities{
		attachAgent:      true,
		attachLocalPane:  true,
		startLocalShell:  true,
		localSessions:    true,
		completeLocalDir: true,
		shutdownOnQuit:   true,
	}
}

func (a *inProcessTUIAPI) attach(agentName, convID, tmuxSession string) tea.Cmd {
	if tmuxSession == "" {
		if sess := pickAliveSession(convID); sess != nil {
			tmuxSession = sess.TmuxSession
		}
	}
	if tmuxSession == "" {
		return func() tea.Msg {
			return tuiAttachedMsg{
				agent: agentName,
				err:   fmt.Errorf("%s has no live tmux session to attach to", agentName),
			}
		}
	}
	return tuiAttachToPane(agentName, tmuxSession, insideTmux())
}

func (a *inProcessTUIAPI) get(path string, out any) error {
	return a.do(http.MethodGet, path, nil, out)
}

func (a *inProcessTUIAPI) post(path string, in, out any) error {
	return a.do(http.MethodPost, path, in, out)
}

func (a *inProcessTUIAPI) do(method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, "http://agentd"+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set(humanTokenHeader, currentOperatorToken())
	req = req.WithContext(context.WithValue(req.Context(), peerKey{}, &peer{
		PID:               a.pid,
		ConvID:            a.convID,
		HasClaudeAncestor: a.hasHarnessAncestor,
		HumanTokenValid:   verifyHumanToken(req),
	}))

	rec := &tuiResponseRecorder{code: http.StatusOK}
	if err := serveTUIRequest(a.handler, rec, req); err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	if rec.code == http.StatusNotFound {
		// Same disposition the remote client gives a 404: the daemon has no
		// such operation, which an optional readout should go quiet about
		// rather than report as a failure.
		return fmt.Errorf("%s %s: %w", method, path, &tuiUnsupportedEndpointError{msg: tuiErrorMessage(rec)})
	}
	if rec.code >= http.StatusBadRequest {
		return fmt.Errorf("%s %s: %s", method, path, tuiErrorMessage(rec))
	}
	if out == nil || rec.body.Len() == 0 {
		return nil
	}
	if err := json.Unmarshal(rec.body.Bytes(), out); err != nil {
		return fmt.Errorf("decode %s response: %w", path, err)
	}
	return nil
}

// serveTUIRequest dispatches one request into the mux and turns a handler
// panic into an error instead of letting it escape.
//
// This restores what the socket path gets for free: net/http recovers a
// panicking handler and loses only that connection. An in-process call has no
// such net, and the console calls this from a bubbletea command goroutine, so
// an unrecovered panic in a handler would take down the whole daemon — and
// leave the terminal in alt-screen/raw mode on the way out. The console polls
// /v1/peers every two seconds, so this is not a corner of the code that is
// rarely reached.
func serveTUIRequest(h http.Handler, w http.ResponseWriter, r *http.Request) (err error) {
	defer func() {
		if rec := recover(); rec != nil {
			// Same disposition net/http gives a panicking handler: log it
			// with the stack, fail this one request, keep serving.
			slog.Error("tui: handler panicked", "path", r.URL.Path, "panic", rec,
				"stack", string(debug.Stack()))
			err = fmt.Errorf("the daemon panicked handling this request: %v", rec)
		}
	}()
	h.ServeHTTP(w, r)
	return nil
}

// tuiErrorMessage renders a failed response for a one-line status bar: the
// daemon's own {"error":…} text when there is one, else the status code.
func tuiErrorMessage(rec *tuiResponseRecorder) string {
	var payload struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.body.Bytes(), &payload); err == nil && strings.TrimSpace(payload.Error) != "" {
		return payload.Error
	}
	return http.StatusText(rec.code)
}

// tuiResponseRecorder is the minimal http.ResponseWriter the in-process
// calls above write into.
type tuiResponseRecorder struct {
	header  http.Header
	code    int
	written bool
	body    bytes.Buffer
}

func (r *tuiResponseRecorder) Header() http.Header {
	if r.header == nil {
		r.header = http.Header{}
	}
	return r.header
}

func (r *tuiResponseRecorder) Write(b []byte) (int, error) {
	r.written = true
	return r.body.Write(b)
}

func (r *tuiResponseRecorder) WriteHeader(code int) {
	if r.written {
		return
	}
	r.written = true
	r.code = code
}
