package agent

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
)

// `tclaude agent cron` — recurring scheduled jobs.
//
// v1 verbs: ls, add, rm. Edit/enable-disable/run-now are follow-up
// slices. Permissions: self.schedule (default-granted) for own jobs,
// agent.schedule for cross-agent, group-owner-of-target for the
// manager pattern.

func cronCmd() *cobra.Command {
	return boa.CmdT[struct{}]{
		Use:         "cron",
		Short:       "Manage recurring scheduled jobs (the agentd cron scheduler)",
		Long:        "List, add, and remove agent_cron_jobs. The daemon's scheduler ticks every 30s and fires due jobs by inserting agent_messages rows (or direct send-keys for solo targets). A job may target a single conv or, via --target group:NAME, multicast to every current member of a group.",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds: []*cobra.Command{
			cronLsCmd(),
			cronAddCmd(),
			cronRmCmd(),
			cronLogsCmd(),
			cronEnableCmd(),
			cronDisableCmd(),
			cronRunNowCmd(),
		},
	}.ToCobra()
}

type cronJobJSON struct {
	ID              int64  `json:"id"`
	Name            string `json:"name,omitempty"`
	OwnerConv       string `json:"owner_conv"`
	TargetKind      string `json:"target_kind"`
	TargetConv      string `json:"target_conv"`
	GroupID         int64  `json:"group_id,omitempty"`
	GroupName       string `json:"group_name,omitempty"`
	IntervalSeconds int64  `json:"interval_seconds"`
	Subject         string `json:"subject,omitempty"`
	Body            string `json:"body"`
	Enabled         bool   `json:"enabled"`
	CreatedAt       string `json:"created_at,omitempty"`
	LastRunAt       string `json:"last_run_at,omitempty"`
	LastRunStatus   string `json:"last_run_status,omitempty"`
}

// ---- ls ----

type cronLsParams struct{}

func cronLsCmd() *cobra.Command {
	return boa.CmdT[cronLsParams]{
		Use:         "ls",
		Short:       "List scheduled cron jobs visible to this caller",
		Long:        "Returns all jobs the caller can see: humans see everything, agents see jobs they own, jobs targeting them, and jobs targeting any conv in a group they own.",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(_ *cronLsParams, _ *cobra.Command, _ []string) {
			os.Exit(runCronLs(os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runCronLs(stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	var resp struct {
		Jobs []cronJobJSON `json:"jobs"`
	}
	if err := DaemonRequest(http.MethodGet, "/v1/cron", nil, &resp, DaemonOpts{}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if len(resp.Jobs) == 0 {
		fmt.Fprintln(stdout, "(no scheduled cron jobs)")
		return rcOK
	}
	fmt.Fprintf(stdout, "%-4s  %-10s  %-9s  %-16s  %-9s  %s\n",
		"ID", "INTERVAL", "ENABLED", "TARGET", "LAST", "NAME / BODY")
	fmt.Fprintln(stdout, strings.Repeat("─", 80))
	for _, j := range resp.Jobs {
		interval := (time.Duration(j.IntervalSeconds) * time.Second).String()
		enabled := "yes"
		if !j.Enabled {
			enabled = "no"
		}
		last := j.LastRunStatus
		if j.LastRunAt != "" && last == "" {
			last = "?"
		}
		if last == "" {
			last = "(never)"
		}
		desc := j.Name
		if desc == "" {
			desc = truncate(j.Body, 40)
		}
		fmt.Fprintf(stdout, "%-4d  %-10s  %-9s  %-16s  %-9s  %s\n",
			j.ID, interval, enabled, cronTargetLabel(j), last, desc)
	}
	return rcOK
}

// cronTargetLabel renders a job's target for the ls TARGET column:
// "group:<name>" (falling back to "group:#<id>") for a group-target
// job, else the short conv id.
func cronTargetLabel(j cronJobJSON) string {
	if j.TargetKind == "group" {
		if j.GroupName != "" {
			return "group:" + j.GroupName
		}
		return "group:#" + strconv.FormatInt(j.GroupID, 10)
	}
	return short(j.TargetConv)
}

// ---- add ----

type cronAddParams struct {
	Target   string `long:"target" optional:"true" help:"Selector for the conv that receives the cron message, or group:NAME to multicast to every member of a group. Defaults to self when omitted."`
	Interval string `long:"interval" help:"Recurrence interval as a Go duration (e.g. 10m, 1h, 30s). Minimum 30s (the scheduler tick)."`
	Body     string `long:"body" optional:"true" help:"Message body the cron job sends each tick. Required unless --file is given."`
	File     string `long:"file" short:"f" optional:"true" help:"Read the message body from this file instead of --body ('-' reads stdin). Sidesteps shell quoting — best for long, multi-line, or backtick-containing bodies. Mutually exclusive with --body."`
	Subject  string `long:"subject" optional:"true" help:"Optional subject. Auto-prefixed with [cron:<name>] when delivered."`
	Name     string `long:"name" optional:"true" help:"Short label for the job (used in dashboard + log lines)."`
}

func cronAddCmd() *cobra.Command {
	return boa.CmdT[cronAddParams]{
		Use:         "add",
		Short:       "Schedule a new recurring cron job",
		Long:        "Creates a job that fires every --interval and delivers a message body to --target. Defaults to self-targeted when --target is omitted. Give the body inline with --body, or with --file <path> (or --file - to read stdin) — the file form sidesteps shell quoting, including backticks the shell would otherwise eat from an inline string. Pass --target group:NAME to multicast each tick to every current member of a group (membership is resolved at fire time).",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *cronAddParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Target).SetAlternativesFunc(completeConvSelectors)
			return nil
		},
		RunFunc: func(p *cronAddParams, _ *cobra.Command, _ []string) {
			os.Exit(runCronAdd(p, os.Stdin, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runCronAdd(p *cronAddParams, stdin io.Reader, stdout, stderr io.Writer) int {
	if strings.TrimSpace(p.Interval) == "" {
		fmt.Fprintln(stderr, "Error: --interval is required (e.g. --interval 10m)")
		return rcInvalidArg
	}
	jobBody, rc := resolveBodyInput(p.Body, p.File, "--body", stdin, stderr)
	if rc != rcOK {
		return rc
	}
	if strings.TrimSpace(jobBody) == "" {
		fmt.Fprintln(stderr, "Error: a message body is required — pass --body or --file")
		return rcInvalidArg
	}
	target := strings.TrimSpace(p.Target)
	if target == "" {
		// Self-targeted default — use the daemon-resolved canonical
		// dot selector so the daemon picks our caller conv.
		target = "."
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	body := map[string]any{
		"target":   target,
		"interval": p.Interval,
		"body":     jobBody,
	}
	if p.Subject != "" {
		body["subject"] = p.Subject
	}
	if p.Name != "" {
		body["name"] = p.Name
	}
	var resp cronJobJSON
	if err := DaemonRequest(http.MethodPost, "/v1/cron", body, &resp, DaemonOpts{}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	interval := (time.Duration(resp.IntervalSeconds) * time.Second).String()
	fmt.Fprintf(stdout, "Scheduled job #%d every %s → %s\n",
		resp.ID, interval, cronTargetLabel(resp))
	switch {
	case resp.TargetKind == "group":
		fmt.Fprintln(stdout, "  Group multicast — each tick fans out to every current member of the group (resolved at fire time).")
	case resp.GroupID > 0:
		fmt.Fprintf(stdout, "  Routed via group %d (will use agent_messages + flush nudge).\n", resp.GroupID)
	default:
		fmt.Fprintln(stdout, "  Solo target — scheduler will send-keys directly when target's pane is alive.")
	}
	return rcOK
}

// ---- rm ----

type cronRmParams struct {
	ID string `pos:"true" help:"Cron job ID (from 'tclaude agent cron ls')."`
}

func cronRmCmd() *cobra.Command {
	return boa.CmdT[cronRmParams]{
		Use:         "rm <id>",
		Short:       "Delete a scheduled cron job",
		Long:        "Removes a job by ID. Caller must be the job's owner_conv, hold agent.schedule, or own a group containing the job's target.",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *cronRmParams, _ *cobra.Command, _ []string) {
			os.Exit(runCronRm(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runCronRm(p *cronRmParams, stdout, stderr io.Writer) int {
	id, err := strconv.ParseInt(strings.TrimSpace(p.ID), 10, 64)
	if err != nil {
		fmt.Fprintf(stderr, "Error: id must be an integer; got %q\n", p.ID)
		return rcInvalidArg
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	path := "/v1/cron/" + strconv.FormatInt(id, 10)
	if err := DaemonRequest(http.MethodDelete, path, nil, nil, DaemonOpts{}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	fmt.Fprintf(stdout, "Deleted cron job #%d\n", id)
	return rcOK
}

// ---- enable / disable / run-now ----

type cronIDOnlyParams struct {
	ID string `pos:"true" help:"Cron job ID (from 'tclaude agent cron ls')."`
}

func cronEnableCmd() *cobra.Command {
	return boa.CmdT[cronIDOnlyParams]{
		Use:         "enable <id>",
		Short:       "Re-enable a previously disabled cron job",
		Long:        "Flips enabled=true. Does NOT touch last_run_at, so re-enabling a paused job doesn't fire immediately if it ran recently.",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *cronIDOnlyParams, _ *cobra.Command, _ []string) {
			os.Exit(runCronEnable(p, true, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func cronDisableCmd() *cobra.Command {
	return boa.CmdT[cronIDOnlyParams]{
		Use:         "disable <id>",
		Short:       "Pause a cron job without deleting it",
		Long:        "Flips enabled=false. The row stays — `cron enable` reactivates it. Use to silence a noisy job temporarily.",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *cronIDOnlyParams, _ *cobra.Command, _ []string) {
			os.Exit(runCronEnable(p, false, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func cronRunNowCmd() *cobra.Command {
	return boa.CmdT[cronIDOnlyParams]{
		Use:         "run-now <id>",
		Short:       "Fire a cron job immediately (without waiting for the next tick)",
		Long:        "Manually triggers one fire. Useful for testing a freshly-added job or for a one-off nudge between regular runs. Updates last_run_at + appends a row to the run history.",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *cronIDOnlyParams, _ *cobra.Command, _ []string) {
			os.Exit(runCronRunNow(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runCronEnable(p *cronIDOnlyParams, enabled bool, stdout, stderr io.Writer) int {
	id, err := strconv.ParseInt(strings.TrimSpace(p.ID), 10, 64)
	if err != nil {
		fmt.Fprintf(stderr, "Error: id must be an integer; got %q\n", p.ID)
		return rcInvalidArg
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	verb := "enable"
	if !enabled {
		verb = "disable"
	}
	path := "/v1/cron/" + strconv.FormatInt(id, 10) + "/" + verb
	if err := DaemonRequest(http.MethodPost, path, nil, nil, DaemonOpts{}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if enabled {
		fmt.Fprintf(stdout, "Enabled cron job #%d\n", id)
	} else {
		fmt.Fprintf(stdout, "Disabled cron job #%d\n", id)
	}
	return rcOK
}

func runCronRunNow(p *cronIDOnlyParams, stdout, stderr io.Writer) int {
	id, err := strconv.ParseInt(strings.TrimSpace(p.ID), 10, 64)
	if err != nil {
		fmt.Fprintf(stderr, "Error: id must be an integer; got %q\n", p.ID)
		return rcInvalidArg
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	path := "/v1/cron/" + strconv.FormatInt(id, 10) + "/run-now"
	var resp struct {
		Status string `json:"status"`
	}
	if err := DaemonRequest(http.MethodPost, path, nil, &resp, DaemonOpts{}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	fmt.Fprintf(stdout, "Fired cron job #%d (status: %s)\n", id, resp.Status)
	return rcOK
}

// ---- logs ----

type cronLogsParams struct {
	ID    string `pos:"true" help:"Cron job ID (from 'tclaude agent cron ls')."`
	Limit int    `long:"limit" optional:"true" help:"Max number of runs to return (default 25, max 1000)."`
}

func cronLogsCmd() *cobra.Command {
	return boa.CmdT[cronLogsParams]{
		Use:         "logs <id>",
		Short:       "Show recent execution history for a cron job",
		Long:        "Returns the most-recent fires for a job (newest first), one row per scheduler tick that picked it up. Visibility: caller must be owner, target, or owner of a group containing the target.",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *cronLogsParams, _ *cobra.Command, _ []string) {
			os.Exit(runCronLogs(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runCronLogs(p *cronLogsParams, stdout, stderr io.Writer) int {
	id, err := strconv.ParseInt(strings.TrimSpace(p.ID), 10, 64)
	if err != nil {
		fmt.Fprintf(stderr, "Error: id must be an integer; got %q\n", p.ID)
		return rcInvalidArg
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	path := "/v1/cron/" + strconv.FormatInt(id, 10) + "/logs"
	if p.Limit > 0 {
		path += "?limit=" + strconv.Itoa(p.Limit)
	}
	var resp struct {
		Runs []struct {
			ID       int64  `json:"id"`
			JobID    int64  `json:"job_id"`
			FiredAt  string `json:"fired_at"`
			Status   string `json:"status"`
			ErrorMsg string `json:"error_msg"`
		} `json:"runs"`
	}
	if err := DaemonRequest(http.MethodGet, path, nil, &resp, DaemonOpts{}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if len(resp.Runs) == 0 {
		fmt.Fprintln(stdout, "(no runs yet)")
		return rcOK
	}
	fmt.Fprintf(stdout, "%-20s  %-12s  %s\n", "FIRED", "STATUS", "ERROR")
	fmt.Fprintln(stdout, strings.Repeat("─", 60))
	for _, run := range resp.Runs {
		fired := run.FiredAt
		if t, err := time.Parse(time.RFC3339, run.FiredAt); err == nil {
			fired = t.Local().Format("2006-01-02 15:04:05")
		}
		fmt.Fprintf(stdout, "%-20s  %-12s  %s\n", fired, run.Status, run.ErrorMsg)
	}
	return rcOK
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 3 {
		return s[:n]
	}
	return s[:n-3] + "..."
}
