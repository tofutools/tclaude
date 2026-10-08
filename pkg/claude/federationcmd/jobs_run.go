package federationcmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/jobrepo"
	"github.com/tofutools/tclaude/pkg/federation/jobstream"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

type jobOutput struct {
	StdoutBytes []byte `json:"stdout_bytes,omitempty"`
	StderrBytes []byte `json:"stderr_bytes,omitempty"`
	Stdout      string `json:"stdout"`
	Stderr      string `json:"stderr"`
	ExitCode    int    `json:"exit_code"`
}
type jobSummary struct {
	Peer   string    `json:"peer"`
	ID     string    `json:"id"`
	Commit string    `json:"commit"`
	State  string    `json:"state"`
	Code   int       `json:"exit_code"`
	Error  string    `json:"error,omitempty"`
	Output jobOutput `json:"output"`
}

func runFederationJobs(p *jobRunParams) int {
	if rc := agent.RequireDaemonOrExit(os.Stderr); rc != 0 {
		return rc
	}
	if len(p.Node) == 0 {
		fmt.Fprintln(os.Stderr, "select --node <peer>, --node auto, or --node group:<pool>")
		return 1
	}
	if p.Follow && p.JSON {
		fmt.Fprintln(os.Stderr, "--follow and --json cannot be combined")
		return 1
	}
	if len(p.Node) > 1 && !jobrepo.FullCommit(p.Ref) {
		fmt.Fprintln(os.Stderr, "multi-node jobs require --ref with a full commit SHA; resolve a branch with e.g. git rev-parse origin/<branch>")
		return 1
	}
	body := map[string]any{"repo": p.Repo, "ref": p.Ref, "group": p.Group, "harness": p.Harness, "command": p.Command, "timeout_seconds": p.Timeout, "require": p.Require, "prefer": p.Prefer}
	var submitted struct {
		Job     db.FederationJob `json:"job"`
		Results []struct {
			Job    db.FederationJob `json:"job"`
			Peer   string           `json:"peer"`
			Status int              `json:"status"`
			Error  any              `json:"error"`
		} `json:"results"`
	}
	if len(p.Node) == 1 {
		body["node"] = p.Node[0]
	} else {
		body["nodes"] = p.Node
	}
	if e := agent.DaemonRequest(http.MethodPost, "/v1/federation/jobs", body, &submitted, agent.DaemonOpts{}); e != nil {
		return fail(os.Stderr, e)
	}
	summaries := make([]jobSummary, max(1, len(submitted.Results)))
	var wg sync.WaitGroup
	var outputMu sync.Mutex
	launch := func(i int, j db.FederationJob) {
		wg.Add(1)
		go func() { defer wg.Done(); summaries[i] = waitFederationJob(p, j, &outputMu, len(summaries) > 1) }()
	}
	if len(submitted.Results) == 0 {
		launch(0, submitted.Job)
	} else {
		for i, r := range submitted.Results {
			if r.Status != 200 || r.Job.ID == "" {
				summaries[i] = jobSummary{Peer: r.Peer, State: "submission_failed", Code: 1, Error: fmt.Sprint(r.Error)}
			} else {
				launch(i, r.Job)
			}
		}
	}
	wg.Wait()
	code := 0
	for _, s := range summaries {
		if s.Code != 0 {
			code = 1
		}
	}
	if p.JSON {
		if len(summaries) == 1 {
			_ = printJSON(os.Stdout, summaries[0])
		} else {
			_ = printJSON(os.Stdout, map[string]any{"nodes": summaries})
		}
	}
	if len(summaries) > 1 && !p.JSON {
		for _, s := range summaries {
			fmt.Fprintf(os.Stderr, "%s: %s commit=%s exit=%d job=%s\n", s.Peer, s.State, s.Commit, s.Code, s.ID)
			if s.Error != "" {
				fmt.Fprintln(os.Stderr, proto.StripControls(s.Error))
			}
		}
	}
	if len(summaries) == 1 {
		return summaries[0].Code
	}
	return code
}
func waitFederationJob(p *jobRunParams, initial db.FederationJob, mu *sync.Mutex, multi bool) jobSummary {
	result := jobSummary{ID: initial.ID, Peer: initial.Peer, Code: 1}
	mu.Lock()
	fmt.Fprintln(os.Stderr, "Job", initial.ID, "node", initial.Peer)
	mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(p.Timeout)*time.Second+6*time.Minute)
	defer cancel()
	cursor := jobstream.Cursor{}
	stdout := &safeJobWriter{out: os.Stdout, mu: mu}
	stderr := &safeJobWriter{out: os.Stderr, mu: mu}
	if multi {
		stdout.prefix = "[" + initial.Peer + " stdout] "
		stderr.prefix = "[" + initial.Peer + " stderr] "
	}
	// The polling goroutine owns lifecycle/exit. Follow only owns the cursors;
	// cancellation joins it before completed output fills any remaining tail.
	done := make(chan struct{})
	if p.Follow {
		go func() { defer close(done); followFederationJob(ctx, initial.ID, &cursor, stdout, stderr) }()
	} else {
		close(done)
	}
	defer func() { cancel(); <-done; stdout.Flush(); stderr.Flush() }()
	for {
		var j db.FederationJob
		if e := agent.DaemonGet("/v1/federation/jobs/"+initial.ID, &j); e != nil {
			result.Error = e.Error()
			return result
		}
		result.State = j.State
		if j.State == "unknown" {
			result.Error = "execution uncertain; inspect or cancel this job"
			return result
		}
		switch j.State {
		case "completed", "failed", "canceled", "timeout", "refused", "interrupted", "output_unavailable":
			cancel()
			<-done
			var receipt proto.JobResult
			_ = json.Unmarshal(j.Result, &receipt)
			result.Commit = receipt.Commit
			result.Code = receipt.ExitCode
			result.Error = receipt.Code
			if len(receipt.Logs) > 0 {
				if e := agent.DaemonGet("/v1/federation/jobs/"+j.ID+"/logs", &result.Output); e != nil {
					result.Code = 1
					result.Error = e.Error()
					return result
				}
			}
			if result.Output.StdoutBytes != nil {
				result.Output.Stdout = string(result.Output.StdoutBytes)
			}
			if result.Output.StderrBytes != nil {
				result.Output.Stderr = string(result.Output.StderrBytes)
			}
			if j.State != "completed" && result.Code == 0 {
				result.Code = 1
			}
			if !p.JSON {
				// Frames replay from zero, so completed bytes after the cursor are the
				// only missing tail. A malformed cursor never slices beyond the artifact.
				if cursor.Stdout > uint64(len(result.Output.Stdout)) || cursor.Stderr > uint64(len(result.Output.Stderr)) {
					result.Code = 1
					result.Error = "live output exceeds completed output"
					return result
				}
				_, _ = stdout.Write([]byte(result.Output.Stdout[cursor.Stdout:]))
				_, _ = stderr.Write([]byte(result.Output.Stderr[cursor.Stderr:]))
				if receipt.Code != "" {
					mu.Lock()
					fmt.Fprintln(os.Stderr, proto.StripControls(receipt.Code))
					mu.Unlock()
				}
			}
			return result
		}
		select {
		case <-ctx.Done():
			result.Error = "job status uncertain; inspect or cancel this job"
			return result
		case <-time.After(time.Second):
		}
	}
}
func followFederationJob(ctx context.Context, id string, cursor *jobstream.Cursor, stdout, stderr io.Writer) {
	for ctx.Err() == nil {
		conn, e := agent.DaemonStreamGet(ctx, "/v1/federation/jobs/"+id+"/follow")
		if e == nil {
			for {
				frame, err := jobstream.Read(conn)
				if err != nil {
					break
				}
				if cursor.Apply(frame, stdout, stderr) != nil {
					_ = conn.Close()
					return
				}
			}
			_ = conn.Close()
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

// Hold partial UTF-8 sequences across chunks and strip terminal controls before
// printing. A fan-out prefix is local metadata, never taken from worker output.
type safeJobWriter struct {
	out     io.Writer
	mu      *sync.Mutex
	prefix  string
	pending []byte
}

func (w *safeJobWriter) Write(p []byte) (int, error) {
	original := len(p)
	w.pending = append(w.pending, p...)
	n := 0
	for n < len(w.pending) {
		if !utf8.FullRune(w.pending[n:]) {
			break
		}
		_, size := utf8.DecodeRune(w.pending[n:])
		n += size
	}
	if n > 0 {
		w.emit(w.pending[:n])
		w.pending = append(w.pending[:0], w.pending[n:]...)
	}
	return original, nil
}
func (w *safeJobWriter) emit(b []byte) {
	text := proto.StripControls(string(b))
	if text == "" {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.prefix != "" {
		_, _ = fmt.Fprint(w.out, w.prefix)
	}
	_, _ = fmt.Fprint(w.out, text)
}
func (w *safeJobWriter) Flush() { w.emit(w.pending); w.pending = nil }
