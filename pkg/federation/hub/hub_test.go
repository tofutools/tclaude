package hub_test

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tofutools/tclaude/pkg/federation/client"
	"github.com/tofutools/tclaude/pkg/federation/hub"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

type peer struct {
	id  *proto.Identity
	cl  *client.Client
	mu  sync.Mutex
	got []*proto.Sealed
	dir []proto.DirectoryEntry
	st  client.Status
}

func (p *peer) received() []*proto.Sealed {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]*proto.Sealed(nil), p.got...)
}

func (p *peer) directory() []proto.DirectoryEntry {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]proto.DirectoryEntry(nil), p.dir...)
}

func (p *peer) status() client.Status {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.st
}

func newHub(t *testing.T, cfg hub.Config) (*hub.Hub, *hub.Store, string) {
	t.Helper()
	st, err := hub.OpenStore(filepath.Join(t.TempDir(), "hub.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	h, err := hub.New(st, cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h.Handler())
	t.Cleanup(func() { h.Close(); srv.Close(); _ = st.Close() })
	return h, st, "ws" + strings.TrimPrefix(srv.URL, "http")
}

func startPeer(t *testing.T, url, name, invite string, id *proto.Identity) *peer {
	t.Helper()
	if id == nil {
		id, _ = proto.NewIdentity()
	}
	p := &peer{id: id}
	cl, err := client.New(client.Options{
		URL: url, Identity: id, Name: name, Invite: invite, MaxBackoff: 200 * time.Millisecond,
		OnDeliver: func(_ string, s *proto.Sealed) { p.mu.Lock(); p.got = append(p.got, s); p.mu.Unlock() },
		OnDirectory: func(d []proto.DirectoryEntry) {
			p.mu.Lock()
			p.dir = d
			p.mu.Unlock()
		},
		OnState: func(s client.Status) { p.mu.Lock(); p.st = s; p.mu.Unlock() },
	})
	if err != nil {
		t.Fatal(err)
	}
	p.cl = cl
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { cl.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return p
}

func eventually(t *testing.T, what string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func online(p *peer, id string) bool {
	for _, e := range p.directory() {
		if e.InstanceID == id && e.Online {
			return true
		}
	}
	return false
}

func TestRouteBetweenAdmittedInstances(t *testing.T) {
	_, st, url := newHub(t, hub.Config{})
	a, _ := proto.NewIdentity()
	b, _ := proto.NewIdentity()
	if err := st.Admit(a.ID()); err != nil {
		t.Fatal(err)
	}
	if err := st.Admit(b.ID()); err != nil {
		t.Fatal(err)
	}
	pa := startPeer(t, url, "alice", "", a)
	pb := startPeer(t, url, "bob", "", b)
	eventually(t, "a sees b online", func() bool { return online(pa, b.ID()) })

	env, _ := proto.NewEnvelope(a, proto.KindMail, proto.Endpoint{}, proto.Endpoint{Instance: b.ID()}, time.Hour, proto.MailPayload{Body: "hi"})
	sealed, _ := proto.Seal(a, env, b.Pub)
	res, err := pa.cl.Send(context.Background(), b.ID(), sealed)
	if err != nil || res.Status != proto.SendDelivered {
		t.Fatalf("send: %+v %v", res, err)
	}
	eventually(t, "b receives", func() bool { return len(pb.received()) == 1 })
	got, err := proto.Open(pb.received()[0], a.Pub, b, time.Now())
	if err != nil || got.ID != env.ID {
		t.Fatalf("open: %v", err)
	}
}

func TestNotAdmittedRefused(t *testing.T) {
	_, _, url := newHub(t, hub.Config{})
	p := startPeer(t, url, "stranger", "", nil)
	eventually(t, "refused", func() bool { return p.status().State == client.StateRefused })
	if !strings.Contains(p.status().LastError, proto.CodeNotAdmitted) {
		t.Fatalf("error = %q", p.status().LastError)
	}
}

func TestInviteAdmitsOnce(t *testing.T) {
	_, st, url := newHub(t, hub.Config{})
	tok, err := st.CreateInvite("team", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	p1 := startPeer(t, url, "one", tok, nil)
	eventually(t, "p1 connected", func() bool { return p1.status().State == client.StateConnected })
	if sp := p1.status().Spaces; len(sp) != 1 || sp[0] != "team" {
		t.Fatalf("spaces = %v", sp)
	}
	p2 := startPeer(t, url, "two", tok, nil)
	eventually(t, "p2 refused", func() bool { return p2.status().State == client.StateRefused })
}

func TestSpacesScopeVisibility(t *testing.T) {
	h, st, url := newHub(t, hub.Config{})
	a, _ := proto.NewIdentity()
	b, _ := proto.NewIdentity()
	c, _ := proto.NewIdentity()
	_ = st.Admit(a.ID(), "red")
	_ = st.Admit(b.ID(), "red")
	_ = st.Admit(c.ID(), "blue")
	pa := startPeer(t, url, "a", "", a)
	startPeer(t, url, "b", "", b)
	pc := startPeer(t, url, "c", "", c)
	eventually(t, "a sees b", func() bool { return online(pa, b.ID()) })
	eventually(t, "c connected", func() bool { return pc.status().State == client.StateConnected })
	for _, e := range pa.directory() {
		if e.InstanceID == c.ID() {
			t.Fatal("a sees c across spaces")
		}
	}
	env, _ := proto.NewEnvelope(a, proto.KindMail, proto.Endpoint{}, proto.Endpoint{Instance: c.ID()}, time.Hour, proto.MailPayload{Body: "x"})
	sealed, _ := proto.Seal(a, env, c.Pub)
	res, err := pa.cl.Send(context.Background(), c.ID(), sealed)
	if err != nil || res.Status != proto.SendRefused || res.Code != proto.CodeNotVisible {
		t.Fatalf("cross-space send: %+v %v", res, err)
	}

	// Revocation drops the live connection.
	if err := st.Revoke(a.ID()); err != nil {
		t.Fatal(err)
	}
	h.RefreshPolicy()
	eventually(t, "a refused after revoke", func() bool { return pa.status().State == client.StateRefused })
}

func TestRateLimit(t *testing.T) {
	_, st, url := newHub(t, hub.Config{FramesPerMinute: 2})
	a, _ := proto.NewIdentity()
	b, _ := proto.NewIdentity()
	_ = st.Admit(a.ID())
	_ = st.Admit(b.ID())
	pa := startPeer(t, url, "a", "", a)
	startPeer(t, url, "b", "", b)
	eventually(t, "a sees b", func() bool { return online(pa, b.ID()) })
	var last string
	for i := 0; i < 3; i++ {
		env, _ := proto.NewEnvelope(a, proto.KindMail, proto.Endpoint{}, proto.Endpoint{Instance: b.ID()}, time.Hour, proto.MailPayload{Body: "x"})
		sealed, _ := proto.Seal(a, env, b.Pub)
		res, err := pa.cl.Send(context.Background(), b.ID(), sealed)
		if err != nil {
			t.Fatal(err)
		}
		last = res.Status
	}
	if last != proto.SendRateLimited {
		t.Fatalf("third send status = %s, want rate_limited", last)
	}
}

func TestOfflineTarget(t *testing.T) {
	_, st, url := newHub(t, hub.Config{})
	a, _ := proto.NewIdentity()
	b, _ := proto.NewIdentity()
	_ = st.Admit(a.ID())
	_ = st.Admit(b.ID())
	pa := startPeer(t, url, "a", "", a)
	eventually(t, "a connected", func() bool { return pa.status().State == client.StateConnected })
	env, _ := proto.NewEnvelope(a, proto.KindMail, proto.Endpoint{}, proto.Endpoint{Instance: b.ID()}, time.Hour, proto.MailPayload{Body: "x"})
	sealed, _ := proto.Seal(a, env, b.Pub)
	res, err := pa.cl.Send(context.Background(), b.ID(), sealed)
	if err != nil || res.Status != proto.SendOffline {
		t.Fatalf("offline send: %+v %v", res, err)
	}
}

func TestClientRejectsPlainRemoteWS(t *testing.T) {
	if _, err := client.ValidateURL("ws://example.com/x"); err == nil {
		t.Fatal("plain ws to remote host accepted")
	}
	if _, err := client.ValidateURL("ws://127.0.0.1:1"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.ValidateURL("wss://hub.example.com"); err != nil {
		t.Fatal(err)
	}
}

func TestRevokedCannotRedeemInvite(t *testing.T) {
	_, st, url := newHub(t, hub.Config{})
	id, _ := proto.NewIdentity()
	_ = st.Admit(id.ID())
	if err := st.Revoke(id.ID()); err != nil {
		t.Fatal(err)
	}
	tok, _ := st.CreateInvite("default", time.Hour)
	p := startPeer(t, url, "revoked", tok, id)
	eventually(t, "refused", func() bool { return p.status().State == client.StateRefused })
	if !strings.Contains(p.status().LastError, "revoked") {
		t.Fatalf("error = %q", p.status().LastError)
	}
}
