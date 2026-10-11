package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"io"
	"net/http"
	"net/url"
	"strings"
	"text/tabwriter"
	"time"
)

// ReadPeerView uses the local operator API and its existing pinned transport.
// Endpoint is a relative dashboard tail, optionally including a query string.
func ReadPeerView(node, endpoint string, out any) error {
	if strings.TrimSpace(node) == "" {
		return fmt.Errorf("--node is required")
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.IsAbs() || u.Host != "" || u.Fragment != "" || strings.HasPrefix(endpoint, "/") {
		return fmt.Errorf("endpoint must be a relative dashboard tail")
	}
	for _, part := range strings.Split(u.Path, "/") {
		if part == ".." || part == "." || part == "" {
			return fmt.Errorf("endpoint must not contain empty or dot path segments")
		}
	}
	return DaemonRequest(http.MethodGet, "/v1/federation/peer/"+url.PathEscape(node)+"/"+u.RequestURI(), nil, out,
		DaemonOpts{Timeout: 20 * time.Second, NoRetry: true})
}

// Keep the remote projection and its omission metadata together. Zeroed
// presence fields can mean withheld data; they are never inferred as offline.
func runPeerListing(node, kind, group string, asJSON bool, stdout, stderr io.Writer) int {
	var payload map[string]json.RawMessage
	endpoint := "snapshot"
	if group != "" {
		endpoint = "groups/" + url.PathEscape(group)
	}
	if err := ReadPeerView(node, endpoint, &payload); err != nil {
		var de *DaemonError
		if errors.As(err, &de) && json.Valid(de.Raw) {
			fmt.Fprintln(stderr, string(de.Raw))
		} else {
			fmt.Fprintf(stderr, "Error: %v\n", err)
		}
		return MapDaemonErrorToRC(err)
	}
	var rows []json.RawMessage
	if group != "" {
		var g struct {
			Members []json.RawMessage `json:"members"`
		}
		if err := json.Unmarshal(payload["group"], &g); err != nil {
			fmt.Fprintln(stderr, err)
			return rcIOFailure
		}
		rows = g.Members
	} else {
		if err := json.Unmarshal(payload[kind], &rows); err != nil {
			fmt.Fprintln(stderr, err)
			return rcIOFailure
		}
	}
	if rows == nil {
		rows = []json.RawMessage{}
	}
	if asJSON {
		if err := json.NewEncoder(stdout).Encode(map[string]any{kind: rows, "peer_view": payload["peer_view"]}); err != nil {
			return rcIOFailure
		}
		return 0
	}
	fmt.Fprintln(stdout, "Peer-authorized projection; blank status and zero counts may be withheld.")
	tw := tabwriter.NewWriter(stdout, 0, 2, 2, ' ', 0)
	if kind == "groups" {
		fmt.Fprintln(tw, "GROUP\tSHARED MEMBERS\tREPORTED ONLINE")
	} else {
		fmt.Fprintln(tw, "AGENT\tTITLE\tREPORTED STATUS")
	}
	for _, raw := range rows {
		if kind == "groups" {
			var g struct {
				Name    string            `json:"name"`
				Members []json.RawMessage `json:"members"`
				Online  int               `json:"online"`
			}
			if err := json.Unmarshal(raw, &g); err != nil {
				fmt.Fprintln(stderr, err)
				return rcIOFailure
			}
			fmt.Fprintf(tw, "%s\t%d\t%d\n", peerViewCell(g.Name), len(g.Members), g.Online)
		} else {
			var a peerEntry
			if err := json.Unmarshal(raw, &a); err != nil {
				fmt.Fprintln(stderr, err)
				return rcIOFailure
			}
			status := a.State.Status
			if status == "" {
				status = "not reported"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\n", peerViewCell(a.AgentID), peerViewCell(a.Title), peerViewCell(status))
		}
	}
	if err := tw.Flush(); err != nil {
		return rcIOFailure
	}
	return 0
}
func peerViewCell(s string) string { return strings.Join(strings.Fields(proto.StripControls(s)), " ") }
