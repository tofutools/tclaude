package transport

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

type pipeAttachment struct{ net.Conn }

func (pipeAttachment) Kind() ports.AttachmentKind { return ports.AttachmentTerminal }

type attachmentProbe struct {
	app.API
	attachment ports.Attachment
	calls      atomic.Int32
}

func (p *attachmentProbe) Attach(context.Context, app.AttachRequest) (app.AttachmentResult, error) {
	p.calls.Add(1)
	return app.AttachmentResult{Attachment: p.attachment}, nil
}

func dialAttachment(t *testing.T, url string, headers http.Header) *websocket.Conn {
	t.Helper()
	c, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(url, "http")+"/v2/attach?execution_id=e&request_id=r", headers)
	if err != nil {
		if response != nil {
			_ = response.Body.Close()
		}
		t.Fatal(err)
	}
	return c
}

func TestAttachmentConnectionExchangesBytesAndDisconnectsWithoutStop(t *testing.T) {
	backend, terminal := net.Pipe()
	defer terminal.Close()
	p := &attachmentProbe{attachment: pipeAttachment{backend}}
	h := testHandler(t, p)
	done := make(chan struct{})
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		h.ServeHTTP(w, r)
	}))
	defer s.Close()
	c := dialAttachment(t, s.URL, http.Header{"Authorization": {"Bearer " + testCredential}})
	defer c.Close()
	if err := c.WriteMessage(websocket.BinaryMessage, []byte("input")); err != nil {
		t.Fatal(err)
	}
	_ = terminal.SetReadDeadline(time.Now().Add(2 * time.Second))
	data := make([]byte, 5)
	if _, err := io.ReadFull(terminal, data); err != nil || string(data) != "input" {
		t.Fatalf("terminal input=%q err=%v", data, err)
	}
	if _, err := terminal.Write([]byte("output")); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	kind, data, err := c.ReadMessage()
	if err != nil || kind != websocket.BinaryMessage || string(data) != "output" {
		t.Fatalf("client output=%q kind=%d err=%v", data, kind, err)
	}
	_ = c.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("attachment workers did not finish after disconnect")
	}
	if p.calls.Load() != 1 {
		t.Fatalf("attachment calls=%d", p.calls.Load())
	}
	// The probe has no Stop implementation: any workload stop would panic.
}

func TestAttachmentDisconnectInterruptsBlockedNativeInput(t *testing.T) {
	backend, terminal := net.Pipe()
	defer terminal.Close()
	p := &attachmentProbe{attachment: pipeAttachment{backend}}
	h := testHandler(t, p)
	done := make(chan struct{})
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		h.ServeHTTP(w, r)
	}))
	defer s.Close()
	c := dialAttachment(t, s.URL, http.Header{"Authorization": {"Bearer " + testCredential}})
	if err := c.WriteMessage(websocket.BinaryMessage, []byte("no native reader consumes this")); err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("blocked native input kept attachment handler alive")
	}
}

func TestAttachmentRejectsForeignOriginBeforeAllocating(t *testing.T) {
	p := &attachmentProbe{}
	s := httptest.NewServer(testHandler(t, p))
	defer s.Close()
	_, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(s.URL, "http")+"/v2/attach?execution_id=e&request_id=r", http.Header{
		"Authorization": {"Bearer " + testCredential}, "Origin": {"https://unrelated.invalid"},
	})
	if response != nil {
		defer response.Body.Close()
	}
	if err == nil || response == nil || response.StatusCode != http.StatusForbidden || p.calls.Load() != 0 {
		t.Fatalf("foreign attachment admitted: response=%v err=%v calls=%d", response, err, p.calls.Load())
	}
}

type resizablePipe struct {
	pipeAttachment
	sizes chan ports.TerminalSize
}

func (p resizablePipe) Resize(_ context.Context, size ports.TerminalSize) error {
	p.sizes <- size
	return nil
}
func TestAttachmentResizeUsesNegotiatedControlChannel(t *testing.T) {
	backend, terminal := net.Pipe()
	defer terminal.Close()
	sizes := make(chan ports.TerminalSize, 1)
	p := &attachmentProbe{attachment: resizablePipe{pipeAttachment{backend}, sizes}}
	server := httptest.NewServer(testHandler(t, p))
	defer server.Close()
	dialer := websocket.Dialer{Subprotocols: []string{"tclaude.terminal.v1"}}
	conn, _, err := dialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v2/attach?execution_id=e&request_id=r", http.Header{"Authorization": {"Bearer " + testCredential}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var capability struct {
		Type   string `json:"type"`
		Resize bool   `json:"resize"`
	}
	if err = conn.ReadJSON(&capability); err != nil || capability.Type != "capabilities" || !capability.Resize {
		t.Fatalf("capabilities: %+v %v", capability, err)
	}
	if err = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"resize","columns":120,"rows":40}`)); err != nil {
		t.Fatal(err)
	}
	select {
	case size := <-sizes:
		if size.Columns != 120 || size.Rows != 40 {
			t.Fatalf("size %+v", size)
		}
	case <-time.After(time.Second):
		t.Fatal("no resize")
	}
	if err = conn.WriteMessage(websocket.BinaryMessage, []byte("input")); err != nil {
		t.Fatal(err)
	}
	_ = terminal.SetReadDeadline(time.Now().Add(time.Second))
	data := make([]byte, 5)
	if _, err = io.ReadFull(terminal, data); err != nil || string(data) != "input" {
		t.Fatalf("input %q: %v", data, err)
	}
}
