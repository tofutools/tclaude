package app

import (
	"context"
	"errors"
	"time"

	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

// NotificationCandidate identifies durable inbox intent, never native content.
// The selected primary is rechecked transactionally immediately before dispatch.
type NotificationCandidate struct {
	RecipientID model.RecipientID
	ExecutionID model.ExecutionID
}

type MessageNotificationStore interface {
	PendingMessageNotifications(context.Context, int) ([]NotificationCandidate, error)
	ConsumeMessageNotification(context.Context, NotificationCandidate, time.Time) error
	CompleteMessageNotification(context.Context, NotificationCandidate, model.NotificationOutcome, string, time.Time) error
}

type MessageNotificationReconciler interface {
	ReconcileMessageNotifications(context.Context) error
}

// ReconcileMessageNotifications sends a fixed inbox notice, not message text or
// an authored command. Delivery is best effort. A consumed but unsettled receipt
// remains unknown across restart and is never replayed.
func (s *Service) ReconcileMessageNotifications(ctx context.Context) error {
	store, ok := s.store.(MessageNotificationStore)
	if !ok {
		return nil
	}
	candidates, err := store.PendingMessageNotifications(ctx, 64)
	if err != nil {
		return err
	}
	var failures []error
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return err
		}
		s.runtimeMu.RLock()
		runtime := s.runtimes[candidate.ExecutionID]
		s.runtimeMu.RUnlock()
		if runtime == nil {
			_, err := s.RecordMessageNotification(ctx, MessageNotificationUpdate{RecipientID: candidate.RecipientID, Outcome: model.NotificationUnavailable, Detail: "no controlled native recipient runtime"})
			if err != nil && !errors.Is(err, ErrConflict) {
				failures = append(failures, err)
			}
			continue
		}
		// No external calls intervene between consuming exact intent and dispatch.
		if err := store.ConsumeMessageNotification(ctx, candidate, s.now().UTC()); err != nil {
			if errors.Is(err, ErrConflict) || errors.Is(err, ErrUnauthorized) {
				_, settleErr := s.RecordMessageNotification(ctx, MessageNotificationUpdate{RecipientID: candidate.RecipientID, Outcome: model.NotificationUnavailable, Detail: "notification authority or recipient availability changed"})
				if settleErr != nil && !errors.Is(settleErr, ErrConflict) {
					failures = append(failures, settleErr)
				}
			} else {
				failures = append(failures, err)
			}
			continue
		}
		effectCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		result, effectErr := runtime.Interact(effectCtx, ports.Interaction{Text: "[tclaude] A new message is available in your inbox. Check your tclaude inbox."})
		cancel()
		outcome, detail := model.NotificationUnknown, "native notification outcome is uncertain"
		if effectErr == nil {
			switch result.Disposition {
			case ports.EffectAccepted:
				outcome, detail = model.NotificationDelivered, "native inbox notice accepted"
			case ports.EffectRefused, ports.EffectUnsupported:
				outcome, detail = model.NotificationUnavailable, "native inbox notice unavailable"
			}
		}
		settlement, cancelSettlement := settlementContext(ctx)
		err := store.CompleteMessageNotification(settlement, candidate, outcome, detail, s.now().UTC())
		cancelSettlement()
		if err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}
