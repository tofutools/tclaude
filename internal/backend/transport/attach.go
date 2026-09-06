package transport

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

// sameOrigin also allows non-browser clients with no Origin. Authentication
// remains mandatory; cross-site browser handshakes cannot allocate attachments.
func sameOrigin(r *http.Request) bool {
	origins := r.Header.Values("Origin")
	if len(origins) == 0 {
		return true
	}
	if len(origins) != 1 {
		return false
	}
	u, err := url.Parse(origins[0])
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && strings.EqualFold(u.Host, r.Host)
}

func (h *Handler) attach(w http.ResponseWriter, r *http.Request) {
	p, ok := h.caller(w, r)
	if !ok {
		return
	}
	if !websocket.IsWebSocketUpgrade(r) {
		writeError(w, http.StatusUpgradeRequired, "websocket_required")
		return
	}
	if !sameOrigin(r) {
		writeError(w, http.StatusForbidden, "foreign_origin")
		return
	}
	q := r.URL.Query()
	if len(q) != 2 || len(q["execution_id"]) != 1 || len(q["request_id"]) != 1 || q.Get("execution_id") == "" || q.Get("request_id") == "" {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	result, err := h.application.Attach(r.Context(), app.AttachRequest{
		RequestContext: app.RequestContext{Principal: p, RequestID: model.RequestID(q.Get("request_id"))},
		ExecutionID:    model.ExecutionID(q.Get("execution_id")), Kind: ports.AttachmentTerminal,
	})
	if err != nil {
		applicationError(w, err)
		return
	}
	if result.Attachment == nil {
		writeError(w, http.StatusConflict, "attachment_unavailable")
		return
	}
	// Closing this view frees connection resources only. The application Stop
	// operation remains the sole way this transport requests workload exit.
	defer func() { _ = result.Attachment.Close() }()
	u := websocket.Upgrader{HandshakeTimeout: 10 * time.Second, CheckOrigin: sameOrigin, Subprotocols: []string{"tclaude.terminal.v1"}}
	conn, err := u.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()
	closed := make(chan struct{})
	defer close(closed)
	go func() {
		select {
		case <-r.Context().Done():
			_ = conn.Close()
		case <-closed:
		}
	}()
	conn.SetReadLimit(maxRequestBytes)
	resizable, canResize := result.Attachment.(ports.ResizableAttachment)
	framed := conn.Subprotocol() == "tclaude.terminal.v1"
	if framed {
		if err := conn.WriteJSON(map[string]any{"type": "capabilities", "resize": canResize}); err != nil {
			return
		}
	}
	outputDone := make(chan struct{})
	inputDone := make(chan struct{})
	input := make(chan []byte, 4)
	go func() {
		defer close(inputDone)
		defer func() { _ = conn.Close() }()
		for data := range input {
			if _, err := io.Copy(result.Attachment, bytes.NewReader(data)); err != nil {
				return
			}
		}
	}()
	go func() {
		defer close(outputDone)
		defer func() { _ = conn.Close() }()
		buffer := make([]byte, 32<<10)
		for {
			n, readErr := result.Attachment.Read(buffer)
			if n > 0 {
				if err := conn.SetWriteDeadline(time.Now().Add(30 * time.Second)); err != nil {
					return
				}
				if err := conn.WriteMessage(websocket.BinaryMessage, buffer[:n]); err != nil {
					return
				}
			}
			if readErr != nil {
				return
			}
		}
	}()
readInput:
	for {
		kind, data, readErr := conn.ReadMessage()
		if readErr != nil {
			break
		}
		if framed && kind == websocket.TextMessage {
			var control struct {
				Type    string `json:"type"`
				Columns uint16 `json:"columns"`
				Rows    uint16 `json:"rows"`
			}
			decoder := json.NewDecoder(bytes.NewReader(data))
			decoder.DisallowUnknownFields()
			if decoder.Decode(&control) != nil || decoder.Decode(new(any)) != io.EOF || control.Type != "resize" || control.Columns < 1 || control.Columns > 1000 || control.Rows < 1 || control.Rows > 1000 || !canResize {
				break
			}
			if err := resizable.Resize(r.Context(), ports.TerminalSize{Columns: control.Columns, Rows: control.Rows}); err != nil {
				break
			}
			continue
		}
		if kind != websocket.BinaryMessage && kind != websocket.TextMessage {
			continue
		}
		select {
		case input <- data:
		case <-inputDone:
			break readInput
		default:
			// A stalled native reader cannot cause unlimited queued input or
			// prevent this loop from noticing a disconnected client.
			break readInput
		}
	}
	// Attachment.Close must interrupt its own reads/writes. Join the output
	// worker before releasing request ownership; no detached goroutine survives.
	_ = result.Attachment.Close()
	_ = conn.Close()
	close(input)
	<-inputDone
	<-outputDone
}
