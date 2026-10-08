package federationcmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/agent"
)

type spawnPlacementReadout struct {
	EnvelopeID string `json:"envelope_id,omitempty"`
	To         string `json:"to,omitempty"`
	State      string `json:"state,omitempty"`
	Connected  bool   `json:"hub_connected"`
	Code       string `json:"code,omitempty"`
	Error      string `json:"error,omitempty"`
	Placement  *struct {
		Prefer     string `json:"prefer"`
		Selected   string `json:"selected,omitempty"`
		Candidates []struct {
			Peer         string   `json:"peer"`
			Instance     string   `json:"instance"`
			Group        string   `json:"group,omitempty"`
			Eligible     bool     `json:"eligible"`
			Reason       string   `json:"reason,omitempty"`
			Attempt      string   `json:"attempt,omitempty"`
			LoadPerCore  *float64 `json:"load_per_core,omitempty"`
			RAMAvailable *uint64  `json:"ram_available_bytes,omitempty"`
		} `json:"candidates"`
	} `json:"placement,omitempty"`
}

func runSpawnRequest(p *spawnRequestParams, stdout, stderr io.Writer) int {
	req := map[string]any{"credentials": p.Credentials, "brief": p.Brief, "name": p.Name, "role": p.Role}
	if p.Node != "" {
		if p.Target != "" {
			return fail(stderr, fmt.Errorf("target and --node are mutually exclusive"))
		}
		req["node"], req["group"], req["require"], req["prefer"] = p.Node, p.Group, p.Require, p.Prefer
	} else {
		i := strings.LastIndex(p.Target, "@")
		if i <= 0 || i == len(p.Target)-1 {
			return fail(stderr, fmt.Errorf("target must be <group>@<peer>, or pass --node auto|group:<pool>"))
		}
		if p.Group != "" || p.Require != "" || p.Prefer != "" {
			return fail(stderr, fmt.Errorf("--group, --require and --prefer need --node"))
		}
		req["group"], req["peer"] = p.Target[:i], p.Target[i+1:]
	}
	if rc := agent.RequireDaemonOrExit(stderr); rc != 0 {
		return rc
	}
	var raw json.RawMessage
	err := agent.DaemonRequest(http.MethodPost, "/v1/federation/spawn-requests", req, &raw, agent.DaemonOpts{})
	if err != nil {
		var daemonErr *agent.DaemonError
		if errors.As(err, &daemonErr) && len(daemonErr.Raw) > 0 {
			raw = daemonErr.Raw
		}
	}
	var out spawnPlacementReadout
	_ = json.Unmarshal(raw, &out)
	if p.JSON && len(raw) > 0 {
		fmt.Fprintln(stdout, string(raw))
	} else {
		renderPlacement(out, stdout)
		if out.EnvelopeID != "" {
			if p.Credentials != "" {
				fmt.Fprintf(stdout, "credentials: %s\n", p.Credentials)
			}
			fmt.Fprintf(stdout, "spawn request %s to %s (envelope %.12s); the decision will arrive in your inbox\n", out.State, out.To, out.EnvelopeID)
			if !out.Connected {
				fmt.Fprintln(stderr, "hub not connected; the request will be sent when it is")
			}
		}
	}
	if err != nil {
		return fail(stderr, err)
	}
	return 0
}
func renderPlacement(out spawnPlacementReadout, w io.Writer) {
	if out.Placement == nil {
		return
	}
	fmt.Fprintf(w, "Placement (%s):\n", out.Placement.Prefer)
	for _, row := range out.Placement.Candidates {
		reason := row.Reason
		if reason == "" {
			reason = "eligible"
		}
		if row.Instance == out.Placement.Selected {
			reason = "selected; " + reason
		}
		if row.Attempt != "" {
			reason += " (" + row.Attempt + ")"
		}
		target := row.Peer
		if row.Group != "" {
			target = row.Group + "@" + target
		}
		metric := ""
		if row.LoadPerCore != nil {
			metric = fmt.Sprintf("; load/core %.3f", *row.LoadPerCore)
		}
		if row.RAMAvailable != nil {
			metric = fmt.Sprintf("; RAM available %d bytes", *row.RAMAvailable)
		}
		fmt.Fprintf(w, "  %s: %s%s\n", target, reason, metric)
	}
}
