package hub

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

type adminLogEntry struct {
	At       time.Time `json:"at"`
	Level    string    `json:"level"`
	Message  string    `json:"message"`
	sequence int64
	code     string
	warn     bool
}
type adminLogRing struct {
	mu       sync.Mutex
	entries  []adminLogEntry
	sequence int64
}
type adminLogHandler struct {
	next  slog.Handler
	ring  *adminLogRing
	attrs []slog.Attr
}

var adminLogSecrets = regexp.MustCompile(`tchac_[a-f0-9]{64}|tchi_[a-f0-9]{48}`)

func safeHubLog(s string) string {
	s = adminLogSecrets.ReplaceAllString(s, "[redacted]")
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	if len(s) > 2048 {
		s = s[:2048]
		s = strings.ToValidUTF8(s, "�")
	}
	return s
}
func (h *adminLogHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}
func (h *adminLogHandler) Handle(ctx context.Context, r slog.Record) error {
	parts := []string{r.Message}
	code := ""
	add := func(a slog.Attr) {
		key := strings.ToLower(a.Key)
		if strings.Contains(key, "token") || strings.Contains(key, "secret") || strings.Contains(key, "password") {
			parts = append(parts, a.Key+"=[redacted]")
			return
		}
		value := a.Value.Resolve().String()
		if a.Key == "code" {
			code = safeHubLog(value)
		}
		parts = append(parts, a.Key+"="+value)
	}
	for _, a := range h.attrs {
		add(a)
	}
	r.Attrs(func(a slog.Attr) bool { add(a); return true })
	h.ring.mu.Lock()
	h.ring.sequence++
	h.ring.entries = append(h.ring.entries, adminLogEntry{At: r.Time, Level: r.Level.String(), Message: safeHubLog(strings.Join(parts, " ")), sequence: h.ring.sequence, code: code, warn: r.Level >= slog.LevelWarn})
	if len(h.ring.entries) > 256 {
		h.ring.entries = h.ring.entries[len(h.ring.entries)-256:]
	}
	h.ring.mu.Unlock()
	return h.next.Handle(ctx, r)
}
func (h *adminLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &adminLogHandler{next: h.next.WithAttrs(attrs), ring: h.ring, attrs: append(append([]slog.Attr{}, h.attrs...), attrs...)}
}
func (h *adminLogHandler) WithGroup(name string) slog.Handler {
	return &adminLogHandler{next: h.next.WithGroup(name), ring: h.ring, attrs: h.attrs}
}
func (h *Hub) recentErrors() []map[string]any {
	h.logs.mu.Lock()
	defer h.logs.mu.Unlock()
	out := []map[string]any{}
	for i := len(h.logs.entries) - 1; i >= 0 && len(out) < 10; i-- {
		e := h.logs.entries[i]
		if e.warn {
			out = append(out, map[string]any{"at": e.At, "code": e.code, "message": e.Message})
		}
	}
	return out
}
func (h *Hub) adminLogTail(p adminParams) (any, error) {
	after := int64(0)
	if p.Cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(p.Cursor)
		if err != nil {
			return nil, adminErr(400, "cursor", "invalid log cursor")
		}
		after, err = strconv.ParseInt(string(raw), 10, 64)
		if err != nil || after < 0 {
			return nil, adminErr(400, "cursor", "invalid log cursor")
		}
	}
	limit := p.MaxEntries
	if limit == 0 {
		limit = 100
	}
	if limit < 1 || limit > 200 {
		return nil, adminErr(400, "max_entries", "max_entries must be1..200")
	}
	h.logs.mu.Lock()
	defer h.logs.mu.Unlock()
	entries := []adminLogEntry{}
	next := after
	budget := proto.MaxAdminResult - 1024
	for _, e := range h.logs.entries {
		if e.sequence > after && len(entries) < limit {
			raw, err := json.Marshal(e)
			if err != nil {
				return nil, err
			}
			if len(raw)+1 > budget {
				break
			}
			budget -= len(raw) + 1
			entries = append(entries, e)
			next = e.sequence
		}
	}
	return map[string]any{"entries": entries, "next_cursor": base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprint(next)))}, nil
}
