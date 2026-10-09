package federationcmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/common"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/noderun"
)

type nodeRunParams struct {
	Log     string        `long:"log" help:"With --job, collect the full bounded stdout or stderr log"`
	Node    []string      `long:"node" help:"Repeatable peer label/ID; local selects this node"`
	All     bool          `long:"all" help:"Run locally and on every online trusted node; report offline nodes as skipped"`
	File    string        `long:"file" help:"Script file (16 KiB maximum); alternatively put a command after --"`
	Timeout time.Duration `long:"timeout" default:"1h" help:"Per-node deadline (1s..24h)"`
	NoWait  bool          `long:"no-wait" help:"Return durable job IDs immediately"`
	Job     string        `long:"job" help:"Inspect an existing job on exactly one node"`
	JSON    bool          `long:"json" help:"Output per-node JSON results"`
}

func nodeRunCmd() *cobra.Command {
	return boa.CmdT[nodeRunParams]{Use: "run", Short: "Run an operator script on selected nodes (full code execution)", Args: cobra.ArbitraryArgs, ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *nodeRunParams, cmd *cobra.Command, args []string) {
		if len(args) > 0 && cmd.ArgsLenAtDash() < 0 {
			fmt.Fprintln(os.Stderr, "commands must follow --")
			os.Exit(1)
		}
		os.Exit(runNodeRuns(p, args, os.Stdout, os.Stderr))
	}}.ToCobra()
}
func nodeRunPath(node, tail string) string {
	if node == "" || node == "local" {
		return "/v1/node/run" + tail
	}
	return "/v1/federation/peer/" + url.PathEscape(node) + "/node/run" + tail
}

type nodeRunOutcome struct {
	Log    []byte       `json:"log,omitempty"`
	Stream string       `json:"stream,omitempty"`
	Node   string       `json:"node"`
	State  string       `json:"state"`
	Job    *noderun.Job `json:"job,omitempty"`
	Error  string       `json:"error,omitempty"`
}

func runNodeRuns(p *nodeRunParams, args []string, stdout, stderr io.Writer) int {
	if p.Log != "" && (p.Job == "" || p.Log != "stdout" && p.Log != "stderr") || p.All && len(p.Node) > 0 || p.File != "" && len(args) > 0 || p.Job != "" && (p.All || len(p.Node) > 1 || p.File != "" || len(args) > 0) || p.Job != "" && !noderun.ValidID(p.Job) {
		fmt.Fprintln(stderr, "invalid node/script/job selection")
		return 1
	}
	req := noderun.Request{TimeoutSeconds: int64(p.Timeout / time.Second)}
	if p.Job == "" {
		if p.Timeout == 0 {
			req.TimeoutSeconds = 3600
		} else if p.Timeout < time.Second || p.Timeout > 24*time.Hour {
			fmt.Fprintln(stderr, "timeout must be 1s..24h")
			return 1
		}
		if p.File != "" {
			f, err := os.Open(p.File)
			if err != nil {
				return fail(stderr, err)
			}
			raw, err := io.ReadAll(io.LimitReader(f, noderun.MaxScriptBytes+1))
			_ = f.Close()
			if err != nil {
				return fail(stderr, err)
			}
			req.Script = string(raw)
		} else if len(args) == 1 {
			req.Script = args[0]
		} else {
			quoted := []string{}
			for _, a := range args {
				quoted = append(quoted, clcommon.ShellQuoteArg(a))
			}
			req.Script = strings.Join(quoted, " ")
		}
		if err := req.Validate(); err != nil {
			return fail(stderr, err)
		}
	}
	if rc := agent.RequireDaemonOrExit(stderr); rc != 0 {
		return rc
	}
	nodes := append([]string(nil), p.Node...)
	offline := map[string]bool{}
	if len(nodes) == 0 {
		nodes = []string{"local"}
	}
	if p.All {
		var status struct {
			Peers []struct {
				InstanceID string `json:"instance_id"`
				Trusted    bool   `json:"trusted"`
				Online     bool   `json:"online"`
			} `json:"peers"`
		}
		if err := agent.DaemonRequest("GET", "/v1/federation/status?summary=1", nil, &status, agent.DaemonOpts{Timeout: 20 * time.Second, NoRetry: true}); err != nil {
			return fail(stderr, err)
		}
		for _, peer := range status.Peers {
			if peer.Trusted {
				nodes = append(nodes, peer.InstanceID)
				offline[peer.InstanceID] = !peer.Online
			}
		}
	}
	seen := map[string]bool{}
	for _, node := range nodes {
		if seen[node] {
			fmt.Fprintln(stderr, "duplicate node selection")
			return 1
		}
		seen[node] = true
	}
	results := make([]nodeRunOutcome, len(nodes))
	var wg sync.WaitGroup
	workers := make(chan struct{}, 4)
	for i, node := range nodes {
		if offline[node] {
			results[i] = nodeRunOutcome{Node: node, State: "skipped", Error: "peer_offline"}
			continue
		}
		wg.Add(1)
		go func(i int, node string) {
			defer wg.Done()
			workers <- struct{}{}
			defer func() { <-workers }()
			result := nodeRunOutcome{Node: node, State: "failed"}
			var job noderun.Job
			request := func(method, tail string, in any) error {
				if err := agent.DaemonRequest(method, nodeRunPath(node, tail), in, &job, agent.DaemonOpts{Timeout: 20 * time.Second, NoRetry: true}); err != nil {
					return err
				}
				if !noderun.ValidID(job.ID) || job.TimeoutSeconds < 1 || job.TimeoutSeconds > 86400 {
					return fmt.Errorf("invalid script job reply")
				}
				switch job.State {
				case "running", "completed", "failed", "canceled", "interrupted":
				default:
					return fmt.Errorf("invalid script job state")
				}
				return nil
			}
			method, tail := "POST", ""
			var in any = req
			if p.Job != "" {
				method = "GET"
				tail = "/jobs/" + p.Job
				in = nil
			}
			if err := request(method, tail, in); err != nil {
				var de *agent.DaemonError
				if errors.As(err, &de) && nodeRunOffline(de) {
					result.State = "skipped"
				}
				result.Error = err.Error()
				results[i] = result
				return
			}
			deadline := time.Now().Add(time.Duration(job.TimeoutSeconds)*time.Second + 30*time.Second)
			for !p.NoWait && p.Job == "" && job.State == "running" && time.Now().Before(deadline) {
				time.Sleep(time.Second)
				if err := request("GET", "/jobs/"+job.ID, nil); err != nil {
					result.Error = err.Error()
					result.Job = &job
					results[i] = result
					return
				}
			}
			if p.Log != "" {
				offset := int64(0)
				for {
					var chunk noderun.LogChunk
					err := agent.DaemonRequest("GET", nodeRunPath(node, "/jobs/"+job.ID+"/logs?stream="+p.Log+"&offset="+fmt.Sprint(offset)), nil, &chunk, agent.DaemonOpts{Timeout: 20 * time.Second, NoRetry: true})
					if err != nil {
						result.Error = err.Error()
						result.Job = &job
						results[i] = result
						return
					}
					if len(chunk.Data) > 64<<10 || chunk.NextOffset != offset+int64(len(chunk.Data)) || !chunk.EOF && len(chunk.Data) == 0 || len(result.Log)+len(chunk.Data) > noderun.MaxOutputBytes+4096 {
						result.Error = "invalid bounded log reply"
						results[i] = result
						return
					}
					result.Log = append(result.Log, chunk.Data...)
					offset = chunk.NextOffset
					if chunk.EOF {
						break
					}
				}
				result.Stream = p.Log
			}
			result.State = job.State
			result.Job = &job
			result.Error = job.Error
			results[i] = result
		}(i, node)
	}
	wg.Wait()
	rc := 0
	for _, r := range results {
		if r.State == "failed" || r.State == "canceled" || r.State == "interrupted" || r.State == "skipped" || r.State == "running" && !p.NoWait && p.Job == "" {
			rc = 1
		}
		if !p.JSON {
			fmt.Fprintf(stdout, "%s: %s", summaryCell(r.Node), summaryCell(r.State))
			if r.Job != nil {
				fmt.Fprintf(stdout, " exit=%d duration=%dms job=%s", r.Job.ExitCode, r.Job.DurationMS, r.Job.ID)
			}
			fmt.Fprintln(stdout)
			if r.Stream != "" {
				fmt.Fprintln(stdout, proto.StripControls(string(r.Log)))
			}
			if r.Job != nil && r.Stream == "" {
				if r.Job.StdoutTail != "" {
					fmt.Fprintf(stdout, "%s stdout:\n%s\n", summaryCell(r.Node), proto.StripControls(r.Job.StdoutTail))
				}
				if r.Job.StderrTail != "" {
					fmt.Fprintf(stdout, "%s stderr:\n%s\n", summaryCell(r.Node), proto.StripControls(r.Job.StderrTail))
				}
			}
			if r.Error != "" {
				fmt.Fprintln(stderr, summaryCell(r.Error))
			}
		}
	}
	if p.JSON {
		if json.NewEncoder(stdout).Encode(results) != nil {
			return 1
		}
	}
	return rc
}

type nodeScriptSettingsParams struct {
	AcceptRemote string  `long:"accept-remote" help:"Local receiving switch: on or off; omit to inspect"`
	Memory       string  `long:"memory" help:"Local Linux memory ceiling, e.g. 1GiB"`
	PIDs         uint64  `long:"pids" help:"Local Linux process/thread ceiling"`
	CPU          float64 `long:"cpu" help:"Local Linux CPU cores ceiling"`
}

func nodeScriptSettingsCmd() *cobra.Command {
	return boa.CmdT[nodeScriptSettingsParams]{Use: "scripts", Short: "Inspect or configure this node's remote script acceptance (local only)", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *nodeScriptSettingsParams, _ *cobra.Command, _ []string) {
		os.Exit(runNodeScriptSettings(p, os.Stdout, os.Stderr))
	}}.ToCobra()
}
func runNodeScriptSettings(p *nodeScriptSettingsParams, stdout, stderr io.Writer) int {
	if p.AcceptRemote != "" && p.AcceptRemote != "on" && p.AcceptRemote != "off" {
		fmt.Fprintln(stderr, "accept-remote must be on or off")
		return 1
	}
	if rc := agent.RequireDaemonOrExit(stderr); rc != 0 {
		return rc
	}
	method := "GET"
	var in any
	body := map[string]any{}
	if p.AcceptRemote != "" {
		body["accept_remote_scripts"] = p.AcceptRemote == "on"
	}
	if p.Memory != "" || p.PIDs != 0 || p.CPU != 0 {
		var current struct {
			Limits map[string]any `json:"resource_limits"`
		}
		if err := agent.DaemonRequest("GET", "/v1/node/run/settings", nil, &current, agent.DaemonOpts{}); err != nil {
			return fail(stderr, err)
		}
		if current.Limits == nil {
			current.Limits = map[string]any{}
		}
		delete(current.Limits, "memory_bytes")
		if p.Memory != "" {
			current.Limits["memory"] = p.Memory
		}
		if p.PIDs != 0 {
			current.Limits["pids"] = p.PIDs
		}
		if p.CPU != 0 {
			current.Limits["cpu"] = p.CPU
		}
		body["resource_limits"] = current.Limits
	}
	if len(body) > 0 {
		method = "PUT"
		in = body
	}
	var raw json.RawMessage
	if err := agent.DaemonRequest(method, "/v1/node/run/settings", in, &raw, agent.DaemonOpts{NoRetry: true}); err != nil {
		return fail(stderr, err)
	}
	if json.NewEncoder(stdout).Encode(raw) != nil {
		return 1
	}
	return 0
}

func nodeRunOffline(err *agent.DaemonError) bool {
	if err.Code == "peer_offline" {
		return true
	}
	var response struct {
		Reason string `json:"reason"`
	}
	return json.Unmarshal(err.Raw, &response) == nil && response.Reason == "peer_offline"
}
