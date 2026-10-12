package agent

import (
	"fmt"
	"io"
	"net/url"
	"time"
)

// awaitAgentSummary asks an agent to write a summary of its earlier work
// for an export or snapshot, and waits for it. It returns the summary
// request's id for the export to carry.
func awaitAgentSummary(agentSel, wait string, stderr io.Writer) (string, int) {
	timeout := 10 * time.Minute
	if wait != "" {
		d, err := time.ParseDuration(wait)
		if err != nil || d <= 0 || d > 15*time.Minute {
			fmt.Fprintln(stderr, "Error: --summary-wait must be a duration up to 15m")
			return "", rcInvalidArg
		}
		timeout = d
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return "", rc
	}
	var req struct {
		ID string `json:"id"`
	}
	if err := DaemonPost("/v1/agent-bundle/summary", map[string]any{"agent": agentSel}, &req); err != nil {
		fmt.Fprintln(stderr, err)
		return "", MapDaemonErrorToRC(err)
	}
	fmt.Fprintf(stderr, "Asked %s for a summary of its earlier work; waiting up to %s...\n", termClean(agentSel), timeout)
	deadline := time.Now().Add(timeout)
	for {
		var st struct {
			State string `json:"state"`
		}
		if err := DaemonGet("/v1/agent-bundle/summary/"+url.PathEscape(req.ID), &st); err != nil {
			fmt.Fprintln(stderr, err)
			return "", MapDaemonErrorToRC(err)
		}
		if st.State == "ready" {
			fmt.Fprintln(stderr, "Summary received.")
			return req.ID, rcOK
		}
		if time.Now().After(deadline) {
			fmt.Fprintln(stderr, "Error: no summary arrived in time; the agent may be busy or offline. Retry, or export without --with-summary.")
			return "", rcIOFailure
		}
		time.Sleep(3 * time.Second)
	}
}
