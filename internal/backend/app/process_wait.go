package app

import (
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tofutools/tclaude/internal/backend/model"
)

func validateWaitPolicy(wait *model.WaitPolicy) error {
	if wait == nil || wait.Duration < 0 || (wait.Duration == 0 && wait.Until == "" && wait.Signal == "") {
		return fail(ErrInvalid, "wait requires a positive duration, absolute timestamp, or named signal")
	}
	if wait.Until != "" {
		if len(wait.Until) > 128 {
			return fail(ErrInvalid, "wait timestamp is too long")
		}
		if _, err := time.Parse(time.RFC3339Nano, wait.Until); err != nil {
			return fail(ErrInvalid, "wait timestamp must be RFC3339")
		}
	}
	if wait.Signal != "" && (strings.TrimSpace(wait.Signal) == "" || len(wait.Signal) > 1024 || !utf8.ValidString(wait.Signal) || strings.ContainsRune(wait.Signal, 0)) {
		return fail(ErrInvalid, "wait signal requires bounded valid text")
	}
	return nil
}
