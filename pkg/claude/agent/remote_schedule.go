package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/tofutools/tclaude/pkg/federation/proto"
)

// --remote <peer> on `cron ls|logs|disable` and `triggers ls|show` reads or
// pauses a trusted peer's cron jobs and trigger rules through its peer view
// (S7a): the peer's grants decide (groups.messages.schedule,
// groups.members.schedule, groups.triggers.read), and what it answers is
// untrusted remote content.

// remotePath is path (a /v1/... path) on the peer remote names, or path
// itself when remote is empty.
func remotePath(remote, path string) string {
	if remote = strings.TrimSpace(remote); remote == "" {
		return path
	}
	return "/v1/federation/peer/" + url.PathEscape(remote) + path
}

// remoteRequest is DaemonRequest for path on this node or, with remote, on
// that peer. A peer's answer has its control characters stripped from its
// values, and put on one line before it is decoded into out; its errors
// are stripped too.
func remoteRequest(remote, method, path string, body, out any) error {
	return remoteRequestOpts(remote, method, path, body, out, DaemonOpts{})
}

// remoteRequestOpts is remoteRequest with daemon options (--ask-human, a
// longer timeout).
func remoteRequestOpts(remote, method, path string, body, out any, opts DaemonOpts) error {
	remote = strings.TrimSpace(remote)
	if remote == "" {
		return DaemonRequest(method, path, body, out, opts)
	}
	var raw any
	if err := DaemonRequest(method, remotePath(remote, path), body, &raw, opts); err != nil {
		if clean := proto.StripControls(err.Error()); clean != err.Error() {
			return fmt.Errorf("%s", clean)
		}
		return err
	}
	if out == nil || raw == nil {
		return nil
	}
	clean, err := json.Marshal(flattenDeep(stripControlsDeep(raw)))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(clean, out); err != nil {
		return fmt.Errorf("the peer returned an invalid answer: %v", err)
	}
	return nil
}

// printRemoteScheduleBanner marks what follows as the peer's answer.
func printRemoteScheduleBanner(w io.Writer, remote, what string) {
	if remote = strings.TrimSpace(remote); remote != "" {
		fmt.Fprintf(w, "Remote %s of %s, as that node reports it (untrusted content)\n\n", what, proto.SafeName(remote, false))
	}
}

// flattenDeep puts every string of a peer's answer on one line: a name, body
// or template it wrote cannot break a printed table row or forge a line under
// the banner.
func flattenDeep(v any) any {
	switch t := v.(type) {
	case string:
		return strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ", "\t", " ").Replace(t)
	case []any:
		for i := range t {
			t[i] = flattenDeep(t[i])
		}
	case map[string]any:
		for k, val := range t {
			t[k] = flattenDeep(val)
		}
	}
	return v
}

// remoteSuffix is " on <peer>" for a verb run on a linked node.
func remoteSuffix(remote string) string {
	if remote = strings.TrimSpace(remote); remote != "" {
		return " on " + proto.SafeName(remote, false)
	}
	return ""
}
