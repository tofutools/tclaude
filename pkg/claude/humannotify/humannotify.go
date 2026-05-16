// Package humannotify delivers agent-to-human notifications over an
// external transport — the data plane behind `tclaude agent
// notify-human`.
//
// The daemon (agentd) resolves a Transport from config and calls Send;
// the CLI never touches a transport directly. Routing every send
// through the daemon means the human.notify permission gate is enforced
// server-side (the CLI is untrusted) and the network call originates
// from agentd, which runs outside the agent sandbox and can reach
// api.telegram.org.
//
// The abstraction is deliberately split into an outbound half
// (Transport) and an optional inbound half (InboundTransport) so the
// PO→human direction can ship before the human→PO reply path. Telegram
// is the reference Transport; its InboundTransport half is staged for a
// follow-up. See docs/plans/TODO/high-prio/human-notify-channel.md.
package humannotify

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

// ErrNotConfigured is returned by Resolve when no human-notify
// transport is set up in config.json. Callers surface it as a clear
// "configure the human_notify section" message rather than a generic
// failure.
var ErrNotConfigured = errors.New("no human-notify transport configured")

// Notification is one outbound human-facing message.
type Notification struct {
	// FromConv is the calling agent's conv-id — "" when the human
	// invoked notify-human directly. Recorded for audit and (in the
	// staged inbound path) reply routing.
	FromConv string
	// FromTitle is the caller's display title, e.g. "tclaude-PO". Best
	// effort; "" when the caller has no resolvable title.
	FromTitle string
	// Group is the caller's group name, when known — context for the
	// human ("which project is pinging me").
	Group string
	// Subject is an optional one-line summary.
	Subject string
	// Body is the message text. Required.
	Body string
	// SentAt is when the daemon accepted the send.
	SentAt time.Time
}

// Transport delivers a Notification over an external channel — the
// outbound half. Every transport implements this.
type Transport interface {
	// Name is the config `transport` value this implements, e.g.
	// "telegram".
	Name() string
	// Send delivers n and returns a transport-specific message handle
	// (e.g. a Telegram message_id) usable later for reply correlation.
	// The handle may be "" for transports that have no such concept.
	Send(ctx context.Context, n Notification) (handle string, err error)
}

// InboundReply is one human reply pulled from an inbound-capable
// transport.
type InboundReply struct {
	// Text is the human's reply body.
	Text string
	// CorrelatesTo is the transport handle of the message this reply
	// was made against, when the transport can tell (e.g. the human
	// used Telegram's native reply). "" otherwise.
	CorrelatesTo string
	// ReceivedAt is when the transport observed the reply.
	ReceivedAt time.Time
}

// InboundTransport is the optional inbound half: a transport that can
// also receive human replies and route them back to the PO.
//
// It is defined now, ahead of its first implementation, so the inbound
// (human→PO) work is purely additive — the outbound code already
// compiles against the final seam. Telegram will implement Poll over
// getUpdates long-polling in the follow-up; until then no transport
// satisfies this interface and no inbound poller is started.
type InboundTransport interface {
	Transport
	// Poll fetches replies newer than cursor and returns them plus an
	// advanced cursor to pass next time. cursor is opaque to the caller
	// (the Telegram impl packs the getUpdates offset into it); "" means
	// "start from the transport's default position".
	Poll(ctx context.Context, cursor string) (replies []InboundReply, next string, err error)
}

// Resolve builds the Transport named by cfg.HumanNotify.Transport.
//
// Returns ErrNotConfigured when no transport is set up, and a
// descriptive error when a transport is named but mis-configured (e.g.
// Telegram selected with an empty bot token) so the human gets an
// actionable message instead of a silent no-op.
func Resolve(cfg *config.Config) (Transport, error) {
	if cfg == nil || cfg.HumanNotify == nil || cfg.HumanNotify.Transport == "" {
		return nil, ErrNotConfigured
	}
	switch cfg.HumanNotify.Transport {
	case TransportTelegram:
		return newTelegramTransport(cfg.HumanNotify.Telegram)
	default:
		return nil, fmt.Errorf("unknown human-notify transport %q (known: %s)",
			cfg.HumanNotify.Transport, TransportTelegram)
	}
}
