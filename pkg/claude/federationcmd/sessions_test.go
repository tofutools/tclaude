package federationcmd

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/tofutools/tclaude/pkg/federation/proto"
)

func TestSessionsWaitingNotifications(t *testing.T) {
	var out bytes.Buffer
	at := time.Now().UTC()
	s := remoteSession{CatalogSession: proto.CatalogSession{Agent: "agt_remote", Session: "runtime", Name: "remote", State: "working"}, Address: "agt_remote@bob", Instance: "inst_bob"}
	previous := notifySessionTransitions(&out, []remoteSession{s}, nil, true)
	if out.Len() != 0 {
		t.Fatal("initial listing must not alert")
	}
	s.State = "awaiting_permission"
	s.WaitingReason = "permission"
	s.WaitingObservedSince = &at
	previous = notifySessionTransitions(&out, []remoteSession{s}, previous, false)
	if !strings.Contains(out.String(), "\aagt_remote@bob (remote) started waiting: permission") {
		t.Fatalf("missing wait notification: %q", out.String())
	}
	out.Reset()
	previous = notifySessionTransitions(&out, []remoteSession{s}, previous, false)
	if out.Len() != 0 {
		t.Fatal("unchanged wait must not repeat")
	}
	s.Stale = true
	previous = notifySessionTransitions(&out, []remoteSession{s}, previous, false)
	s.Stale = false
	previous = notifySessionTransitions(&out, []remoteSession{s}, previous, false)
	if out.Len() != 0 {
		t.Fatal("reconnection must not repeat the same wait")
	}
	next := at.Add(time.Minute)
	s.WaitingObservedSince = &next
	notifySessionTransitions(&out, []remoteSession{s}, previous, false)
	if out.Len() == 0 {
		t.Fatal("a new wait with the same reason must alert")
	}
}

func TestSessionsStaleWaitingDuration(t *testing.T) {
	var out bytes.Buffer
	since := time.Now().Add(-time.Hour)
	printSessions(&out, []remoteSession{{CatalogSession: proto.CatalogSession{WaitingReason: "question", WaitingObservedSince: &since}, Stale: true, ObservedAt: since.Add(3 * time.Minute)}})
	if !strings.Contains(out.String(), "waiting ≥3m0s") || !strings.Contains(out.String(), "stale") {
		t.Fatalf("stale duration must freeze at snapshot: %s", out.String())
	}
}
