// Package reflink validates user-authored HTTP(S) references rendered by the
// dashboard, such as per-agent task links and per-group attachments.
package reflink

import (
	"fmt"
	"net/url"
	"strings"
)

const (
	MaxURLLen   = 2048
	MaxLabelLen = 200
)

// ValidateURL requires an absolute HTTP(S) URL with a host and bounds the
// value carried in dashboard snapshots.
func ValidateURL(rawURL string) error {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return fmt.Errorf("reference URL is empty")
	}
	if len(rawURL) > MaxURLLen {
		return fmt.Errorf("reference URL is too long (%d > %d chars)", len(rawURL), MaxURLLen)
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("reference URL is not a valid URL: %w", err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("reference URL must be http(s), got %q", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("reference URL must include a host")
	}
	return nil
}

// ValidateLabel bounds an optional explicit display label.
func ValidateLabel(label string) error {
	if len(label) > MaxLabelLen {
		return fmt.Errorf("reference label is too long (%d > %d chars)", len(label), MaxLabelLen)
	}
	return nil
}

// NormalizeOptional trims an optional URL/label pair and validates nonempty
// references. A label without a URL is discarded so storage never carries
// invisible attachment metadata.
func NormalizeOptional(rawURL, rawLabel string) (string, string, error) {
	rawURL = strings.TrimSpace(rawURL)
	rawLabel = strings.TrimSpace(rawLabel)
	if rawURL == "" {
		return "", "", nil
	}
	if err := ValidateURL(rawURL); err != nil {
		return "", "", err
	}
	if err := ValidateLabel(rawLabel); err != nil {
		return "", "", err
	}
	return rawURL, rawLabel, nil
}
