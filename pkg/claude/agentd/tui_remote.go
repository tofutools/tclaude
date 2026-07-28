package agentd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/common"
)

const tuiHTTPPrefix = "/api/tui"

type tuiDashboardParams struct {
	ConnectTo string `long:"connect-to" required:"true" help:"Dashboard URL or host[:port] to connect to (for example 10.0.0.4:8321 or https://agents.example.com)."`
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
		Long:        "Runs the terminal dashboard as a standalone HTTP client. It authenticates with the same operator token as `tclaude agent` commands and keeps polling through agentd restarts.",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *tuiDashboardParams, _ *cobra.Command, _ []string) {
			if err := runRemoteTUIDashboard(p.ConnectTo); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
		},
	}.ToCobra()
}

func runRemoteTUIDashboard(target string) error {
	token := agent.OperatorToken()
	if token == "" {
		return fmt.Errorf("no operator token available; export %s from the agentd startup banner and retry",
			agent.HumanTokenEnvVar)
	}
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
	return tuiCapabilities{}
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
