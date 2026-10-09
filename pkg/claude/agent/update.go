package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/selfupdate"
)

type updateParams struct {
	Node     string `long:"node" help:"Trusted peer label or instance ID; omit for this node"`
	Version  string `long:"version" help:"Pin a canonical v-prefixed release version"`
	Apply    bool   `long:"apply" help:"Apply a verified update and gracefully restart agentd"`
	Rollback bool   `long:"rollback" help:"Restore the previous binaries and restart agentd"`
	Check    bool   `long:"check" help:"Check for an update (the default)"`
	NoWait   bool   `long:"no-wait" help:"Return the durable job ID without waiting"`
	Job      string `long:"job" help:"Read an existing durable update job by ID"`
	JSON     bool   `long:"json" help:"Output JSON"`
}

func UpdateCmd() *cobra.Command {
	return boa.CmdT[updateParams]{Use: "update", Short: "Check, apply or roll back a tclaude update (operator only)", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *updateParams, _ *cobra.Command, _ []string) { os.Exit(runUpdate(p, os.Stdout, os.Stderr)) }}.ToCobra()
}
func updateAPIPath(node, tail string) string {
	if node == "" {
		return "/v1/node/update" + tail
	}
	return "/v1/federation/peer/" + url.PathEscape(node) + "/node/update" + tail
}
func runUpdate(p *updateParams, stdout, stderr io.Writer) int {
	if (p.Apply && p.Rollback) || (p.Check && (p.Apply || p.Rollback)) || p.Job != "" && (p.Apply || p.Rollback || p.Check || p.Version != "") {
		fmt.Fprintln(stderr, "Error: choose only one update action")
		return rcInvalidArg
	}
	req := selfupdate.Request{Action: "check", Version: p.Version}
	if p.Apply {
		req.Action = "apply"
	}
	if p.Rollback {
		req.Action = "rollback"
	}
	if err := req.Validate(); err != nil {
		fmt.Fprintln(stderr, "Error:", err)
		return rcInvalidArg
	}
	if p.Job != "" && (len(p.Job) != 32 || strings.Trim(p.Job, "0123456789abcdef") != "") {
		fmt.Fprintln(stderr, "Error: invalid update job ID")
		return rcInvalidArg
	}
	if rc := RequireDaemonOrExit(stderr); rc != 0 {
		return rc
	}
	var raw json.RawMessage
	request := func(method, path string, in any) error {
		return DaemonRequest(method, path, in, &raw, DaemonOpts{Timeout: 20 * time.Second, NoRetry: true, RetryOutput: stderr})
	}
	fail := func(err error) int {
		var de *DaemonError
		if errors.As(err, &de) && json.Valid(de.Raw) {
			fmt.Fprintln(stderr, string(de.Raw))
		} else {
			fmt.Fprintln(stderr, "Error:", err)
		}
		return MapDaemonErrorToRC(err)
	}
	if p.Job != "" {
		if err := request(http.MethodGet, updateAPIPath(p.Node, "/jobs/"+url.PathEscape(p.Job)), nil); err != nil {
			return fail(err)
		}
		return printUpdateResult(p, raw, stdout, stderr)
	}
	if err := request(http.MethodPost, updateAPIPath(p.Node, ""), req); err != nil {
		return fail(err)
	}
	var job selfupdate.Job
	if err := json.Unmarshal(raw, &job); err != nil {
		return fail(err)
	}
	for _, warning := range job.Warnings {
		fmt.Fprintln(stderr, "Warning:", updateCell(warning))
	}
	if p.NoWait {
		return printUpdateResult(p, raw, stdout, stderr)
	}
	deadline := time.Now().Add(20 * time.Minute)
	phase := ""
	unreachableSince := time.Time{}
	for time.Now().Before(deadline) {
		if job.Phase != phase {
			fmt.Fprintf(stderr, "Update %s: %s\n", job.ID, updateCell(job.Phase))
			phase = job.Phase
		}
		if job.State == "succeeded" || job.State == "failed" {
			return printUpdateResult(p, raw, stdout, stderr)
		}
		time.Sleep(time.Second)
		if err := request(http.MethodGet, updateAPIPath(p.Node, "/jobs/"+job.ID), nil); err != nil {
			// Updates intentionally disconnect the daemon. Keep the job ID so the
			// operator can resume inspection even if reconnection takes too long.
			if req.Action == "check" {
				return fail(err)
			}
			var de *DaemonError
			if errors.As(err, &de) && de.Status >= 400 && de.Status < 500 {
				return fail(err)
			}
			if unreachableSince.IsZero() {
				unreachableSince = time.Now()
			}
			if time.Since(unreachableSince) > 90*time.Second {
				fmt.Fprintf(stderr, "Update connection lost; inspect later with --job %s\n", job.ID)
				return fail(err)
			}
			continue
		}
		unreachableSince = time.Time{}
		if err := json.Unmarshal(raw, &job); err != nil {
			return fail(err)
		}
	}
	fmt.Fprintf(stderr, "Update still running; inspect with --job %s\n", job.ID)
	return rcIOFailure
}
func updateCell(s string) string { return strings.Join(strings.Fields(proto.StripControls(s)), " ") }
func printUpdateResult(p *updateParams, raw json.RawMessage, stdout, stderr io.Writer) int {
	var job selfupdate.Job
	if err := json.Unmarshal(raw, &job); err != nil {
		fmt.Fprintln(stderr, err)
		return rcIOFailure
	}
	if p.JSON {
		if err := json.NewEncoder(stdout).Encode(raw); err != nil {
			return rcIOFailure
		}
	} else {
		fmt.Fprintf(stdout, "%s %s: %s (%s)\n", updateCell(job.Action), updateCell(job.Version), updateCell(job.State), updateCell(job.ID))
		if job.Action == "check" && job.State == "succeeded" {
			available := "unknown"
			if job.UpdateAvailable != nil {
				available = fmt.Sprint(*job.UpdateAvailable)
			}
			fmt.Fprintf(stdout, "Current: %s; selected: %s; update available: %s\n", updateCell(job.CurrentVersion), updateCell(job.Version), available)
		}
		if job.Error != "" {
			fmt.Fprintln(stderr, updateCell(job.Error))
		}
	}
	if job.State == "failed" {
		return rcIOFailure
	}
	return 0
}
