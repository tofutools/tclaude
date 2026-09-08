package app

import (
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tofutools/tclaude/internal/backend/model"
)

func validateApprovalRetry(node model.WorkNode) error {
	stages := node.Stages
	if stages == nil {
		return nil
	}
	if stages.Review != nil && stages.Review.ApprovalRetry != nil {
		return fail(ErrInvalid, "approval retry is only valid on a plan")
	}
	for _, check := range stages.Checks {
		if check.ApprovalRetry != nil {
			return fail(ErrInvalid, "approval retry is only valid on a plan")
		}
	}
	if stages.Plan == nil || stages.Plan.ApprovalRetry == nil {
		return nil
	}
	retry := stages.Plan.ApprovalRetry
	if stages.PlanApproval == nil {
		return fail(ErrInvalid, "approval retry requires explicit plan approval")
	}
	if retry.MaxAttempts == 0 {
		return fail(ErrInvalid, "approval retry requires positive max attempts")
	}
	switch retry.OnFail {
	case "", "fresh-attempt", "feedback-same-session":
	default:
		return fail(ErrInvalid, "approval retry mode must be fresh-attempt or feedback-same-session")
	}
	if len(retry.Backoff) > 256 || !utf8.ValidString(retry.Backoff) {
		return fail(ErrInvalid, "approval retry backoff requires bounded valid text")
	}
	if value := strings.TrimSpace(retry.Backoff); value != "" {
		if duration, err := time.ParseDuration(value); err != nil || duration <= 0 {
			return fail(ErrInvalid, "approval retry backoff must be a positive duration")
		}
	}
	return nil
}
