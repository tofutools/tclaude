package humannotify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

// TransportTelegram is the config `human_notify.transport` value that
// selects the Telegram transport.
const TransportTelegram = "telegram"

// telegramAPIBase is the Telegram Bot API root. A package var, not a
// const, purely so the white-box unit test can point it at an
// httptest.Server — production code never reassigns it.
var telegramAPIBase = "https://api.telegram.org"

// telegramHTTPTimeout bounds a single Bot API HTTP call: long enough
// for a slow mobile uplink, short enough that a stuck call cannot pin
// the daemon's request goroutine.
const telegramHTTPTimeout = 15 * time.Second

// telegramTransport is the Telegram Transport implementation. It
// satisfies the outbound Transport interface; the inbound
// InboundTransport half is staged for a follow-up (see humannotify.go).
type telegramTransport struct {
	token  string
	chatID string
	http   *http.Client
}

// newTelegramTransport validates the Telegram config and builds the
// transport. Both the bot token and the chat id are required for an
// outbound send; an empty value yields an actionable error naming the
// config file, rather than a confusing Bot API rejection later.
func newTelegramTransport(cfg *config.TelegramConfig) (*telegramTransport, error) {
	var token, chatID string
	if cfg != nil {
		token = strings.TrimSpace(cfg.BotToken)
		chatID = strings.TrimSpace(cfg.ChatID)
	}
	if token == "" {
		return nil, fmt.Errorf("human_notify.telegram.bot_token is empty in %s — get one from @BotFather", config.ConfigPath())
	}
	if chatID == "" {
		return nil, fmt.Errorf("human_notify.telegram.chat_id is empty in %s — resolve it with `tclaude agent notify-human resolve-chat-id`", config.ConfigPath())
	}
	return &telegramTransport{
		token:  token,
		chatID: chatID,
		http:   &http.Client{Timeout: telegramHTTPTimeout},
	}, nil
}

func (t *telegramTransport) Name() string { return TransportTelegram }

// Send delivers the notification via the Telegram sendMessage method,
// returning the resulting message_id (as a string) for the handle.
func (t *telegramTransport) Send(ctx context.Context, n Notification) (string, error) {
	req := map[string]any{
		"chat_id": t.chatID,
		"text":    formatTelegramText(n),
		// Plain text — deliberately no parse_mode — so nothing in a
		// body can break the message by colliding with Markdown / HTML
		// escaping rules.
		"disable_web_page_preview": true,
	}
	var result struct {
		MessageID int64 `json:"message_id"`
	}
	if err := telegramCall(ctx, t.http, t.token, "sendMessage", req, &result); err != nil {
		return "", err
	}
	if result.MessageID == 0 {
		return "", nil
	}
	return strconv.FormatInt(result.MessageID, 10), nil
}

// formatTelegramText renders a Notification as the plain-text body of a
// Telegram message: a 🔔 attribution line (who / which group), the
// optional subject, then the body.
func formatTelegramText(n Notification) string {
	attribution := strings.TrimSpace(n.FromTitle)
	if attribution == "" {
		attribution = "tclaude"
	}
	if g := strings.TrimSpace(n.Group); g != "" {
		attribution += " · group " + g
	}
	var b strings.Builder
	b.WriteString("🔔 " + attribution + "\n")
	if s := strings.TrimSpace(n.Subject); s != "" {
		b.WriteString(s + "\n")
	}
	b.WriteString("\n")
	b.WriteString(strings.TrimSpace(n.Body))
	return b.String()
}

// ChatCandidate is one chat the bot has recently received a message
// from — a row in the `notify-human resolve-chat-id` output.
type ChatCandidate struct {
	ChatID string // numeric chat id, as a string for config.json
	Type   string // "private", "group", "supergroup", "channel"
	Label  string // best-effort human label: title, @username, or a name
}

// ResolveTelegramChatIDs calls getUpdates once and returns the distinct
// chats the bot has recently seen a message from, so the human can
// pick their chat_id for config.json without crafting a curl.
//
// It is a setup helper, kept separate from the staged
// InboundTransport.Poll runtime path: it does NOT advance the update
// offset, so it is side-effect free and safe to run repeatedly.
func ResolveTelegramChatIDs(ctx context.Context, token string) ([]ChatCandidate, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, fmt.Errorf("telegram bot token is empty")
	}
	client := &http.Client{Timeout: telegramHTTPTimeout}
	var updates []telegramUpdate
	if err := telegramCall(ctx, client, token, "getUpdates", map[string]any{}, &updates); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []ChatCandidate
	for _, u := range updates {
		c := u.Message.Chat
		if c.ID == 0 {
			continue
		}
		id := strconv.FormatInt(c.ID, 10)
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, ChatCandidate{
			ChatID: id,
			Type:   c.Type,
			Label:  telegramChatLabel(c),
		})
	}
	return out, nil
}

// telegramUpdate / telegramMessage / telegramChat mirror the subset of
// the Bot API getUpdates response this package consumes.
type telegramUpdate struct {
	UpdateID int64           `json:"update_id"`
	Message  telegramMessage `json:"message"`
}

type telegramMessage struct {
	Chat telegramChat `json:"chat"`
}

type telegramChat struct {
	ID        int64  `json:"id"`
	Type      string `json:"type"`
	Title     string `json:"title"`
	Username  string `json:"username"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
}

// telegramChatLabel picks the best human-readable label for a chat:
// a group title, else an @username, else a person's name.
func telegramChatLabel(c telegramChat) string {
	if c.Title != "" {
		return c.Title
	}
	if c.Username != "" {
		return "@" + c.Username
	}
	if name := strings.TrimSpace(c.FirstName + " " + c.LastName); name != "" {
		return name
	}
	return "(unnamed)"
}

// telegramCall POSTs a JSON body to a Telegram Bot API method and
// decodes the envelope's `result` into out. Telegram's
// {"ok":false,"description":...} error envelope, a non-JSON response,
// and any HTTP / transport failure all collapse into a single Go error.
func telegramCall(ctx context.Context, client *http.Client, token, method string, body, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal %s request: %w", method, err)
	}
	// PathEscape the token defensively: a real BotFather token is
	// already URL-safe, but a malformed one must not be able to mangle
	// the path or smuggle extra segments.
	endpoint := fmt.Sprintf("%s/bot%s/%s", telegramAPIBase, url.PathEscape(token), method)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("telegram %s: %w", method, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)

	// Telegram always returns a JSON envelope, even on error:
	//   {"ok":true,"result":...}
	//   {"ok":false,"error_code":N,"description":"..."}
	var env struct {
		OK          bool            `json:"ok"`
		Description string          `json:"description"`
		ErrorCode   int             `json:"error_code"`
		Result      json.RawMessage `json:"result"`
	}
	if jerr := json.Unmarshal(raw, &env); jerr != nil {
		return fmt.Errorf("telegram %s: HTTP %d, unparseable response: %s",
			method, resp.StatusCode, truncateForError(raw))
	}
	if !env.OK {
		desc := env.Description
		if desc == "" {
			desc = "unknown error"
		}
		return fmt.Errorf("telegram %s rejected (error_code %d): %s", method, env.ErrorCode, desc)
	}
	if out != nil && len(env.Result) > 0 {
		if jerr := json.Unmarshal(env.Result, out); jerr != nil {
			return fmt.Errorf("telegram %s: decode result: %w", method, jerr)
		}
	}
	return nil
}

// truncateForError trims an unparseable response body to a short,
// log-safe snippet for an error message. It truncates on a rune
// boundary — a Telegram error page or proxy banner may be multi-byte
// UTF-8, and a byte-slice could split a rune into invalid bytes.
func truncateForError(b []byte) string {
	const max = 200
	s := strings.TrimSpace(string(b))
	r := []rune(s)
	if len(r) > max {
		return string(r[:max]) + "…"
	}
	return s
}
