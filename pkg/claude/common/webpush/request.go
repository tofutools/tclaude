package webpush

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"time"
)

// Options are the per-message headers of RFC 8030 §5.
type Options struct {
	// TTL is how long the push service keeps an undelivered message.
	TTL time.Duration
	// Urgency is "very-low", "low", "normal" or "high"; empty is normal.
	Urgency string
	// Topic lets a newer message replace an undelivered older one with
	// the same topic: at most 32 base64url characters.
	Topic string
	// Subject is the VAPID contact claim (a mailto: or https: URL).
	Subject string
}

var topicRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)

// NewRequest builds the POST that delivers plaintext to the subscription:
// the encrypted body and the VAPID, TTL, Urgency and Topic headers.
func NewRequest(ctx context.Context, s Subscription, plaintext []byte, v *VAPID, o Options, now time.Time) (*http.Request, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	if o.Topic != "" && !topicRE.MatchString(o.Topic) {
		return nil, errors.New("topic must be 1-32 base64url characters")
	}
	switch o.Urgency {
	case "", "very-low", "low", "normal", "high":
	default:
		return nil, errors.New("unknown urgency " + o.Urgency)
	}
	pub, auth, err := s.Keys()
	if err != nil {
		return nil, err
	}
	body, err := Encrypt(plaintext, pub, auth)
	if err != nil {
		return nil, err
	}
	authz, err := v.Authorization(s.Endpoint, o.Subject, now)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.Endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("TTL", strconv.Itoa(int(o.TTL/time.Second)))
	req.Header.Set("Authorization", authz)
	if o.Urgency != "" {
		req.Header.Set("Urgency", o.Urgency)
	}
	if o.Topic != "" {
		req.Header.Set("Topic", o.Topic)
	}
	return req, nil
}

// Outcome is what a push service's answer means for the subscription.
type Outcome int

const (
	// Delivered: the service took the message (201, or any 2xx).
	Delivered Outcome = iota
	// Gone: the subscription no longer exists (404, 410) and must be
	// forgotten.
	Gone
	// Retry: the service is busy or failing (429, 5xx); try again later.
	Retry
	// Rejected: the service refused this message (another 4xx, such as 413
	// or a bad VAPID header). Trying it again will not help.
	Rejected
)

// Classify reads a push service's status code.
func Classify(status int) Outcome {
	switch {
	case status >= 200 && status < 300:
		return Delivered
	case status == http.StatusNotFound || status == http.StatusGone:
		return Gone
	case status == http.StatusTooManyRequests || status >= 500:
		return Retry
	default:
		return Rejected
	}
}
