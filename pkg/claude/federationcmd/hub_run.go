package federationcmd

import (
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/common"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/noderun"
)

type hubRunParams struct {
	File    string        `long:"file" optional:"true" help:"Script file (maximum 16 KiB)"`
	Timeout time.Duration `long:"timeout" default:"1h" help:"Deadline (1s..24h)"`
	Job     string        `long:"job" optional:"true" help:"Read an existing job instead of starting one"`
	Log     string        `long:"log" optional:"true" help:"Read stdout or stderr for --job"`
	Offset  int64         `long:"offset" optional:"true" help:"Byte offset for --log"`
	NoWait  bool          `long:"no-wait" help:"Return the durable job immediately"`
}

func hubRunCmd() *cobra.Command {
	return boa.CmdT[hubRunParams]{Use: "run", Short: "Run a script on the connected hub (hub.exec and host opt-in required)", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *hubRunParams, _ *cobra.Command, _ []string) { os.Exit(runHubScript(p, os.Stdout, os.Stderr)) }}.ToCobra()
}
func runHubScript(p *hubRunParams, stdout, stderr io.Writer) int {
	if p.Job != "" && (!noderun.ValidID(p.Job) || p.File != "") || p.Log != "" && (p.Job == "" || p.Log != "stdout" && p.Log != "stderr") || p.Offset < 0 {
		return fail(stderr, fmt.Errorf("invalid script/job selection"))
	}
	if p.Job != "" {
		tail := "run/jobs/" + url.PathEscape(p.Job)
		if p.Log != "" {
			tail += "/logs?stream=" + p.Log + "&offset=" + strconv.FormatInt(p.Offset, 10)
		}
		return runHubCall("GET", tail, nil, stdout, stderr)
	}
	if p.File == "" {
		return runHubCall("GET", "run", nil, stdout, stderr)
	}
	f, err := os.Open(p.File)
	if err != nil {
		return fail(stderr, err)
	}
	raw, err := io.ReadAll(io.LimitReader(f, noderun.MaxScriptBytes+1))
	_ = f.Close()
	if err != nil {
		return fail(stderr, err)
	}
	req := noderun.Request{Script: string(raw), TimeoutSeconds: int64(p.Timeout / time.Second)}
	if p.Timeout < time.Second || p.Timeout > 24*time.Hour || p.Timeout%time.Second != 0 {
		return fail(stderr, fmt.Errorf("timeout must be whole seconds from1s to24h"))
	}
	if err = req.Validate(); err != nil {
		return fail(stderr, err)
	}
	if rc := agent.RequireDaemonOrExit(stderr); rc != 0 {
		return rc
	}
	var job noderun.Job
	request := func(method, tail string, body any) error {
		return agent.DaemonRequest(method, "/v1/federation/hub/"+tail, body, &job, agent.DaemonOpts{NoRetry: true, Timeout: 35 * time.Second})
	}
	if err = request("POST", "run", req); err != nil {
		return fail(stderr, err)
	}
	if !noderun.ValidID(job.ID) {
		return fail(stderr, fmt.Errorf("invalid hub job reply"))
	}
	if p.NoWait {
		return printJSON(stdout, job)
	}
	offsets := map[string]int64{"stdout": 0, "stderr": 0}
	deadline := time.Now().Add(p.Timeout + 30*time.Second)
	for {
		for _, stream := range []string{"stdout", "stderr"} {
			var chunk noderun.LogChunk
			tail := "run/jobs/" + job.ID + "/logs?stream=" + stream + "&offset=" + strconv.FormatInt(offsets[stream], 10)
			if err = agent.DaemonRequest("GET", "/v1/federation/hub/"+tail, nil, &chunk, agent.DaemonOpts{NoRetry: true, Timeout: 35 * time.Second}); err != nil {
				return fail(stderr, err)
			}
			if len(chunk.Data) > 64<<10 || chunk.NextOffset != offsets[stream]+int64(len(chunk.Data)) || chunk.NextOffset > noderun.MaxOutputBytes {
				return fail(stderr, fmt.Errorf("invalid bounded output reply"))
			}
			target := stdout
			if stream == "stderr" {
				target = stderr
			}
			if _, err = io.WriteString(target, proto.StripControls(string(chunk.Data))); err != nil {
				return 1
			}
			offsets[stream] = chunk.NextOffset
		}
		if job.State != "running" && offsets["stdout"] >= job.StdoutBytes && offsets["stderr"] >= job.StderrBytes {
			break
		}
		if time.Now().After(deadline) {
			return fail(stderr, fmt.Errorf("hub job still running; inspect --job %s", job.ID))
		}
		// Three calls per cycle fit the default120/min hub control limit.
		time.Sleep(3 * time.Second)
		if err = request("GET", "run/jobs/"+job.ID, nil); err != nil {
			return fail(stderr, err)
		}
	}
	fmt.Fprintf(stdout, "\n%s: %s exit=%d duration=%dms\n", job.ID, job.State, job.ExitCode, job.DurationMS)
	if job.State != "completed" || job.ExitCode != 0 {
		return 1
	}
	return 0
}
