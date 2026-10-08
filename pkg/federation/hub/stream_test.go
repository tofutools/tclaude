package hub_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/tofutools/tclaude/pkg/federation/client"
	"github.com/tofutools/tclaude/pkg/federation/hub"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/federation/stream"
)

func streamPair(t *testing.T, cfg hub.Config) (*peer, *peer, *proto.Identity, *proto.Identity, *hub.Hub) {
	t.Helper()
	h, st, url := newHub(t, cfg)
	a, _ := proto.NewIdentity()
	b, _ := proto.NewIdentity()
	_ = st.Admit(a.ID(), "red")
	_ = st.Admit(b.ID(), "red")
	pa := startPeer(t, url, "a", "", a)
	pb := startPeer(t, url, "b", "", b)
	eventually(t, "a sees b", func() bool { return online(pa, b.ID()) })
	eventually(t, "b connected", func() bool { return pb.status().State == client.StateConnected })
	return pa, pb, a, b, h
}

type dialResult struct {
	c   *stream.Conn
	err error
}

// openStream dials both ends of sid concurrently and wraps them.
func openStream(t *testing.T, pa, pb *peer, a, b *proto.Identity, sid string) (*stream.Conn, *stream.Conn) {
	t.Helper()
	ka, _ := stream.NewKeyPair()
	kb, _ := stream.NewKeyPair()
	dial := func(p *peer, other string, kp *stream.KeyPair, peerPub []byte, initiator bool, out chan<- dialResult) {
		ws, err := p.cl.DialStream(context.Background(), sid, other)
		if err != nil {
			out <- dialResult{err: err}
			return
		}
		keys, err := stream.DeriveKeys(kp, peerPub, sid, initiator)
		if err != nil {
			out <- dialResult{err: err}
			return
		}
		c, err := stream.New(ws, keys)
		out <- dialResult{c: c, err: err}
	}
	ra, rb := make(chan dialResult, 1), make(chan dialResult, 1)
	go dial(pa, b.ID(), ka, kb.Pub, true, ra)
	go dial(pb, a.ID(), kb, ka.Pub, false, rb)
	x, y := <-ra, <-rb
	if x.err != nil || y.err != nil {
		t.Fatalf("dial: %v / %v", x.err, y.err)
	}
	t.Cleanup(func() { _ = x.c.Close(); _ = y.c.Close() })
	return x.c, y.c
}

func TestStreamRelayRoundTrip(t *testing.T) {
	pa, pb, a, b, _ := streamPair(t, hub.Config{})
	ca, cb := openStream(t, pa, pb, a, b, proto.NewEnvelopeID())

	payload := make([]byte, 300<<10)
	_, _ = rand.Read(payload)
	go func() {
		_, _ = ca.Write(payload)
		_ = ca.CloseWrite()
	}()
	got, err := io.ReadAll(cb)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("payload mismatch: %d vs %d bytes", len(got), len(payload))
	}
	// The other direction still works after a half close.
	go func() { _, _ = cb.Write([]byte("pong")); _ = cb.CloseWrite() }()
	back, err := io.ReadAll(ca)
	if err != nil || string(back) != "pong" {
		t.Fatalf("reverse: %q %v", back, err)
	}
}

func TestStreamRelayLimitsAndVisibility(t *testing.T) {
	pa, pb, a, b, h := streamPair(t, hub.Config{MaxStreams: 1, StreamWait: 300 * time.Millisecond})
	openStream(t, pa, pb, a, b, proto.NewEnvelopeID())
	eventually(t, "two stream dialers", func() bool { return h.StreamCount() == 2 })

	// a already has its one stream.
	_, err := pa.cl.DialStream(context.Background(), proto.NewEnvelopeID(), b.ID())
	var refused *client.RefusedError
	if !errors.As(err, &refused) || refused.Code != proto.CodeStreamLimit {
		t.Fatalf("second stream: %v", err)
	}

	// An instance outside the shared space cannot be named as the peer.
	c, _ := proto.NewIdentity()
	_, err = pb.cl.DialStream(context.Background(), proto.NewEnvelopeID(), c.ID())
	if !errors.As(err, &refused) || refused.Code != proto.CodeNotVisible {
		t.Fatalf("invisible peer: %v", err)
	}
}

func TestStreamRelayPeerMustJoin(t *testing.T) {
	pa, pb, a, b, _ := streamPair(t, hub.Config{StreamWait: 200 * time.Millisecond})
	_, err := pa.cl.DialStream(context.Background(), proto.NewEnvelopeID(), b.ID())
	var refused *client.RefusedError
	if !errors.As(err, &refused) || refused.Code != proto.CodeStreamWait {
		t.Fatalf("lonely dial: %v", err)
	}
	// A dialer naming the wrong peer cannot join someone else's stream.
	sid := proto.NewEnvelopeID()
	done := make(chan error, 1)
	go func() { _, err := pa.cl.DialStream(context.Background(), sid, b.ID()); done <- err }()
	time.Sleep(50 * time.Millisecond)
	x, _ := proto.NewIdentity()
	if _, err := pb.cl.DialStream(context.Background(), sid, x.ID()); err == nil {
		t.Fatal("mismatched peer joined")
	}
	_ = a
	<-done
}
