package federationcmd

import (
	"net/http"
	"net/url"
	"os"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
)

type jobRunParams struct {
	Node    []string `long:"node" help:"Peer labels or IDs (repeat for fan-out); auto or group:<pool> selects one node"`
	Repo    string   `long:"repo" help:"Receiver's allowlisted repository alias"`
	Ref     string   `long:"ref" help:"Git branch, full ref or exact commit"`
	Group   string   `long:"group" help:"Receiving group"`
	Harness string   `long:"harness" optional:"true" help:"Requested harness (shell executes command text)"`
	Command string   `long:"command" help:"One-shot task or shell command"`
	Timeout int64    `long:"timeout" default:"3600" help:"Total job deadline in seconds"`
	Follow  bool     `long:"follow" help:"Print live output while the job runs"`
	Require string   `long:"require" optional:"true" help:"Required node capabilities, e.g. os=darwin,harness=codex"`
	Prefer  string   `long:"prefer" default:"least-loaded" help:"Auto placement rank: least-loaded or most-free-ram"`
	JSON    bool     `long:"json" help:"Print job state and output as JSON"`
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
		boa.CmdT[jobRunParams]{Use: "run", Short: "Run on selected nodes and return verified output and exit status", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *jobRunParams, _ *cobra.Command, _ []string) {
			os.Exit(runFederationJobs(p))
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
