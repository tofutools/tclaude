package app

import (
	"github.com/tofutools/tclaude/internal/backend/model"
	"strings"
	"time"
)

func performerTimeout(performer model.Performer) (time.Duration, error) {
	if strings.TrimSpace(performer.Timeout) == "" {
		return 0, nil
	}
	duration, err := time.ParseDuration(strings.TrimSpace(performer.Timeout))
	if err != nil || duration <= 0 {
		return 0, fail(ErrInvalid, "performer timeout must be a positive duration")
	}
	return duration, nil
}

func performerDeadline(performer *model.Performer, readyAt, limit time.Time) time.Time {
	if performer == nil {
		return limit
	}
	duration, err := performerTimeout(*performer)
	if err == nil && duration > 0 && readyAt.Add(duration).Before(limit) {
		return readyAt.Add(duration)
	}
	return limit
}
