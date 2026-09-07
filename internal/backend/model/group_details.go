package model

import (
	"fmt"
	"net/url"
	"strings"
	"unicode/utf8"
)

type GroupDetails struct {
	Description string
	Mission     string
	LinkURL     string
	LinkLabel   string
}

func ValidateGroupDetails(d GroupDetails) error {
	for _, field := range []struct {
		value string
		limit int
	}{{d.Description, 32768}, {d.Mission, 32768}, {d.LinkURL, 4096}, {d.LinkLabel, 256}} {
		value, limit := field.value, field.limit
		if len(value) > limit || !utf8.ValidString(value) || strings.ContainsRune(value, 0) {
			return fmt.Errorf("invalid group detail text")
		}
	}
	if d.LinkURL != "" {
		u, err := url.Parse(d.LinkURL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil || strings.ContainsAny(d.LinkURL, "\r\n\t ") {
			return fmt.Errorf("group link must be an HTTP(S) URL without credentials")
		}
	}
	if d.LinkLabel != "" && d.LinkURL == "" {
		return fmt.Errorf("group link label requires a URL")
	}
	return nil
}
