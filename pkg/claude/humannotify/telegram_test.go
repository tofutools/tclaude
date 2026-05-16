package humannotify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

// pointTelegramAt redirects the Bot API base URL at srv for the
// duration of the test — the white-box seam that keeps the unit tests
// off the real api.telegram.org.
func pointTelegramAt(t *testing.T, srv *httptest.Server) {
	t.Helper()
	prev := telegramAPIBase
	telegramAPIBase = srv.URL
	t.Cleanup(func() { telegramAPIBase = prev })
}

// telegramStub is an httptest handler that fakes the two Bot API
// methods this package calls. It records the last request body so a
// test can assert on what was sent.
type telegramStub struct {
	t        *testing.T
	sendResp string // raw JSON for sendMessage
	updResp  string // raw JSON for getUpdates
	lastPath string
	lastBody map[string]any
}

func (s *telegramStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.lastPath = r.URL.Path
	raw, _ := io.ReadAll(r.Body)
	s.lastBody = map[string]any{}
	_ = json.Unmarshal(raw, &s.lastBody)
	w.Header().Set("Content-Type", "application/json")
	switch {
	case strings.HasSuffix(r.URL.Path, "/sendMessage"):
		_, _ = io.WriteString(w, s.sendResp)
	case strings.HasSuffix(r.URL.Path, "/getUpdates"):
		_, _ = io.WriteString(w, s.updResp)
	default:
		s.t.Fatalf("unexpected telegram method path %q", r.URL.Path)
	}
}

func TestTelegramTransport_Send_OK(t *testing.T) {
	stub := &telegramStub{t: t, sendResp: `{"ok":true,"result":{"message_id":4242}}`}
	srv := httptest.NewServer(stub)
	defer srv.Close()
	pointTelegramAt(t, srv)

	tr, err := newTelegramTransport(&config.TelegramConfig{BotToken: "TESTTOKEN", ChatID: "C123"})
	require.NoError(t, err)

	handle, err := tr.Send(context.Background(), Notification{
		FromTitle: "tclaude-PO",
		Group:     "tclaude-dev",
		Subject:   "blocker",
		Body:      "need a decision",
	})
	require.NoError(t, err)
	assert.Equal(t, "4242", handle, "handle should be the returned message_id")

	// Request shaping: the token is in the path, chat_id + text in the body.
	assert.Equal(t, "/botTESTTOKEN/sendMessage", stub.lastPath)
	assert.Equal(t, "C123", stub.lastBody["chat_id"])
	text, _ := stub.lastBody["text"].(string)
	assert.Contains(t, text, "need a decision", "body must reach Telegram")
	assert.Contains(t, text, "tclaude-PO", "attribution must reach Telegram")
	assert.Contains(t, text, "blocker", "subject must reach Telegram")
}

func TestTelegramTransport_Send_APIError(t *testing.T) {
	stub := &telegramStub{t: t, sendResp: `{"ok":false,"error_code":400,"description":"Bad Request: chat not found"}`}
	srv := httptest.NewServer(stub)
	defer srv.Close()
	pointTelegramAt(t, srv)

	tr, err := newTelegramTransport(&config.TelegramConfig{BotToken: "TESTTOKEN", ChatID: "C123"})
	require.NoError(t, err)

	_, err = tr.Send(context.Background(), Notification{Body: "hi"})
	require.Error(t, err, "a Telegram {ok:false} envelope must surface as an error")
	assert.Contains(t, err.Error(), "chat not found")
	assert.Contains(t, err.Error(), "400")
}

func TestTelegramTransport_Send_NonJSONResponse(t *testing.T) {
	stub := &telegramStub{t: t, sendResp: `<html>502 Bad Gateway</html>`}
	srv := httptest.NewServer(stub)
	defer srv.Close()
	pointTelegramAt(t, srv)

	tr, err := newTelegramTransport(&config.TelegramConfig{BotToken: "T", ChatID: "C"})
	require.NoError(t, err)

	_, err = tr.Send(context.Background(), Notification{Body: "hi"})
	require.Error(t, err, "an unparseable response must surface as an error, not a silent success")
	assert.Contains(t, err.Error(), "unparseable")
}

func TestNewTelegramTransport_MissingFields(t *testing.T) {
	_, err := newTelegramTransport(&config.TelegramConfig{ChatID: "C"})
	require.Error(t, err, "empty bot token must be rejected")
	assert.Contains(t, err.Error(), "bot_token")

	_, err = newTelegramTransport(&config.TelegramConfig{BotToken: "T"})
	require.Error(t, err, "empty chat id must be rejected")
	assert.Contains(t, err.Error(), "chat_id")

	_, err = newTelegramTransport(nil)
	require.Error(t, err, "nil telegram config must be rejected")
}

func TestResolveTelegramChatIDs(t *testing.T) {
	// Two distinct chats plus a duplicate of the first — dedup expected.
	stub := &telegramStub{t: t, updResp: `{"ok":true,"result":[
		{"update_id":1,"message":{"chat":{"id":111,"type":"private","first_name":"Jo","last_name":"K"}}},
		{"update_id":2,"message":{"chat":{"id":222,"type":"group","title":"Dev Team"}}},
		{"update_id":3,"message":{"chat":{"id":111,"type":"private","first_name":"Jo","last_name":"K"}}}
	]}`}
	srv := httptest.NewServer(stub)
	defer srv.Close()
	pointTelegramAt(t, srv)

	got, err := ResolveTelegramChatIDs(context.Background(), "TESTTOKEN")
	require.NoError(t, err)
	require.Len(t, got, 2, "the duplicate chat must be collapsed")

	assert.Equal(t, "/botTESTTOKEN/getUpdates", stub.lastPath)
	assert.Equal(t, ChatCandidate{ChatID: "111", Type: "private", Label: "Jo K"}, got[0])
	assert.Equal(t, ChatCandidate{ChatID: "222", Type: "group", Label: "Dev Team"}, got[1])
}

func TestResolveTelegramChatIDs_EmptyToken(t *testing.T) {
	_, err := ResolveTelegramChatIDs(context.Background(), "  ")
	require.Error(t, err, "an empty token must fail before any HTTP call")
}

func TestResolve_NotConfigured(t *testing.T) {
	_, err := Resolve(nil)
	require.ErrorIs(t, err, ErrNotConfigured, "nil config means not configured")

	_, err = Resolve(&config.Config{})
	require.ErrorIs(t, err, ErrNotConfigured, "absent human_notify section means not configured")

	_, err = Resolve(&config.Config{HumanNotify: &config.HumanNotifyConfig{}})
	require.ErrorIs(t, err, ErrNotConfigured, "empty transport string means not configured")
}

func TestResolve_UnknownTransport(t *testing.T) {
	_, err := Resolve(&config.Config{HumanNotify: &config.HumanNotifyConfig{Transport: "carrierpigeon"}})
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrNotConfigured, "a named-but-unknown transport is a distinct error from not-configured")
	assert.Contains(t, err.Error(), "carrierpigeon")
}

func TestResolve_Telegram(t *testing.T) {
	tr, err := Resolve(&config.Config{HumanNotify: &config.HumanNotifyConfig{
		Transport: TransportTelegram,
		Telegram:  &config.TelegramConfig{BotToken: "T", ChatID: "C"},
	}})
	require.NoError(t, err)
	assert.Equal(t, TransportTelegram, tr.Name())
}

func TestTruncateForError(t *testing.T) {
	// A long, all-multibyte body must truncate to valid UTF-8 — a
	// naive byte-slice would split a rune into invalid bytes.
	long := strings.Repeat("世", 300)
	got := truncateForError([]byte(long))
	assert.True(t, utf8.ValidString(got), "truncated snippet must stay valid UTF-8")
	assert.Equal(t, 201, utf8.RuneCountInString(got), "200 runes + the ellipsis")
	assert.True(t, strings.HasSuffix(got, "…"))

	// A short body passes through untouched (modulo trimming).
	assert.Equal(t, "short body", truncateForError([]byte("  short body  ")))
}

func TestFormatTelegramText(t *testing.T) {
	withSubject := formatTelegramText(Notification{
		FromTitle: "tclaude-PO", Group: "tclaude-dev", Subject: "blocker", Body: "decide please",
	})
	assert.Contains(t, withSubject, "tclaude-PO · group tclaude-dev")
	assert.Contains(t, withSubject, "blocker")
	assert.Contains(t, withSubject, "decide please")

	// No title falls back to "tclaude"; no subject line still renders.
	bare := formatTelegramText(Notification{Body: "hello"})
	assert.Contains(t, bare, "tclaude")
	assert.Contains(t, bare, "hello")
	assert.NotContains(t, bare, "· group", "no group => no group clause")
}
