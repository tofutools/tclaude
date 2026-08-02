//go:build linux

package routeadapter

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/routebroker"
)

func TestValidateLoopbackEndpoints(t *testing.T) {
	tests := []struct {
		name, raw, wantErr string
	}{
		{name: "ipv4", raw: "tcp://127.0.0.1:43127"},
		{name: "ipv6", raw: "tcp://[::1]:43127"},
		{name: "hostname denied", raw: "tcp://localhost:43127", wantErr: "loopback"},
		{name: "private non-loopback denied", raw: "tcp://10.0.0.1:43127", wantErr: "loopback"},
		{name: "internet denied", raw: "tcp://1.1.1.1:443", wantErr: "loopback"},
		{name: "invalid port", raw: "tcp://127.0.0.1:0", wantErr: "port"},
		{name: "wrong scheme", raw: "http://127.0.0.1:43127", wantErr: "tcp"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ValidatePublisherTarget(tt.raw)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidatePublisherTarget(%q): %v", tt.raw, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("ValidatePublisherTarget(%q) error = %v, want substring %q", tt.raw, err, tt.wantErr)
			}
		})
	}
}

func TestRunPublisherCarriesOpaqueBytes(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	target := "tcp://" + listener.Addr().String()
	serverDone := make(chan error, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			serverDone <- acceptErr
			return
		}
		defer conn.Close()
		buf := make([]byte, len("routed-bytes"))
		_, readErr := io.ReadFull(conn, buf)
		if readErr == nil {
			_, readErr = conn.Write(buf)
		}
		serverDone <- readErr
	}()

	channel, peer := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	runDone := make(chan error, 1)
	go func() { runDone <- RunPublisher(ctx, channel, target) }()
	t.Cleanup(func() { _ = peer.Close() })
	if err := routebroker.WriteFrame(peer, routebroker.Frame{Kind: routebroker.KindOpen, Stream: 1}, routebroker.MaxFramePayload); err != nil {
		t.Fatal(err)
	}
	if got := readFrameWithTimeout(t, peer); got.Kind != routebroker.KindOpenOK || got.Stream != 1 {
		t.Fatalf("publisher open response = %#v", got)
	}
	payload := []byte("routed-bytes")
	if err := routebroker.WriteFrame(peer, routebroker.Frame{Kind: routebroker.KindData, Stream: 1, Payload: payload}, routebroker.MaxFramePayload); err != nil {
		t.Fatal(err)
	}
	if got := readFrameWithTimeout(t, peer); got.Kind != routebroker.KindData || string(got.Payload) != string(payload) {
		t.Fatalf("publisher response = %#v, want opaque payload", got)
	}
	if err := routebroker.WriteFrame(peer, routebroker.Frame{Kind: routebroker.KindClose, Stream: 1}, routebroker.MaxFramePayload); err != nil {
		t.Fatal(err)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
	_ = peer.Close()
	select {
	case <-runDone:
	case <-time.After(time.Second):
		t.Fatal("publisher did not stop after channel close")
	}
	if _, err := ValidatePublisherTarget("tcp://10.0.0.1:43127"); !errors.Is(err, ErrInvalidTarget) {
		t.Fatalf("neighbor target error = %v, want ErrInvalidTarget", err)
	}
}

func TestRunConsumerExposesConsumerLocalEndpoint(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	channel, peer := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	runDone := make(chan error, 1)
	go func() { runDone <- RunConsumer(ctx, channel, listener) }()
	local, err := net.Dial("tcp4", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = local.Close(); _ = peer.Close() })
	open := readFrameWithTimeout(t, peer)
	if open.Kind != routebroker.KindOpen || open.Stream == 0 {
		t.Fatalf("consumer open frame = %#v", open)
	}
	if err := routebroker.WriteFrame(peer, routebroker.Frame{Kind: routebroker.KindOpenOK, Stream: open.Stream}, routebroker.MaxFramePayload); err != nil {
		t.Fatal(err)
	}
	payload := []byte("consumer-local")
	if _, err := local.Write(payload); err != nil {
		t.Fatal(err)
	}
	if got := readFrameWithTimeout(t, peer); got.Kind != routebroker.KindData || string(got.Payload) != string(payload) {
		t.Fatalf("consumer data frame = %#v", got)
	}
	response := []byte("publisher-response")
	if err := routebroker.WriteFrame(peer, routebroker.Frame{Kind: routebroker.KindData, Stream: open.Stream, Payload: response}, routebroker.MaxFramePayload); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(response))
	if _, err := io.ReadFull(local, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != string(response) {
		t.Fatalf("consumer local response = %q", got)
	}
	_ = peer.Close()
	select {
	case <-runDone:
	case <-time.After(time.Second):
		t.Fatal("consumer did not stop after channel close")
	}
}

func TestDialUnixChannelUsesAuthenticatedUpgradeHeaders(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "tclaude-route-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socketPath := filepath.Join(dir, "agentd-"+strconv.Itoa(os.Getpid())+".sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close(); _ = os.Remove(socketPath) })
	serverDone := make(chan error, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			serverDone <- acceptErr
			return
		}
		defer conn.Close()
		req, readErr := http.ReadRequest(bufio.NewReader(conn))
		if readErr != nil {
			serverDone <- readErr
			return
		}
		if req.Header.Get(channelHeaderRole) != RolePublisher || req.Header.Get(channelHeaderID) != "rte_test" || req.Header.Get("X-Tclaude-Route-Helper-Credential") != "credential_test" {
			serverDone <- errors.New("upgrade headers were not carried")
			return
		}
		_, writeErr := io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: tclaude-route-v1\r\n\r\n")
		serverDone <- writeErr
	}()

	conn, err := DialUnixChannel(context.Background(), socketPath, ChannelAuth{
		Role: RolePublisher, RouteID: "rte_test", AgentID: "agt_test", ConvID: "conv_test", LaunchGeneration: "launch_test", GroupGeneration: 1, Credential: "credential_test",
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}

func readFrameWithTimeout(t *testing.T, conn net.Conn) routebroker.Frame {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	frame, err := routebroker.ReadFrame(conn, routebroker.MaxFramePayload)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Time{})
	return frame
}
