package agent

import (
	"bytes"
	"strings"
	"testing"
)

func TestPeerReachEditNeedsOneHolderAPeerAndASlug(t *testing.T) {
	for name, p := range map[string]peerReachEditParams{
		"no holder":   {Peer: "bob/builders", Slug: []string{"message.direct"}},
		"two holders": {Target: "lead", Group: "ops", Peer: "bob/builders", Slug: []string{"message.direct"}},
		"no peer":     {Target: "lead", Slug: []string{"message.direct"}},
		"no slug":     {Target: "lead", Peer: "bob/builders"},
	} {
		var out, errb bytes.Buffer
		if rc := runPeerReachEdit(&p, false, &out, &errb); rc != rcInvalidArg || !strings.Contains(errb.String(), "Error:") {
			t.Errorf("%s: rc %d, stderr %q", name, rc, errb.String())
		}
	}
}

func TestPeerReachTableNamesHolders(t *testing.T) {
	var out bytes.Buffer
	printPeerReach(peerReachResp{Rows: []peerReachRow{
		{Kind: "group", Group: "ops", Slug: "message.direct", Peers: []string{"inst_bob/builders"}},
		{Kind: "agent", AgentID: "agt_abcdef123456", Title: "lead", Slug: "sessions.watch", Peers: []string{"inst_bob"}},
	}}, false, &out)
	for _, want := range []string{"group ops", "lead (", "inst_bob/builders", "sessions.watch"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("table lacks %q:\n%s", want, out.String())
		}
	}
}
