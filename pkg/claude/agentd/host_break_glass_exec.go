package agentd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

type breakGlassExecRequest struct {
	Args    []string `json:"args"`
	Shell   string   `json:"shell"`
	Cwd     string   `json:"cwd"`
	Env     []string `json:"env"`
	Timeout string   `json:"timeout"`
	Reason  string   `json:"reason"`
	Stdin   bool     `json:"stdin"`
}

type breakGlassFrame struct {
	Stream   string `json:"stream"`
	Data     []byte `json:"data,omitempty"`
	ExitCode *int   `json:"exit_code,omitempty"`
	Error    string `json:"error,omitempty"`
}

func handleBreakGlassExec(w http.ResponseWriter, r *http.Request) {
	if peerActionFromRequest(r) != nil {
		writeError(w, 403, "local_only", "break-glass is local only")
		return
	}
	p := peerFromContext(r.Context())
	if classify(p) != classAgent {
		writeError(w, 403, "auth", "host execution requires an identified agent with a live operator grant")
		return
	}
	state, err := db.AgentState(p.ConvID)
	if err != nil || state == db.AgentStateRetired {
		writeError(w, 403, "auth", "agent is not active")
		return
	}
	grants, err := liveBreakGlassGrants(p.ConvID)
	if err != nil {
		writeError(w, 500, "io", err.Error())
		return
	}
	if len(grants) == 0 {
		writeError(w, 403, "operator_grant_required", "no live break-glass grant; agents cannot request this capability")
		return
	}
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return r.Header.Get("Origin") == "" }}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	conn.SetReadLimit(1 << 20)
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	var spec breakGlassExecRequest
	if err = conn.ReadJSON(&spec); err != nil {
		return
	}
	_ = conn.SetReadDeadline(time.Time{})
	if err = validateBreakGlassExec(spec); err != nil {
		_ = conn.WriteJSON(breakGlassFrame{Stream: "error", Error: err.Error()})
		return
	}
	dur := 10 * time.Minute
	if spec.Timeout != "" {
		dur, _ = time.ParseDuration(spec.Timeout)
	}
	ctx, cancel := context.WithTimeout(r.Context(), dur)
	defer cancel()
	// Expiry and revocation end existing runs as well as denying new ones.
	go func() {
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				live, e := liveBreakGlassGrants(p.ConvID)
				if e != nil || len(live) == 0 {
					cancel()
					return
				}
			}
		}
	}()

	args := spec.Args
	if spec.Shell != "" {
		args = []string{"/bin/sh", "-c", spec.Shell}
	}
	cmd, waitProcess, cleanupProcess, err := newBreakGlassProcess(ctx, args)
	if err != nil {
		_ = conn.WriteJSON(breakGlassFrame{Stream: "error", Error: err.Error()})
		return
	}
	defer cleanupProcess()
	if spec.Cwd == "" {
		spec.Cwd, err = os.Getwd()
		if err != nil {
			_ = conn.WriteJSON(breakGlassFrame{Stream: "error", Error: err.Error()})
			return
		}
	}
	cmd.Dir = spec.Cwd
	cmd.Env = append(os.Environ(), spec.Env...)
	// Open the operator's log before executing anything; logging failure is fatal.
	logDir := filepath.Join(config.DataDir(), "break-glass")
	if err = os.MkdirAll(logDir, 0700); err != nil {
		_ = conn.WriteJSON(breakGlassFrame{Stream: "error", Error: err.Error()})
		return
	}
	logFile, err := os.CreateTemp(logDir, "run-*.jsonl")
	if err != nil {
		_ = conn.WriteJSON(breakGlassFrame{Stream: "error", Error: err.Error()})
		return
	}
	defer logFile.Close()
	started := time.Now()
	code := 1
	defer func() {
		detail, _ := json.Marshal(map[string]any{"command": clipBreakGlassText(strings.Join(args, " "), 4096), "cwd": clipBreakGlassText(spec.Cwd, 4096), "reason": clipBreakGlassText(spec.Reason, 4096), "exit_code": code, "duration_ms": time.Since(started).Milliseconds(), "log": filepath.Base(logFile.Name())})
		agentID, _ := db.AgentIDForConv(p.ConvID)
		_, _ = db.InsertAuditLog(db.AuditLogEntry{At: started, ActorKind: db.AuditActorAgent, ActorConv: p.ConvID, ActorAgent: agentID, Verb: "break-glass.exec", Detail: string(detail), Method: r.Method, Path: r.URL.Path, Status: 200, Source: db.AuditSourceCLI})

	}()
	logEncoder := json.NewEncoder(logFile)
	if err = logEncoder.Encode(map[string]any{"started": started, "conv_id": p.ConvID, "grant_id": grants[0].ID, "command": args, "cwd": spec.Cwd, "reason": spec.Reason}); err != nil {
		_ = conn.WriteJSON(breakGlassFrame{Stream: "error", Error: err.Error()})
		return
	}
	var mu sync.Mutex
	send := func(frame breakGlassFrame) error {
		mu.Lock()
		defer mu.Unlock()
		if e := logEncoder.Encode(frame); e != nil {
			cancel()
			return e
		}
		_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if e := conn.WriteJSON(frame); e != nil {
			cancel()
			return e
		}
		return nil
	}
	cmd.Stdout = breakGlassOutput{stream: "stdout", send: send}
	cmd.Stderr = breakGlassOutput{stream: "stderr", send: send}
	var input io.WriteCloser
	if spec.Stdin {
		input, err = cmd.StdinPipe()
		if err != nil {
			_ = send(breakGlassFrame{Stream: "error", Error: err.Error()})
			return
		}
		defer input.Close()
	}
	if err = cmd.Start(); err != nil {
		_ = send(breakGlassFrame{Stream: "error", Error: err.Error()})
		return
	}
	go func() {
		defer cancel()
		for {
			var frame breakGlassFrame
			if e := conn.ReadJSON(&frame); e != nil {
				return
			}
			if frame.Stream == "stdin" && input != nil {
				if _, e := input.Write(frame.Data); e != nil {
					return
				}
			}
			if frame.Stream == "eof" && input != nil {
				_ = input.Close()
			}
		}
	}()
	code, err = waitProcess()
	if err != nil && code == 0 {
		code = 1
	}
	frame := breakGlassFrame{Stream: "exit", ExitCode: &code}
	if err != nil {
		frame.Error = err.Error()
	}

	_ = send(frame)
}

func validateBreakGlassExec(spec breakGlassExecRequest) error {
	if (len(spec.Args) == 0) == (spec.Shell == "") {
		return fmt.Errorf("provide command arguments or a shell string")
	}
	if spec.Timeout != "" {
		d, err := time.ParseDuration(spec.Timeout)
		if err != nil || d <= 0 {
			return fmt.Errorf("timeout must be a positive Go duration")
		}
	}
	for _, entry := range spec.Env {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || key == "" || strings.ContainsRune(entry, 0) {
			return fmt.Errorf("environment entries must be K=V without NUL")
		}
	}
	return nil
}

func clipBreakGlassText(s string, limit int) string {
	if len(s) > limit {
		return s[:limit]
	}
	return s
}

type breakGlassOutput struct {
	stream string
	send   func(breakGlassFrame) error
}

func (out breakGlassOutput) Write(p []byte) (int, error) {
	for off := 0; off < len(p); {
		end := min(off+16384, len(p))
		if err := out.send(breakGlassFrame{Stream: out.stream, Data: p[off:end]}); err != nil {
			return off, err
		}
		off = end
	}
	return len(p), nil
}
