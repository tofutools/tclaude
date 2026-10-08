package federationcmd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/common"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

type jobRunParams struct {
	Node    string `long:"node" help:"Peer label or immutable instance ID"`
	Repo    string `long:"repo" help:"Receiver's allowlisted repository alias"`
	Ref     string `long:"ref" help:"Git branch, full ref or exact commit"`
	Group   string `long:"group" help:"Receiving group"`
	Harness string `long:"harness" optional:"true" help:"Requested harness (shell executes command text)"`
	Command string `long:"command" help:"One-shot task or shell command"`
	Timeout int64  `long:"timeout" default:"3600" help:"Total job deadline in seconds"`
	JSON    bool   `long:"json" help:"Print job state and output as JSON"`
}
type jobAcknowledgeParams struct {
	ID                 string `pos:"true" help:"Uncertain local job ID"`
	AcknowledgeStopped bool   `long:"acknowledge-stopped" help:"Confirm workload teardown has been checked"`
}
type jobIDParams struct {
	ID string `pos:"true" help:"Immutable job ID"`
}

func jobsCmd() *cobra.Command {
	return boa.CmdT[struct{}]{Use: "job", Aliases: []string{"jobs"}, Short: "Run one-shot tasks in a peer's allowed Git repositories", ParamEnrich: common.DefaultParamEnricher(), SubCmds: []*cobra.Command{
		boa.CmdT[jobRunParams]{Use: "run", Short: "Run on one explicit peer and return verified output and exit status", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *jobRunParams, _ *cobra.Command, _ []string) {
			if rc := agent.RequireDaemonOrExit(os.Stderr); rc != 0 {
				os.Exit(rc)
			}
			var sent struct {
				Job db.FederationJob `json:"job"`
			}
			body := map[string]any{"node": p.Node, "repo": p.Repo, "ref": p.Ref, "group": p.Group, "harness": p.Harness, "command": p.Command, "timeout_seconds": p.Timeout}
			if e := agent.DaemonRequest(http.MethodPost, "/v1/federation/jobs", body, &sent, agent.DaemonOpts{}); e != nil {
				os.Exit(fail(os.Stderr, e))
			}
			fmt.Fprintln(os.Stderr, "Job", sent.Job.ID)
			deadline := time.Now().Add(time.Duration(p.Timeout)*time.Second + 6*time.Minute)
			for time.Now().Before(deadline) {
				var j db.FederationJob
				if e := agent.DaemonGet("/v1/federation/jobs/"+sent.Job.ID, &j); e != nil {
					os.Exit(fail(os.Stderr, e))
				}
				switch j.State {
				case "unknown":
					fmt.Fprintln(os.Stderr, "Execution uncertain; inspect job", j.ID)
					os.Exit(1)
				case "completed", "failed", "canceled", "timeout", "refused", "interrupted", "output_unavailable":
					var res proto.JobResult
					_ = json.Unmarshal(j.Result, &res)
					var logs struct {
						Stdout   string `json:"stdout"`
						Stderr   string `json:"stderr"`
						ExitCode int    `json:"exit_code"`
					}
					if len(res.Logs) > 0 {
						if e := agent.DaemonGet("/v1/federation/jobs/"+j.ID+"/logs", &logs); e != nil {
							os.Exit(fail(os.Stderr, e))
						}
					}
					if p.JSON {
						_ = printJSON(os.Stdout, map[string]any{"job": j, "output": logs})
					} else {
						fmt.Fprint(os.Stdout, proto.StripControls(logs.Stdout))
						fmt.Fprint(os.Stderr, proto.StripControls(logs.Stderr))
						if res.Code != "" {
							fmt.Fprintln(os.Stderr, res.Code)
						}
					}
					code := res.ExitCode
					if j.State != "completed" && code == 0 {
						code = 1
					}
					os.Exit(code)
				}
				time.Sleep(time.Second)
			}
			fmt.Fprintln(os.Stderr, "Job status uncertain; inspect or cancel", sent.Job.ID)
			os.Exit(1)
		}}.ToCobra(),
		boa.CmdT[struct{}]{Use: "ls", Short: "List local and submitted jobs", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(_ *struct{}, _ *cobra.Command, _ []string) {
			repoRequest(http.MethodGet, "/v1/federation/jobs", nil)
		}}.ToCobra(),
		boa.CmdT[jobIDParams]{Use: "status", Short: "Query a job without launching it again", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *jobIDParams, _ *cobra.Command, _ []string) {
			repoRequest(http.MethodGet, "/v1/federation/jobs/"+url.PathEscape(p.ID), nil)
		}}.ToCobra(),
		boa.CmdT[jobIDParams]{Use: "cancel", Short: "Request cancellation of this job", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *jobIDParams, _ *cobra.Command, _ []string) {
			repoRequest(http.MethodPost, "/v1/federation/jobs/"+url.PathEscape(p.ID)+"/cancel", nil)
		}}.ToCobra(),
		boa.CmdT[jobAcknowledgeParams]{Use: "acknowledge-stopped", Short: "Release an uncertain reservation after checking workload teardown", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *jobAcknowledgeParams, _ *cobra.Command, _ []string) {
			repoRequest(http.MethodPost, "/v1/federation/jobs/"+url.PathEscape(p.ID)+"/acknowledge-stopped", map[string]any{"acknowledge_stopped": p.AcknowledgeStopped})
		}}.ToCobra(),
		boa.CmdT[jobIDParams]{Use: "retry", Short: "Resend the same immutable request after uncertain delivery", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *jobIDParams, _ *cobra.Command, _ []string) {
			repoRequest(http.MethodPost, "/v1/federation/jobs/"+url.PathEscape(p.ID)+"/retry", nil)
		}}.ToCobra(),
		boa.CmdT[jobIDParams]{Use: "approve", Short: "Approve a pending inbound job", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *jobIDParams, _ *cobra.Command, _ []string) {
			repoRequest(http.MethodPost, "/v1/federation/jobs/"+url.PathEscape(p.ID)+"/approve", nil)
		}}.ToCobra(),
	}}.ToCobra()
}
