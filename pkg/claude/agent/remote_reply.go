package agent

import (
	"fmt"
	"io"

	"github.com/tofutools/tclaude/pkg/federation/proto"
)

// remoteMarker is how the daemon marks a reply that a peer wrote: a
// member@peer verb's reply, or a peer's fleet brief.
type remoteMarker struct {
	Target    string `json:"target"`
	Untrusted bool   `json:"untrusted"`
}

// remoteReply is embedded in a verb's reply: set when the verb ran on a
// peer's agent (member@peer).
type remoteReply struct {
	Remote     *remoteMarker `json:"remote,omitempty"`
	RemoteNote string        `json:"remote_note,omitempty"`
}

// printRemoteBanner says, before any of the reply is printed, that what
// follows is the peer's own account and untrusted.
func (r *remoteReply) printRemoteBanner(w io.Writer) {
	if r.Remote != nil {
		fmt.Fprintf(w, "Remote agent %s, as its node reports it (untrusted content):\n", proto.SafeName(r.Remote.Target, true))
	}
}

// printRemoteNote says what the remote verb left out.
func (r *remoteReply) printRemoteNote(w io.Writer) {
	if r.RemoteNote != "" {
		fmt.Fprintf(w, "  note: %s\n", proto.StripControls(r.RemoteNote))
	}
}
