package agentd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// ghcmderror.go shapes the errors of agentd's background gh subprocesses so a
// poller's log line names the cause — gh's own stderr, a non-JSON error page,
// or our local timeout — instead of a bare "exit status 1" or
// "signal: killed".

// errCommandTimedOut marks a subprocess our own context deadline killed.
var errCommandTimedOut = errors.New("timed out")

const (
	subprocessStderrMax = 500
	subprocessBodyMax   = 160
)

// subprocessError wraps a failed run. stdout is the raw output, inspected only
// when it is not JSON: a gh api call that hits an HTML error page (a GitHub
// 502, say) prints that page, and its title is the only trace of the cause.
func subprocessError(ctx context.Context, timeout time.Duration, err error, stderr string, stdout []byte) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("%w after %s", errCommandTimedOut, timeout)
	}
	var details []string
	if detail := collapseSubprocessText(stderr, subprocessStderrMax); detail != "" {
		details = append(details, detail)
	}
	if body := nonJSONBodySnippet(stdout); body != "" {
		details = append(details, "response: "+body)
	}
	if len(details) == 0 {
		return err
	}
	return fmt.Errorf("%s: %w", strings.Join(details, "; "), err)
}

func collapseSubprocessText(s string, limit int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > limit {
		// Cut on a rune boundary so the log line stays valid UTF-8.
		cut := limit
		for cut > 0 && !isRuneStart(s[cut]) {
			cut--
		}
		s = s[:cut] + "..."
	}
	return s
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

var htmlTitlePattern = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)

// nonJSONBodySnippet returns a short, printable summary of a response body
// that is not JSON: an HTML page's title, otherwise its first characters.
// JSON bodies return "" — the caller decodes those, and gh already echoes
// their message on stderr.
func nonJSONBodySnippet(stdout []byte) string {
	body := bytes.TrimSpace(stdout)
	if len(body) == 0 || body[0] == '{' || body[0] == '[' {
		return ""
	}
	if m := htmlTitlePattern.FindSubmatch(body); m != nil {
		body = m[1]
	}
	printable := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, string(body))
	return collapseSubprocessText(printable, subprocessBodyMax)
}

// GitHub poller failure kinds, logged as `kind` so failures can be counted by
// cause rather than read one by one.
const (
	ghErrNetwork            = "network"
	ghErrTimeoutLocal       = "timeout_local"
	ghErrGitHub5xx          = "github_5xx"
	ghErrGitHubTimeout      = "github_timeout"
	ghErrSecondaryRateLimit = "secondary_rate_limit"
	ghErrRateLimit          = "rate_limit"
	ghErrAuth               = "auth"
	ghErrGHMissing          = "gh_missing"
	ghErrDecode             = "decode"
	ghErrOther              = "other"
)

var ghHTTP5xxPattern = regexp.MustCompile(`(?i)HTTP 5\d\d|bad gateway|service unavailable`)

// classifyGHError buckets a poller error by its shaped message. Order
// matters: a secondary rate limit also says "rate limit", and GitHub's
// timeout page arrives as a 5xx.
func classifyGHError(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, errCommandTimedOut) {
		return ghErrTimeoutLocal
	}
	if errors.Is(err, exec.ErrNotFound) {
		return ghErrGHMissing
	}
	msg := strings.ToLower(err.Error())
	contains := func(subs ...string) bool {
		for _, sub := range subs {
			if strings.Contains(msg, sub) {
				return true
			}
		}
		return false
	}
	switch {
	case contains("secondary rate limit", "abuse detection"):
		return ghErrSecondaryRateLimit
	case contains("rate limit", "http 429", "rate_limited"):
		return ghErrRateLimit
	case contains("couldn't respond to your request in time", "timedout", "http 504", "gateway time"):
		return ghErrGitHubTimeout
	case ghHTTP5xxPattern.MatchString(err.Error()):
		return ghErrGitHub5xx
	case contains("http 401", "bad credentials", "gh auth login", "not logged in", "resolve active github login"):
		return ghErrAuth
	case contains("dial tcp", "connection reset", "connection refused", "no such host", "tls handshake",
		"i/o timeout", "network is unreachable", "unexpected eof", "error connecting to", "broken pipe"):
		return ghErrNetwork
	case contains("decode", "returned no data", "returned no result", "invalid character"):
		return ghErrDecode
	}
	return ghErrOther
}
