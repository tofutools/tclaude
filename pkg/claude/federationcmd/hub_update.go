package federationcmd

import (
	"fmt"
	"io"
	"net/url"
	"os"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/common"
	"github.com/tofutools/tclaude/pkg/selfupdate"
)

type hubUpdateParams struct {
	Action  string `pos:"true" optional:"true" help:"check, apply or rollback (default: status)"`
	Version string `long:"version" help:"Canonical release version; downgrades refused"`
	Job     string `long:"job" help:"Read an existing update job"`
	NoWait  bool   `long:"no-wait" help:"Return immediately after creating a durable job"`
}

func hubUpdateCmd() *cobra.Command {
	return boa.CmdT[hubUpdateParams]{Use: "update", Short: "Check or update a supervised hub (hub.update required)", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *hubUpdateParams, _ *cobra.Command, _ []string) { os.Exit(runHubUpdate(p, os.Stdout, os.Stderr)) }}.ToCobra()
}
func runHubUpdate(p *hubUpdateParams, stdout, stderr io.Writer) int {
	if p.Job != "" {
		if p.Action != "" || p.Version != "" {
			return fail(stderr, fmt.Errorf("--job cannot start an update"))
		}
		return runHubCall("GET", "update/jobs/"+url.PathEscape(p.Job), nil, stdout, stderr)
	}
	if p.Action == "" {
		if p.Version != "" {
			return fail(stderr, fmt.Errorf("--version requires check or apply"))
		}
		return runHubCall("GET", "update", nil, stdout, stderr)
	}
	req := selfupdate.Request{Action: p.Action, Version: p.Version}
	if err := req.Validate(); err != nil {
		return fail(stderr, err)
	}
	if rc := agent.RequireDaemonOrExit(stderr); rc != 0 {
		return rc
	}
	var job selfupdate.Job
	if err := agent.DaemonRequest("POST", "/v1/federation/hub/update", req, &job, agent.DaemonOpts{NoRetry: true, Timeout: 35 * time.Second}); err != nil {
		return fail(stderr, err)
	}
	if p.NoWait {
		return printJSON(stdout, job)
	}
	deadline := time.Now().Add(25 * time.Minute)
	for job.State == "running" || job.State == "restarting" {
		if time.Now().After(deadline) {
			return fail(stderr, fmt.Errorf("update still pending; resume with --job %s", job.ID))
		}
		fmt.Fprintf(stderr, "hub update %s: %s\n", job.ID, job.Phase)
		time.Sleep(2 * time.Second)
		// A serving child restart temporarily drops the authenticated connection.
		// Only reads are retried; the apply mutation is never replayed.
		var next selfupdate.Job
		if err := agent.DaemonRequest("GET", "/v1/federation/hub/update/jobs/"+url.PathEscape(job.ID), nil, &next, agent.DaemonOpts{NoRetry: true, Timeout: 10 * time.Second}); err == nil {
			job = next
		}
	}
	if rc := printJSON(stdout, job); rc != 0 {
		return rc
	}
	if job.State == "failed" || job.State == "rolled_back" {
		return 1
	}
	return 0
}
