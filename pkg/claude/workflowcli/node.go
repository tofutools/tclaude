package workflowcli

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/common"
)

// The control verbs (new, node, cancel, rm) drive an instance through the
// daemon's mutating /v1/workflows* endpoints. The node settle path is the
// load-bearing one — the engine observes the node-status flip the same verb
// produces — so its request/response shape is frozen with the agentd side:
// PATCH /v1/workflows/{id}/nodes/{node} {status,outcome?,output?} →
// {ok,node_id,status,instance_status,ready[],skipped[]}.
//
// There is deliberately no `skip` action: a direct hop to "skipped" would
// bypass the engine's Advance (the #230 isManualDriveStatus guard rejects it
// server-side) and strand the sub-tree behind the node. Branch-skipping is what
// Advance does on a settle; skipping a whole instance is `cancel`.

// --- node <inst> <node> {start|done|fail} ---

type nodeParams struct {
	Instance string `pos:"true" help:"Workflow instance id"`
	Node     string `pos:"true" help:"Node id within the instance"`
	Action   string `pos:"true" help:"start | done | fail"`
	Outcome  string `long:"outcome" optional:"true" help:"Outcome for 'done' (validated against the node's allowed outcomes; required for enum-verified nodes)"`
	Output   string `long:"output" optional:"true" help:"Attach a captured-output summary to the node"`
	JSON     bool   `long:"json" help:"Output JSON"`
}

func nodeCmd() *cobra.Command {
	return boa.CmdT[nodeParams]{
		Use:   "node",
		Short: "Drive a node: start | done [--outcome v] | fail",
		Long: "Drive one node of a workflow instance.\n\n" +
			"  start          mark the node running\n" +
			"  done           settle the node done (use --outcome to pick the branch)\n" +
			"  fail           settle the node failed (halts the instance unless on_fail: continue)\n\n" +
			"There is no 'skip' — branch-skipping happens automatically when a node\n" +
			"settles; to abandon a whole instance use `tclaude workflow cancel`.",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *nodeParams, _ *cobra.Command, _ []string) {
			os.Exit(runNode(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

// nodeActionStatus maps a CLI action to the node status the PATCH carries.
func nodeActionStatus(action string) (string, bool) {
	switch action {
	case "start":
		return "running", true
	case "done":
		return "done", true
	case "fail":
		return "failed", true
	default:
		return "", false
	}
}

func runNode(p *nodeParams, stdout, stderr io.Writer) int {
	id, rc := parseInstanceID(p.Instance, stderr)
	if rc != rcOK {
		return rc
	}
	node := strings.TrimSpace(p.Node)
	if node == "" {
		fmt.Fprintln(stderr, "Error: node id is required")
		return rcInvalidArg
	}
	action := strings.TrimSpace(p.Action)
	status, ok := nodeActionStatus(action)
	if !ok {
		fmt.Fprintf(stderr, "Error: unknown action %q (use start, done, or fail)\n", p.Action)
		return rcInvalidArg
	}
	// --outcome only makes sense for `done`: start isn't settling, and fail is
	// always OutcomeFail server-side. Reject it elsewhere so a mistaken flag is
	// caught loudly instead of being silently ignored.
	if strings.TrimSpace(p.Outcome) != "" && action != "done" {
		fmt.Fprintf(stderr, "Error: --outcome is only valid with the 'done' action\n")
		return rcInvalidArg
	}

	if rc := agent.RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}

	body := map[string]string{"status": status}
	if action == "done" && strings.TrimSpace(p.Outcome) != "" {
		body["outcome"] = p.Outcome
	}
	if p.Output != "" {
		body["output"] = p.Output
	}

	var resp nodePatchResp
	path := "/v1/workflows/" + strconv.FormatInt(id, 10) + "/nodes/" + node
	if err := agent.DaemonRequest(http.MethodPatch, path, body, &resp, agent.DaemonOpts{}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcForDaemonErr(err)
	}
	if p.JSON {
		return writeJSON(stdout, stderr, resp)
	}
	fmt.Fprintf(stdout, "node %s → %s (instance: %s)\n", resp.NodeID, resp.Status, resp.InstanceStatus)
	if len(resp.Ready) > 0 {
		fmt.Fprintf(stdout, "  readied: %s\n", strings.Join(resp.Ready, ", "))
	}
	if len(resp.Skipped) > 0 {
		fmt.Fprintf(stdout, "  skipped: %s\n", strings.Join(resp.Skipped, ", "))
	}
	return rcOK
}

type nodePatchResp struct {
	OK             bool     `json:"ok"`
	NodeID         string   `json:"node_id"`
	Status         string   `json:"status"`
	InstanceStatus string   `json:"instance_status"`
	Ready          []string `json:"ready"`
	Skipped        []string `json:"skipped"`
}

// --- new <ref> [--param k=v]... [--title T] [--group G] ---

type newParams struct {
	Ref   string   `pos:"true" help:"Template reference to instantiate (name, or project:/user:/example: qualified)"`
	Param []string `long:"param" help:"Instantiation parameter as key=value (repeatable)"`
	Title string   `long:"title" optional:"true" help:"Instance title (defaults to the template name)"`
	Group string   `long:"group" optional:"true" help:"Bind the instance to an existing agent group by name"`
	JSON  bool     `long:"json" help:"Output JSON"`
}

func newCmd() *cobra.Command {
	return boa.CmdT[newParams]{
		Use:         "new",
		Short:       "Instantiate a template into a running workflow instance",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *newParams, _ *cobra.Command, _ []string) {
			os.Exit(runNew(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runNew(p *newParams, stdout, stderr io.Writer) int {
	params, err := parseParams(p.Param)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcInvalidArg
	}
	if rc := agent.RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	body := map[string]any{
		"template_ref": p.Ref,
		"title":        p.Title,
		"params":       params,
		"group":        p.Group,
	}
	var resp struct {
		ID      int64 `json:"id"`
		GroupID int64 `json:"group_id"`
	}
	if err := agent.DaemonRequest(http.MethodPost, "/v1/workflows", body, &resp, agent.DaemonOpts{}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcForDaemonErr(err)
	}
	if p.JSON {
		return writeJSON(stdout, stderr, resp)
	}
	fmt.Fprintf(stdout, "created instance #%d\n", resp.ID)
	return rcOK
}

// parseParams turns repeated key=value flags into a JSON-object-shaped map.
// Values are taken verbatim as strings (template params interpolate as text);
// an entry without '=' or with an empty key is an error.
func parseParams(kvs []string) (map[string]string, error) {
	out := map[string]string{}
	for _, kv := range kvs {
		k, v, ok := strings.Cut(kv, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" {
			return nil, fmt.Errorf("param %q is not in key=value form", kv)
		}
		out[k] = v
	}
	return out, nil
}

// --- cancel <inst> ---

type cancelParams struct {
	Instance string `pos:"true" help:"Workflow instance id"`
	JSON     bool   `long:"json" help:"Output JSON"`
}

func cancelCmd() *cobra.Command {
	return boa.CmdT[cancelParams]{
		Use:         "cancel",
		Short:       "Cancel a running instance (skips every non-terminal node)",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *cancelParams, _ *cobra.Command, _ []string) {
			os.Exit(runCancel(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runCancel(p *cancelParams, stdout, stderr io.Writer) int {
	id, rc := parseInstanceID(p.Instance, stderr)
	if rc != rcOK {
		return rc
	}
	if rc := agent.RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	var resp struct {
		OK             bool   `json:"ok"`
		InstanceStatus string `json:"instance_status"`
	}
	path := "/v1/workflows/" + strconv.FormatInt(id, 10) + "/cancel"
	if err := agent.DaemonRequest(http.MethodPost, path, nil, &resp, agent.DaemonOpts{}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcForDaemonErr(err)
	}
	if p.JSON {
		return writeJSON(stdout, stderr, resp)
	}
	fmt.Fprintf(stdout, "cancelled instance #%d (status: %s)\n", id, resp.InstanceStatus)
	return rcOK
}

// --- rm <inst> ---

type rmParams struct {
	Instance string `pos:"true" help:"Workflow instance id"`
	JSON     bool   `long:"json" help:"Output JSON"`
}

func rmCmd() *cobra.Command {
	return boa.CmdT[rmParams]{
		Use:         "rm",
		Short:       "Delete an instance and its nodes/events",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *rmParams, _ *cobra.Command, _ []string) {
			os.Exit(runRm(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runRm(p *rmParams, stdout, stderr io.Writer) int {
	id, rc := parseInstanceID(p.Instance, stderr)
	if rc != rcOK {
		return rc
	}
	if rc := agent.RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	path := "/v1/workflows/" + strconv.FormatInt(id, 10)
	if err := agent.DaemonRequest(http.MethodDelete, path, nil, nil, agent.DaemonOpts{}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcForDaemonErr(err)
	}
	if p.JSON {
		return writeJSON(stdout, stderr, map[string]any{"ok": true, "id": id})
	}
	fmt.Fprintf(stdout, "removed instance #%d\n", id)
	return rcOK
}
