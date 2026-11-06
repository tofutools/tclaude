package processcmd

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

// --remote <peer> reads or starts process runs on a trusted peer node,
// through its peer view (S6): the peer's own grants decide (process.runs.read
// for reads, process.runs.start or groups.members.process for a start), and
// what it answers is untrusted remote content.

// processRemotePath is path (a /v1/process/... path) on the peer remote
// names, or path itself when remote is empty.
func processRemotePath(remote, path string) string {
	if remote = strings.TrimSpace(remote); remote == "" {
		return path
	}
	return "/v1/federation/peer/" + url.PathEscape(remote) + path
}

// processRequest is agent.DaemonRequest for path on this node or, with
// remote, on that peer. A peer's answer has its control characters stripped,
// keys and values, before it is decoded into out; its errors too.
func processRequest(remote, method, path string, body, out any, opts agent.DaemonOpts) error {
	remote = strings.TrimSpace(remote)
	if remote == "" {
		return agent.DaemonRequest(method, path, body, out, opts)
	}
	var raw any
	if err := agent.DaemonRequest(method, processRemotePath(remote, path), body, &raw, opts); err != nil {
		if clean := proto.StripControls(err.Error()); clean != err.Error() {
			return fmt.Errorf("%s", clean)
		}
		return err
	}
	clean, err := json.Marshal(stripControlsDeep(raw))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(clean, out); err != nil {
		return fmt.Errorf("the peer returned an invalid answer: %v", err)
	}
	return nil
}

// processRemoteMarker marks a peer's answer in --json output, as the agent
// CLI's remote marker does.
type processRemoteMarker struct {
	Target    string `json:"target"`
	Untrusted bool   `json:"untrusted"`
}

// printProcessRemoteBanner marks what follows as the peer's answer.
func printProcessRemoteBanner(w io.Writer, remote string) {
	if remote = strings.TrimSpace(remote); remote != "" {
		fmt.Fprintf(w, "Remote process runs of %s, as that node reports it (untrusted content)\n\n", proto.SafeName(remote, false))
	}
}

func stripControlsDeep(v any) any {
	switch t := v.(type) {
	case string:
		return proto.StripControls(t)
	case []any:
		for i := range t {
			t[i] = stripControlsDeep(t[i])
		}
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[proto.StripControls(k)] = stripControlsDeep(val)
		}
		return out
	}
	return v
}
