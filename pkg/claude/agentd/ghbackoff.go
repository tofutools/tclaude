package agentd

import (
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// ghbackoff.go is one GitHub backoff shared by every gh call agentd makes
// in the background: the open-prs and tracked-prs pollers, presented-PR and
// branch-link lookups, check and detail fetches. They all spend one token's
// budget, so once GitHub says that token is rate limited, any of them asking
// again only extends the penalty — a secondary limit in particular counts
// every refused request. The first rate-limited answer pauses them all.
//
// gh does not surface Retry-After or the rate-limit reset headers on an
// error, so the pause is a fixed schedule: ghBackoffMin, doubling while
// GitHub keeps refusing right after a pause, capped at ghBackoffMax.

const (
	ghBackoffMin = 5 * time.Minute
	ghBackoffMax = 30 * time.Minute
)

// errGitHubBackoff is returned in place of running gh while paused.
var errGitHubBackoff = errors.New("GitHub calls paused after a rate limit")

var ghBackoff struct {
	sync.Mutex
	until  time.Time
	kind   string
	streak int
}

// ghArgsReachGitHub reports whether a gh invocation talks to GitHub, as
// opposed to reading local state (`gh auth token`, `gh config get`).
func ghArgsReachGitHub(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "auth", "config", "version", "--version", "help":
		return false
	}
	return true
}

// gitHubBackoffRemaining reports how long GitHub calls stay paused, and why.
func gitHubBackoffRemaining(now time.Time) (time.Duration, string) {
	ghBackoff.Lock()
	defer ghBackoff.Unlock()
	if now.Before(ghBackoff.until) {
		return ghBackoff.until.Sub(now), ghBackoff.kind
	}
	return 0, ""
}

// errGitHubLoginUnresolved is the pollers' error when the active gh login is
// unknown. While paused, the `gh api user` behind it was never sent, so the
// error wraps errGitHubBackoff and the poller waits out the pause instead.
func errGitHubLoginUnresolved() error {
	if remaining, _ := gitHubBackoffRemaining(time.Now()); remaining > 0 {
		return fmt.Errorf("resolve active GitHub login: %w", errGitHubBackoff)
	}
	return errors.New("resolve active GitHub login")
}

// gitHubBackoffGate returns errGitHubBackoff while paused, for a gh call that
// would reach GitHub.
func gitHubBackoffGate(args []string) error {
	if !ghArgsReachGitHub(args) {
		return nil
	}
	if remaining, kind := gitHubBackoffRemaining(time.Now()); remaining > 0 {
		return fmt.Errorf("%w (%s; %s left)", errGitHubBackoff, kind, remaining.Round(time.Second))
	}
	return nil
}

// noteGitHubResult records a gh call's outcome. A rate-limit answer starts or
// lengthens the pause; a success ends the doubling streak.
func noteGitHubResult(args []string, err error) {
	if !ghArgsReachGitHub(args) {
		return
	}
	noteGitHubResultAt(err, time.Now())
}

func noteGitHubResultAt(err error, now time.Time) {
	kind := classifyGHError(err)
	ghBackoff.Lock()
	defer ghBackoff.Unlock()
	switch kind {
	case "":
		// Only a success after the pause ends the doubling: one that was in
		// flight when it began says nothing about the token now.
		if !now.Before(ghBackoff.until) {
			ghBackoff.streak = 0
		}
		return
	case ghErrSecondaryRateLimit, ghErrRateLimit:
	default:
		return
	}
	if now.Before(ghBackoff.until) {
		// A call that was already in flight when the pause began.
		return
	}
	ghBackoff.streak++
	pause := ghBackoffMin
	for i := 1; i < ghBackoff.streak && pause < ghBackoffMax; i++ {
		pause *= 2
	}
	pause = min(pause, ghBackoffMax)
	ghBackoff.until = now.Add(pause)
	ghBackoff.kind = kind
	slog.Warn("github: rate limited; pausing agentd's GitHub calls",
		"kind", kind, "pause", pause, "consecutive", ghBackoff.streak, "error", err, "module", "agentd")
}
