package agentd

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"
)

// The POST endpoint streams NDJSON. The CLI uses a WebSocket transport for
// bidirectional stdin; both transports run the same admission and process path.
type breakGlassConnection interface {
	ReadJSON(any) error
	WriteJSON(any) error
	SetReadLimit(int64)
	SetReadDeadline(time.Time) error
	SetWriteDeadline(time.Time) error
	Close() error
}

type breakGlassHTTPStream struct {
	decoder *json.Decoder
	encoder *json.Encoder
	control *http.ResponseController
	ctx     context.Context
	closed  chan struct{}
	once    sync.Once
	eof     bool
}

func (s *breakGlassHTTPStream) ReadJSON(v any) error {
	if s.eof {
		select {
		case <-s.ctx.Done():
			return s.ctx.Err()
		case <-s.closed:
			return io.EOF
		}
	}
	err := s.decoder.Decode(v)
	if err == io.EOF {
		s.eof = true
		if frame, ok := v.(*breakGlassFrame); ok {
			frame.Stream = "eof"
			return nil
		}
	}
	return err
}
func (s *breakGlassHTTPStream) WriteJSON(v any) error {
	if err := s.encoder.Encode(v); err != nil {
		return err
	}
	return s.control.Flush()
}
func (s *breakGlassHTTPStream) SetReadLimit(int64) {}
func (s *breakGlassHTTPStream) SetReadDeadline(t time.Time) error {
	return s.control.SetReadDeadline(t)
}
func (s *breakGlassHTTPStream) SetWriteDeadline(t time.Time) error {
	return s.control.SetWriteDeadline(t)
}
func (s *breakGlassHTTPStream) Close() error { s.once.Do(func() { close(s.closed) }); return nil }
