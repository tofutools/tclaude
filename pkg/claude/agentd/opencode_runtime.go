package agentd

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

const (
	openCodeServerUsername = "opencode"
	openCodeStartupTimeout = 12 * time.Second
)

type openCodeLaunch struct {
	SessionID string
	ConvID    string
	ServerURL string
	Password  string
}

type openCodeProcess struct {
	cmd    *exec.Cmd
	done   chan error
	cancel context.CancelFunc
}

var openCodeProcesses = struct {
	sync.Mutex
	bySession map[string]*openCodeProcess
}{bySession: map[string]*openCodeProcess{}}

var openCodeHTTPClient = &http.Client{Timeout: 5 * time.Second}

// These seams keep flow tests independent of a locally-installed OpenCode
// binary while still exercising executeSpawn's production orchestration.
var (
	startOpenCodeRuntimeForSpawn = startOpenCodeRuntime
	sendOpenCodePromptForSpawn   = sendOpenCodePrompt
)

func startOpenCodeRuntime(sessionID, cwd, title, resumeID string) (*openCodeLaunch, error) {
	existing, err := db.GetOpenCodeRuntime(sessionID)
	if err != nil {
		return nil, fmt.Errorf("look up OpenCode runtime: %w", err)
	}
	if existing != nil {
		if openCodeHealthy(*existing) {
			ensureOpenCodeSSE(*existing)
			return &openCodeLaunch{
				SessionID: sessionID, ConvID: existing.ConvID,
				ServerURL: existing.ServerURL, Password: existing.Password,
			}, nil
		}
		_ = stopOpenCodeRuntime(sessionID)
	}
	// A fresh launch is keyed by its temporary tclaude session label because
	// the server has not minted the conversation ID yet. A later resume is
	// keyed by the durable `ses_…` ID. Retire the old label-keyed runtime
	// before starting the resume server so an immediate stop→resume never has
	// two authoritative servers for the same conversation.
	if resumeID != "" {
		if prior, err := db.GetOpenCodeRuntimeByConvID(resumeID); err != nil {
			return nil, fmt.Errorf("look up prior OpenCode runtime: %w", err)
		} else if prior != nil && prior.SessionID != sessionID {
			if err := stopOpenCodeRuntime(prior.SessionID); err != nil {
				return nil, fmt.Errorf("retire prior OpenCode runtime: %w", err)
			}
		}
	}

	password, err := randomOpenCodePassword()
	if err != nil {
		return nil, err
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		serverURL, err := allocateOpenCodeServerURL()
		if err != nil {
			return nil, err
		}
		runtime := db.OpenCodeRuntime{
			SessionID: sessionID,
			ConvID:    resumeID,
			ServerURL: serverURL,
			Password:  password,
			Cwd:       cwd,
		}
		process, err := startOpenCodeProcess(runtime)
		if err != nil {
			lastErr = err
			continue
		}
		runtime.PID = process.cmd.Process.Pid
		if err := db.UpsertOpenCodeRuntime(runtime); err != nil {
			stopOpenCodeProcess(runtime, process)
			return nil, fmt.Errorf("persist OpenCode runtime: %w", err)
		}
		if runtime.ConvID == "" {
			runtime.ConvID, err = createOpenCodeSession(runtime, title)
			if err != nil {
				_ = stopOpenCodeRuntime(sessionID)
				return nil, err
			}
			if err := db.UpsertOpenCodeRuntime(runtime); err != nil {
				_ = stopOpenCodeRuntime(sessionID)
				return nil, fmt.Errorf("persist OpenCode conversation id: %w", err)
			}
		}
		ensureOpenCodeSSE(runtime)
		return &openCodeLaunch{
			SessionID: sessionID, ConvID: runtime.ConvID,
			ServerURL: runtime.ServerURL, Password: runtime.Password,
		}, nil
	}
	return nil, fmt.Errorf("start OpenCode server after 3 port attempts: %w", lastErr)
}

func startOpenCodeProcess(runtime db.OpenCodeRuntime) (*openCodeProcess, error) {
	executable, err := harness.OpenCodeExecutable()
	if err != nil {
		return nil, fmt.Errorf("find OpenCode executable: %w", err)
	}
	parsed, err := url.Parse(runtime.ServerURL)
	if err != nil {
		return nil, err
	}
	_, port, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		return nil, fmt.Errorf("parse OpenCode server endpoint: %w", err)
	}
	cmd := exec.Command(executable, "serve",
		"--hostname", "127.0.0.1", "--port", port, "--log-level", "ERROR")
	cmd.Dir = runtime.Cwd
	cmd.Env = append(os.Environ(),
		"OPENCODE_SERVER_USERNAME="+openCodeServerUsername,
		"OPENCODE_SERVER_PASSWORD="+runtime.Password)
	cmd.Stdout = io.Discard
	stderr := newSpawnStderrCapture()
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	process := &openCodeProcess{cmd: cmd, done: make(chan error, 1)}
	openCodeProcesses.Lock()
	openCodeProcesses.bySession[runtime.SessionID] = process
	openCodeProcesses.Unlock()
	go func() {
		err := cmd.Wait()
		process.done <- err
		close(process.done)
		if err != nil {
			slog.Warn("OpenCode server exited", "session", runtime.SessionID,
				"pid", cmd.Process.Pid, "error", err, "stderr", stderr.String(),
				"stderr_truncated", stderr.Truncated())
		}
	}()
	deadline := time.Now().Add(openCodeStartupTimeout)
	for time.Now().Before(deadline) {
		if openCodeHealthy(db.OpenCodeRuntime{
			ServerURL: runtime.ServerURL, Password: runtime.Password,
		}) {
			return process, nil
		}
		select {
		case err := <-process.done:
			if err == nil {
				err = fmt.Errorf("server exited before health check")
			}
			return nil, fmt.Errorf("OpenCode server failed during startup: %w: %s", err, stderr.String())
		case <-time.After(100 * time.Millisecond):
		}
	}
	stopOpenCodeProcess(runtime, process)
	return nil, fmt.Errorf("OpenCode server at %s did not become healthy within %s",
		runtime.ServerURL, openCodeStartupTimeout)
}

func allocateOpenCodeServerURL() (string, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	address := listener.Addr().(*net.TCPAddr)
	if err := listener.Close(); err != nil {
		return "", err
	}
	return "http://127.0.0.1:" + strconv.Itoa(address.Port), nil
}

func randomOpenCodePassword() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("mint OpenCode server password: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

func openCodeRequest(method, endpoint, password string, body any) (*http.Request, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, endpoint, reader)
	if err != nil {
		return nil, err
	}
	request.SetBasicAuth(openCodeServerUsername, password)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	return request, nil
}

func openCodeHealthy(runtime db.OpenCodeRuntime) bool {
	request, err := openCodeRequest(http.MethodGet,
		runtime.ServerURL+"/global/health", runtime.Password, nil)
	if err != nil {
		return false
	}
	response, err := openCodeHTTPClient.Do(request)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	return response.StatusCode == http.StatusOK
}

func createOpenCodeSession(runtime db.OpenCodeRuntime, title string) (string, error) {
	body := map[string]string{}
	if strings.TrimSpace(title) != "" {
		body["title"] = title
	}
	endpoint := runtime.ServerURL + "/session?directory=" + url.QueryEscape(runtime.Cwd)
	request, err := openCodeRequest(http.MethodPost, endpoint, runtime.Password, body)
	if err != nil {
		return "", err
	}
	response, err := openCodeHTTPClient.Do(request)
	if err != nil {
		return "", fmt.Errorf("create OpenCode session: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("create OpenCode session: HTTP %d", response.StatusCode)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&created); err != nil {
		return "", fmt.Errorf("decode OpenCode session: %w", err)
	}
	if !strings.HasPrefix(created.ID, "ses_") {
		return "", fmt.Errorf("create OpenCode session returned invalid id %q", created.ID)
	}
	return created.ID, nil
}

func sendOpenCodePrompt(launch *openCodeLaunch, cwd, prompt, model, effort string) error {
	if launch == nil || prompt == "" {
		return nil
	}
	body := map[string]any{
		"parts": []map[string]string{{"type": "text", "text": prompt}},
	}
	if provider, modelID, ok := strings.Cut(model, "/"); ok && provider != "" && modelID != "" {
		body["model"] = map[string]string{"providerID": provider, "modelID": modelID}
	}
	if effort != "" {
		body["variant"] = effort
	}
	endpoint := launch.ServerURL + "/session/" + url.PathEscape(launch.ConvID) +
		"/prompt_async?directory=" + url.QueryEscape(cwd)
	request, err := openCodeRequest(http.MethodPost, endpoint, launch.Password, body)
	if err != nil {
		return err
	}
	response, err := openCodeHTTPClient.Do(request)
	if err != nil {
		return fmt.Errorf("submit OpenCode launch prompt: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		return fmt.Errorf("submit OpenCode launch prompt: HTTP %d", response.StatusCode)
	}
	return nil
}

// reconcileOpenCodeRuntime is the server-side half of OpenCode liveness.
// A healthy pane is insufficient: the conversation lives in `serve`. Restart
// the same authenticated endpoint when possible so the attached client can
// reconnect; return false when recovery failed and the reaper should fail the
// pane visibly.
func reconcileOpenCodeRuntime(sessionID string) bool {
	runtime, err := db.GetOpenCodeRuntime(sessionID)
	if err != nil || runtime == nil {
		return false
	}
	if openCodeHealthy(*runtime) {
		ensureOpenCodeSSE(*runtime)
		return true
	}
	stopOpenCodeProcess(*runtime, nil)
	process, err := startOpenCodeProcess(*runtime)
	if err != nil {
		slog.Error("OpenCode server restart failed", "session", sessionID, "error", err)
		return false
	}
	runtime.PID = process.cmd.Process.Pid
	if err := db.UpsertOpenCodeRuntime(*runtime); err != nil {
		slog.Error("OpenCode server restart state could not be persisted",
			"session", sessionID, "error", err)
		stopOpenCodeProcess(*runtime, process)
		return false
	}
	ensureOpenCodeSSE(*runtime)
	return true
}

func stopOpenCodeRuntime(sessionID string) error {
	runtime, err := db.GetOpenCodeRuntime(sessionID)
	if err != nil {
		return err
	}
	if runtime == nil {
		return nil
	}
	stopOpenCodeProcess(*runtime, nil)
	return db.DeleteOpenCodeRuntime(sessionID)
}

func stopOpenCodeProcess(runtime db.OpenCodeRuntime, known *openCodeProcess) {
	openCodeProcesses.Lock()
	process := known
	if process == nil {
		process = openCodeProcesses.bySession[runtime.SessionID]
	}
	delete(openCodeProcesses.bySession, runtime.SessionID)
	openCodeProcesses.Unlock()
	if process != nil {
		if process.cancel != nil {
			process.cancel()
		}
		if process.cmd != nil && process.cmd.Process != nil {
			_ = process.cmd.Process.Signal(os.Interrupt)
			select {
			case <-process.done:
				return
			case <-time.After(2 * time.Second):
				_ = process.cmd.Process.Kill()
				return
			}
		}
	}
	if runtime.PID > 1 && runtime.PID != os.Getpid() {
		if recovered, err := os.FindProcess(runtime.PID); err == nil {
			// TODO(TCL-668): verify process start-time/argv before killing a
			// recovered PID to close the unlikely PID-reuse window.
			_ = recovered.Kill()
		}
	}
}

func ensureOpenCodeSSE(runtime db.OpenCodeRuntime) {
	openCodeProcesses.Lock()
	process := openCodeProcesses.bySession[runtime.SessionID]
	if process == nil {
		process = &openCodeProcess{}
		openCodeProcesses.bySession[runtime.SessionID] = process
	}
	if process.cancel != nil {
		openCodeProcesses.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	process.cancel = cancel
	openCodeProcesses.Unlock()
	go consumeOpenCodeSSE(ctx, runtime)
}

// consumeOpenCodeSSE lands the authenticated transport/reconnect plumbing.
// TCL-672 will map event names onto tclaude's status model; this slice only
// proves the managed topology has one durable consumer per server.
func consumeOpenCodeSSE(ctx context.Context, runtime db.OpenCodeRuntime) {
	endpoint := runtime.ServerURL + "/event?directory=" + url.QueryEscape(runtime.Cwd)
	for ctx.Err() == nil {
		request, err := openCodeRequest(http.MethodGet, endpoint, runtime.Password, nil)
		if err == nil {
			request = request.WithContext(ctx)
			var response *http.Response
			response, err = http.DefaultClient.Do(request)
			if err == nil && response.StatusCode == http.StatusOK {
				scanner := bufio.NewScanner(response.Body)
				for scanner.Scan() {
					line := scanner.Text()
					if strings.HasPrefix(line, "data:") {
						consumeOpenCodeEvent(runtime.SessionID,
							json.RawMessage(strings.TrimSpace(strings.TrimPrefix(line, "data:"))))
					}
				}
				_ = response.Body.Close()
				err = scanner.Err()
			} else if response != nil {
				_ = response.Body.Close()
			}
		}
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			slog.Debug("OpenCode SSE disconnected; retrying",
				"session", runtime.SessionID, "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

func consumeOpenCodeEvent(sessionID string, event json.RawMessage) {
	var envelope struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(event, &envelope) == nil && envelope.Type != "" {
		slog.Debug("OpenCode SSE event", "session", sessionID, "type", envelope.Type)
	}
}

func reapOrphanedOpenCodeRuntimes(states []*session.SessionState) {
	known := make(map[string]bool, len(states))
	for _, state := range states {
		if state.Harness == harness.OpenCodeName && state.Status != session.StatusExited {
			known[state.ID] = true
		}
	}
	runtimes, err := db.ListOpenCodeRuntimes()
	if err != nil {
		slog.Warn("OpenCode runtime orphan scan failed", "error", err)
		return
	}
	for _, runtime := range runtimes {
		if !known[runtime.SessionID] {
			if !runtime.CreatedAt.IsZero() &&
				time.Since(runtime.CreatedAt) < sessionReaperGracePeriod {
				continue
			}
			if err := stopOpenCodeRuntime(runtime.SessionID); err != nil {
				slog.Warn("OpenCode orphan runtime cleanup failed",
					"session", runtime.SessionID, "error", err)
			}
		}
	}
}
