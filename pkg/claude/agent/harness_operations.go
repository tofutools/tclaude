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
	"github.com/tofutools/tclaude/pkg/harnessops"
)

type harnessOperationParams struct {
	Harness              string `pos:"true" optional:"true" help:"Harness name (claude, codex, opencode, copilot, gemini)"`
	Node                 string `long:"node" help:"Trusted node label/ID; omit for local node"`
	All                  bool   `long:"all" help:"Update every installed harness"`
	AllNodes             bool   `long:"all-nodes" help:"Update on this node and every trusted linked node"`
	Now                  bool   `long:"now" help:"Explicitly update now even if agents are active"`
	WhenIdle             bool   `long:"when-idle" help:"Wait until the selected harness is idle"`
	CopyCredentials      bool   `long:"copy-credentials" help:"Explicitly copy own file credentials to the target; its agents will act as you"`
	OverwriteCredentials bool   `long:"overwrite-credentials" help:"Confirm replacement of target credentials after a successful private backup"`
	NoWait               bool   `long:"no-wait" help:"Return job IDs immediately"`
	Job                  string `long:"job" help:"Read a durable harness-operation job"`
	JSON                 bool   `long:"json" help:"Output JSON"`
}

func harnessOperationCommands() []*cobra.Command {
	install := boa.CmdT[harnessOperationParams]{Use: "install", Short: "Install a harness with its official package (operator only)", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *harnessOperationParams, _ *cobra.Command, _ []string) {
		os.Exit(runHarnessOperation("install", p, os.Stdout, os.Stderr))
	}}.ToCobra()
	update := boa.CmdT[harnessOperationParams]{Use: "update", Short: "Update a harness locally or across trusted nodes (operator only)", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *harnessOperationParams, _ *cobra.Command, _ []string) {
		os.Exit(runHarnessOperation("update", p, os.Stdout, os.Stderr))
	}}.ToCobra()
	return []*cobra.Command{install, update}
}
func harnessOperationPath(node, tail string) string {
	if node == "" {
		return "/v1/harnesses/operations" + tail
	}
	return "/v1/federation/peer/" + url.PathEscape(node) + "/harnesses/operations" + tail
}
func runHarnessOperation(action string, p *harnessOperationParams, stdout, stderr io.Writer) int {
	req := harnessops.Request{Action: action, Harness: p.Harness, All: p.All, CopyCredentials: p.CopyCredentials, OverwriteCredentials: p.OverwriteCredentials}
	if p.Now {
		req.Mode = "now"
	}
	if p.WhenIdle {
		req.Mode = "when_idle"
	}
	if p.Now && p.WhenIdle || p.Node != "" && p.AllNodes || p.AllNodes && action != "update" || p.CopyCredentials && p.Node == "" || p.Job != "" && (p.AllNodes || p.All || p.Harness != "" || p.Now || p.WhenIdle || p.CopyCredentials || p.OverwriteCredentials) {
		fmt.Fprintln(stderr, "Error: incompatible harness operation flags")
		return rcInvalidArg
	}
	if p.Job != "" {
		if len(p.Job) != 32 || strings.Trim(p.Job, "0123456789abcdef") != "" {
			fmt.Fprintln(stderr, "Error: invalid job ID")
			return rcInvalidArg
		}
	} else if err := req.Validate(false); err != nil {
		fmt.Fprintln(stderr, "Error:", err)
		return rcInvalidArg
	}
	if rc := RequireDaemonOrExit(stderr); rc != 0 {
		return rc
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
	request := func(method, path string, in, out any) error {
		return DaemonRequest(method, path, in, out, DaemonOpts{Timeout: 20 * time.Second, NoRetry: true})
	}
	nodes := []string{p.Node}
	if p.AllNodes {
		var status struct {
			Peers []struct {
				InstanceID string `json:"instance_id"`
				Trusted    bool   `json:"trusted"`
			} `json:"peers"`
		}
		if err := request(http.MethodGet, "/v1/federation/status?summary=1", nil, &status); err != nil {
			return fail(err)
		}
		nodes = []string{""}
		for _, peer := range status.Peers {
			if peer.Trusted {
				nodes = append(nodes, peer.InstanceID)
			}
		}
	}
	type outcome struct {
		Node string          `json:"node"`
		Job  json.RawMessage `json:"job,omitempty"`
		Code int             `json:"exit_code"`
	}
	outcomes := []outcome{}
	rc := 0
	for _, node := range nodes {
		nodeRC := 0
		var raw json.RawMessage
		path := harnessOperationPath(node, "")
		method := http.MethodPost
		var in any = req
		if p.Job != "" {
			path = harnessOperationPath(node, "/jobs/"+p.Job)
			method = http.MethodGet
			in = nil
		}
		if err := request(method, path, in, &raw); err != nil {
			code := fail(err)
			rc = code
			outcomes = append(outcomes, outcome{Node: node, Code: code})
			continue
		}
		var job harnessops.Job
		if err := json.Unmarshal(raw, &job); err != nil {
			rc = fail(err)
			continue
		}
		for _, warning := range job.Warnings {
			fmt.Fprintln(stderr, "Warning:", updateCell(warning))
		}
		if !p.NoWait && p.Job == "" {
			deadline := time.Now().Add(31 * time.Minute)
			phase := ""
			for time.Now().Before(deadline) && (job.State == "running" || job.State == "waiting_idle") {
				if job.Phase != phase {
					fmt.Fprintf(stderr, "Harness job %s: %s\n", job.ID, updateCell(job.Phase))
					phase = job.Phase
				}
				time.Sleep(time.Second)
				if err := request(http.MethodGet, harnessOperationPath(node, "/jobs/"+job.ID), nil, &raw); err != nil {
					nodeRC = fail(err)
					rc = nodeRC
					break
				}
				if err := json.Unmarshal(raw, &job); err != nil {
					nodeRC = fail(err)
					rc = nodeRC
					break
				}
			}
			if job.State == "running" || job.State == "waiting_idle" {
				fmt.Fprintf(stderr, "Inspect job later with --job %s\n", job.ID)
				rc = rcIOFailure
				nodeRC = rcIOFailure
			}
		}
		code := nodeRC
		if job.State == "failed" {
			code = rcIOFailure
			rc = code
		}
		outcomes = append(outcomes, outcome{Node: node, Job: raw, Code: code})
		if !p.JSON {
			label := node
			if label == "" {
				label = "local"
			}
			fmt.Fprintf(stdout, "%s %s: %s (%s)\n", updateCell(label), action, updateCell(job.State), job.ID)
			for _, r := range job.Results {
				fmt.Fprintf(stdout, "  %s: %s\n", updateCell(r.Harness), updateCell(r.State))
				if r.Error != "" {
					fmt.Fprintln(stderr, updateCell(r.Error))
				}
				if r.ManualCommand != "" {
					fmt.Fprintln(stdout, "  Manual:", updateCell(r.ManualCommand))
				}
				if r.Credentials != nil {
					fmt.Fprintln(stdout, "  Credential backup:", updateCell(r.Credentials.BackupLocation))
				}
			}
		}
	}
	if p.JSON {
		var out any = outcomes
		if !p.AllNodes && len(outcomes) == 1 && len(outcomes[0].Job) > 0 {
			out = outcomes[0].Job
		}
		if err := json.NewEncoder(stdout).Encode(out); err != nil {
			return rcIOFailure
		}
	}
	return rc
}
